package backend

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
)

// aggregate3 function selector: keccak256("aggregate3((address,bool,bytes)[])")[:4]
var aggregate3Selector = [4]byte{0x82, 0xad, 0x56, 0xcb}

// immutableSelectors are view functions whose return value is fixed for the
// life of the contract, so their results are cached forever, independent of
// block (see subCallKey).
var immutableSelectors = map[[4]byte]bool{
	{0x31, 0x3c, 0xe5, 0x67}: true, // decimals()
	{0x95, 0xd8, 0x9b, 0x41}: true, // symbol()
	{0x06, 0xfd, 0xde, 0x03}: true, // name()
	{0x0d, 0xfe, 0x16, 0x81}: true, // token0() — set in a pair's constructor
	{0xd2, 0x12, 0x20, 0xa7}: true, // token1() — set in a pair's constructor
}

func isImmutableSelector(callData []byte) bool {
	if len(callData) < 4 {
		return false
	}
	return immutableSelectors[[4]byte(callData[:4])]
}

// decodeImmutableCall returns the target and calldata of a standalone eth_call to
// an immutable view function (see immutableSelectors), or ok=false otherwise. It
// rejects calls carrying state overrides or execution-context modifiers
// (from/value/gas): their result is not a pure function of the contract, so it
// must never share the block-agnostic forever-cache. This is the standalone
// counterpart to decodeMulticall's per-sub-call immutable handling — an immutable
// read is immutable whether wrapped in aggregate3 or sent directly, and both
// share subCallKey entries.
func decodeImmutableCall(method string, params json.RawMessage) ([20]byte, []byte, bool) {
	var zero [20]byte
	if method != "eth_call" {
		return zero, nil, false
	}
	parsed, ok := parseEthCallParams(params)
	if !ok || parsed.Params.To == "" || parsed.Params.Data == "" {
		return zero, nil, false
	}
	if parsed.HasStateOverrides || parsed.Params.From != "" || parsed.Params.Value != "" || parsed.Params.Gas != "" || parsed.Params.GasPrice != "" {
		return zero, nil, false
	}
	callData := hexToBytes(parsed.Params.Data)
	if !isImmutableSelector(callData) {
		return zero, nil, false
	}
	return hexToAddress(parsed.Params.To), callData, true
}

// decodeMulticall returns the inner Call3 tuples of an eth_call that wraps a
// Multicall3 aggregate3 batch, or ok=false for any other request.
func decodeMulticall(method string, params json.RawMessage) ([]call3, bool) {
	if method != "eth_call" {
		return nil, false
	}
	parsed, ok := parseEthCallParams(params)
	if !ok || parsed.Params.Data == "" {
		return nil, false
	}
	return decodeAggregate3Calls(hexToBytes(parsed.Params.Data))
}

// decodeAggregate3Calls is the inverse of encodeAggregate3.
func decodeAggregate3Calls(data []byte) ([]call3, bool) {
	if len(data) < 4 || [4]byte{data[0], data[1], data[2], data[3]} != aggregate3Selector {
		return nil, false
	}
	args := data[4:]
	if len(args) < 32 {
		return nil, false
	}

	arrayOff := readUint256(args[0:32])
	if int(arrayOff) > len(args) || len(args)-int(arrayOff) < 32 {
		return nil, false
	}
	arr := args[arrayOff:]

	n := readUint256(arr[0:32])
	if n == 0 || n > 10000 {
		return nil, false
	}
	if len(arr) < 32+int(n)*32 {
		return nil, false
	}

	calls := make([]call3, n)
	for i := uint64(0); i < n; i++ {
		tupleOff := readUint256(arr[32+int(i)*32 : 64+int(i)*32])
		abs := 32 + int(tupleOff)
		if abs < 0 || abs+96 > len(arr) {
			return nil, false
		}
		tuple := arr[abs:]

		copy(calls[i].Target[:], tuple[12:32])
		calls[i].AllowFailure = tuple[63] != 0

		cdOff := readUint256(tuple[64:96])
		if int(cdOff)+32 > len(tuple) {
			return nil, false
		}
		cdLen := readUint256(tuple[cdOff : cdOff+32])
		cdStart := int(cdOff) + 32
		if cdStart+int(cdLen) > len(tuple) {
			return nil, false
		}
		calls[i].CallData = make([]byte, cdLen)
		copy(calls[i].CallData, tuple[cdStart:cdStart+int(cdLen)])
	}
	return calls, true
}

// multicallIDCounter generates unique synthetic request IDs for multicall requests.
var multicallIDCounter atomic.Uint64

type call3 struct {
	Target       [20]byte
	AllowFailure bool
	CallData     []byte
}

type multicallResult struct {
	Success    bool
	ReturnData []byte
}

// ethCallParams represents the parsed parameters of an eth_call request.
type ethCallParams struct {
	To       string `json:"to"`
	Data     string `json:"data"`
	From     string `json:"from,omitempty"`
	Gas      string `json:"gas,omitempty"`
	GasPrice string `json:"gasPrice,omitempty"`
	Value    string `json:"value,omitempty"`
}

type multicallEntry struct {
	OriginalID     json.RawMessage
	OriginalParams json.RawMessage
	Index          int
}

type multicallGroup struct {
	SyntheticID json.RawMessage
	BlockTag    string
	Entries     []multicallEntry
}

// MulticallMapping tracks the relationship between synthetic multicall requests
// and the original eth_call requests they replace.
type MulticallMapping struct {
	Groups      []multicallGroup
	Passthrough []BatchRequest
}

// parsedEthCall holds the parsed components of an eth_call request.
type parsedEthCall struct {
	Params            ethCallParams
	BlockTag          string
	HasStateOverrides bool
}

// parseEthCallParams extracts the call object, block tag, and state override
// presence from eth_call params.
// eth_call params: [{to, data, ...}, blockTag, stateOverrides?]
func parseEthCallParams(params json.RawMessage) (parsedEthCall, bool) {
	var raw []json.RawMessage
	if err := json.Unmarshal(params, &raw); err != nil || len(raw) == 0 {
		return parsedEthCall{}, false
	}

	var callObj ethCallParams
	if err := json.Unmarshal(raw[0], &callObj); err != nil {
		return parsedEthCall{}, false
	}

	blockTag := "latest"
	if len(raw) > 1 {
		var tag string
		if err := json.Unmarshal(raw[1], &tag); err == nil && tag != "" {
			blockTag = tag
		}
	}

	return parsedEthCall{
		Params:            callObj,
		BlockTag:          blockTag,
		HasStateOverrides: len(raw) > 2,
	}, true
}

type eligibleCall struct {
	requestIdx int
	params     ethCallParams
	blockTag   string
}

// aggregateEthCalls transforms a batch of requests by combining eligible eth_call
// requests into Multicall3 aggregate3 calls. Returns the optimized request list
// and a mapping to expand responses back to original callers.
func aggregateEthCalls(requests []BatchRequest, cfg MulticallConfig) ([]BatchRequest, *MulticallMapping) {
	mapping := &MulticallMapping{}

	var eligible []eligibleCall

	for i, req := range requests {
		if req.Method != "eth_call" {
			mapping.Passthrough = append(mapping.Passthrough, req)
			continue
		}

		parsed, ok := parseEthCallParams(req.Params)
		if !ok || parsed.Params.To == "" || parsed.Params.Data == "" {
			mapping.Passthrough = append(mapping.Passthrough, req)
			continue
		}

		// Calls with state overrides depend on custom code/storage deployed at
		// call time. Wrapping them in Multicall3 strips those overrides and
		// changes the execution environment. Pass through as-is.
		if parsed.HasStateOverrides {
			mapping.Passthrough = append(mapping.Passthrough, req)
			continue
		}

		// Calls with execution context modifiers behave differently through
		// Multicall3 (msg.sender, gas, value all change). Pass through as-is.
		if parsed.Params.From != "" || parsed.Params.Value != "" || parsed.Params.Gas != "" || parsed.Params.GasPrice != "" {
			mapping.Passthrough = append(mapping.Passthrough, req)
			continue
		}

		eligible = append(eligible, eligibleCall{
			requestIdx: i,
			params:     parsed.Params,
			blockTag:   parsed.BlockTag,
		})
	}

	if len(eligible) < 2 {
		return requests, nil
	}

	// Group eligible calls by block tag, preserving insertion order.
	tagGroups := make(map[string][]eligibleCall)
	var tagOrder []string
	for _, e := range eligible {
		if _, exists := tagGroups[e.blockTag]; !exists {
			tagOrder = append(tagOrder, e.blockTag)
		}
		tagGroups[e.blockTag] = append(tagGroups[e.blockTag], e)
	}

	out := make([]BatchRequest, len(mapping.Passthrough))
	copy(out, mapping.Passthrough)

	for _, tag := range tagOrder {
		calls := tagGroups[tag]

		for chunkStart := 0; chunkStart < len(calls); chunkStart += cfg.MaxCalls {
			chunkEnd := chunkStart + cfg.MaxCalls
			if chunkEnd > len(calls) {
				chunkEnd = len(calls)
			}
			chunk := calls[chunkStart:chunkEnd]

			if len(chunk) == 1 {
				mapping.Passthrough = append(mapping.Passthrough, requests[chunk[0].requestIdx])
				out = append(out, requests[chunk[0].requestIdx])
				continue
			}

			syntheticID := json.RawMessage(
				fmt.Sprintf(`"__multicall_%d"`, multicallIDCounter.Add(1)),
			)

			group := multicallGroup{
				SyntheticID: syntheticID,
				BlockTag:    tag,
				Entries:     make([]multicallEntry, 0, len(chunk)),
			}

			call3s := make([]call3, len(chunk))
			for j, c := range chunk {
				call3s[j] = call3{
					Target:       hexToAddress(c.params.To),
					AllowFailure: true,
					CallData:     hexToBytes(c.params.Data),
				}
				group.Entries = append(group.Entries, multicallEntry{
					OriginalID:     requests[c.requestIdx].ID,
					OriginalParams: requests[c.requestIdx].Params,
					Index:          j,
				})
			}

			encoded := encodeAggregate3(call3s)
			callObj := map[string]string{
				"to":   cfg.Address,
				"data": "0x" + hex.EncodeToString(encoded),
			}
			callObjJSON, _ := json.Marshal(callObj)

			tagJSON, _ := json.Marshal(tag)
			paramsJSON, _ := json.Marshal([]json.RawMessage{callObjJSON, json.RawMessage(tagJSON)})

			out = append(out, BatchRequest{
				JSONRPC: "2.0",
				ID:      syntheticID,
				Method:  "eth_call",
				Params:  paramsJSON,
			})

			mapping.Groups = append(mapping.Groups, group)
		}
	}

	return out, mapping
}

// buildMulticallBody constructs a JSON-RPC eth_call request wrapping an aggregate3
// calldata blob at the given multicall3 address and block tag — used to forward
// only the cache-miss sub-calls of a partially-served multicall upstream.
func buildMulticallBody(id json.RawMessage, to, blockTag string, calldata []byte) []byte {
	callObj, _ := json.Marshal(map[string]string{
		"to":   to,
		"data": "0x" + hex.EncodeToString(calldata),
	})
	tagJSON, _ := json.Marshal(blockTag)
	params, _ := json.Marshal([]json.RawMessage{callObj, json.RawMessage(tagJSON)})
	body, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{"2.0", id, "eth_call", params})
	return body
}

// expandMulticallResponses decodes multicall aggregate3 responses and fans them
// back out into individual responses with the original request IDs.
func expandMulticallResponses(responses []BatchResponse, mapping *MulticallMapping) ([]BatchResponse, error) {
	respByID := make(map[string]BatchResponse, len(responses))
	for _, resp := range responses {
		respByID[string(resp.ID)] = resp
	}

	expanded := make([]BatchResponse, 0, len(mapping.Passthrough)+countEntries(mapping))

	for _, pt := range mapping.Passthrough {
		if resp, ok := respByID[string(pt.ID)]; ok {
			expanded = append(expanded, resp)
		}
	}

	for _, group := range mapping.Groups {
		resp, ok := respByID[string(group.SyntheticID)]
		if !ok {
			for _, entry := range group.Entries {
				expanded = append(expanded, BatchResponse{
					JSONRPC: "2.0",
					ID:      entry.OriginalID,
					Error: map[string]any{
						"code":    -32603,
						"message": "multicall response missing",
					},
				})
			}
			continue
		}

		if resp.Error != nil {
			for _, entry := range group.Entries {
				expanded = append(expanded, BatchResponse{
					JSONRPC: "2.0",
					ID:      entry.OriginalID,
					Error:   resp.Error,
				})
			}
			continue
		}

		resultHex, ok := resp.Result.(string)
		if !ok {
			for _, entry := range group.Entries {
				expanded = append(expanded, BatchResponse{
					JSONRPC: "2.0",
					ID:      entry.OriginalID,
					Error: map[string]any{
						"code":    -32603,
						"message": "multicall result not a hex string",
					},
				})
			}
			continue
		}

		results, err := decodeAggregate3Result(resultHex)
		if err != nil {
			for _, entry := range group.Entries {
				expanded = append(expanded, BatchResponse{
					JSONRPC: "2.0",
					ID:      entry.OriginalID,
					Error: map[string]any{
						"code":    -32603,
						"message": "multicall decode error: " + err.Error(),
					},
				})
			}
			continue
		}

		if len(results) != len(group.Entries) {
			for _, entry := range group.Entries {
				expanded = append(expanded, BatchResponse{
					JSONRPC: "2.0",
					ID:      entry.OriginalID,
					Error: map[string]any{
						"code":    -32603,
						"message": fmt.Sprintf("multicall result count mismatch: got %d, expected %d", len(results), len(group.Entries)),
					},
				})
			}
			continue
		}

		for _, entry := range group.Entries {
			r := results[entry.Index]
			if r.Success {
				expanded = append(expanded, BatchResponse{
					JSONRPC: "2.0",
					ID:      entry.OriginalID,
					Result:  "0x" + hex.EncodeToString(r.ReturnData),
				})
			} else {
				expanded = append(expanded, BatchResponse{
					JSONRPC: "2.0",
					ID:      entry.OriginalID,
					Error: map[string]any{
						"code":    3,
						"message": "execution reverted",
						"data":    "0x" + hex.EncodeToString(r.ReturnData),
					},
				})
			}
		}
	}

	return expanded, nil
}

func countEntries(m *MulticallMapping) int {
	n := 0
	for _, g := range m.Groups {
		n += len(g.Entries)
	}
	return n
}

// encodeAggregate3 ABI-encodes an aggregate3((address,bool,bytes)[]) call.
//
// Layout:
//
//	[0:4]   function selector 0x82ad56cb
//	[4:36]  offset to array data (always 0x20)
//	[36:68] array length N
//	[68:..] N offset pointers to tuple data (relative to array data start)
//	[..]    tuple data: address(32) + bool(32) + bytes_offset(32=0x60) + bytes_len(32) + bytes_padded
func encodeAggregate3(calls []call3) []byte {
	n := len(calls)

	// Calculate total size.
	offsetTableSize := n * 32
	tupleOffsets := make([]int, n)
	runningOffset := offsetTableSize

	for i, c := range calls {
		tupleOffsets[i] = runningOffset
		// 32 (addr) + 32 (bool) + 32 (bytes offset) + 32 (bytes len) + ceil32(calldata)
		runningOffset += 128 + ceil32(len(c.CallData))
	}

	totalSize := 4 + 32 + 32 + runningOffset
	buf := make([]byte, totalSize)
	pos := 0

	copy(buf[pos:], aggregate3Selector[:])
	pos += 4

	writeUint256(buf[pos:], 32)
	pos += 32

	writeUint256(buf[pos:], uint64(n))
	pos += 32

	for i := 0; i < n; i++ {
		writeUint256(buf[pos:], uint64(tupleOffsets[i]))
		pos += 32
	}

	for _, c := range calls {
		// address: right-aligned in 32-byte slot
		copy(buf[pos+12:pos+32], c.Target[:])
		pos += 32

		// bool allowFailure
		if c.AllowFailure {
			buf[pos+31] = 1
		}
		pos += 32

		// offset to callData within this tuple (3 head slots * 32 = 96 = 0x60)
		writeUint256(buf[pos:], 96)
		pos += 32

		// callData length
		writeUint256(buf[pos:], uint64(len(c.CallData)))
		pos += 32

		// callData bytes, right-padded to 32-byte boundary
		copy(buf[pos:], c.CallData)
		pos += ceil32(len(c.CallData))
	}

	return buf
}

// encodeAggregate3Result is the inverse of decodeAggregate3Result; sabre uses
// it to synthesize a multicall response from individually-cached sub-calls.
func encodeAggregate3Result(results []multicallResult) []byte {
	n := len(results)

	elemOffsets := make([]int, n)
	running := n * 32 // element-offset table precedes the tuple bodies
	for i, r := range results {
		elemOffsets[i] = running
		// success(32) + bytesOffset(32) + bytesLen(32) + padded returnData
		running += 96 + ceil32(len(r.ReturnData))
	}

	total := 32 + 32 + running // array offset + length + body
	buf := make([]byte, total)
	pos := 0

	writeUint256(buf[pos:], 32) // offset to array data
	pos += 32

	writeUint256(buf[pos:], uint64(n)) // array length
	pos += 32

	for i := 0; i < n; i++ {
		writeUint256(buf[pos:], uint64(elemOffsets[i]))
		pos += 32
	}

	for _, r := range results {
		if r.Success {
			buf[pos+31] = 1
		}
		pos += 32

		writeUint256(buf[pos:], 64) // offset to returnData within the tuple
		pos += 32

		writeUint256(buf[pos:], uint64(len(r.ReturnData)))
		pos += 32

		copy(buf[pos:], r.ReturnData)
		pos += ceil32(len(r.ReturnData))
	}

	return buf
}

// decodeAggregate3Result ABI-decodes the return value of aggregate3: Result[]
// where Result = (bool success, bytes returnData).
func decodeAggregate3Result(resultHex string) ([]multicallResult, error) {
	resultHex = strings.TrimPrefix(resultHex, "0x")
	data, err := hex.DecodeString(resultHex)
	if err != nil {
		return nil, fmt.Errorf("invalid hex: %w", err)
	}

	if len(data) < 64 {
		return nil, fmt.Errorf("result too short: %d bytes", len(data))
	}

	arrayOffset := readUint256(data[0:32])
	if int(arrayOffset) >= len(data) {
		return nil, fmt.Errorf("array offset out of bounds: %d >= %d", arrayOffset, len(data))
	}

	arrayData := data[arrayOffset:]
	if len(arrayData) < 32 {
		return nil, fmt.Errorf("array data too short for length word")
	}

	n := readUint256(arrayData[0:32])
	if n > 10000 {
		return nil, fmt.Errorf("unreasonable array length: %d", n)
	}

	offsetTableEnd := 32 + int(n)*32
	if len(arrayData) < offsetTableEnd {
		return nil, fmt.Errorf("offset table exceeds data: need %d, have %d", offsetTableEnd, len(arrayData))
	}

	results := make([]multicallResult, n)

	// Offsets in the array are relative to the start of the heads area, which
	// begins right after the length word (arrayData[32:]). We add 32 to convert
	// the offset into an index within arrayData.
	headsStart := 32

	for i := uint64(0); i < n; i++ {
		tupleOffset := readUint256(arrayData[32+int(i)*32 : 64+int(i)*32])
		absOffset := headsStart + int(tupleOffset)
		if absOffset >= len(arrayData) {
			return nil, fmt.Errorf("tuple %d offset out of bounds: %d >= %d", i, absOffset, len(arrayData))
		}

		tupleData := arrayData[absOffset:]
		if len(tupleData) < 64 {
			return nil, fmt.Errorf("tuple %d too short: %d bytes", i, len(tupleData))
		}

		results[i].Success = tupleData[31] != 0

		returnDataOffset := readUint256(tupleData[32:64])
		if int(returnDataOffset)+32 > len(tupleData) {
			return nil, fmt.Errorf("tuple %d returnData offset out of bounds", i)
		}

		returnDataLen := readUint256(tupleData[returnDataOffset : returnDataOffset+32])
		start := returnDataOffset + 32
		end := start + returnDataLen
		if int(end) > len(tupleData) {
			return nil, fmt.Errorf("tuple %d returnData exceeds bounds: end %d > len %d", i, end, len(tupleData))
		}

		results[i].ReturnData = make([]byte, returnDataLen)
		copy(results[i].ReturnData, tupleData[start:end])
	}

	return results, nil
}

func writeUint256(dst []byte, v uint64) {
	binary.BigEndian.PutUint64(dst[24:32], v)
}

func readUint256(b []byte) uint64 {
	return binary.BigEndian.Uint64(b[24:32])
}

func ceil32(n int) int {
	return (n + 31) &^ 31
}

func hexToAddress(s string) [20]byte {
	s = strings.TrimPrefix(s, "0x")
	var addr [20]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return addr
	}
	if len(b) > 20 {
		b = b[len(b)-20:]
	}
	copy(addr[20-len(b):], b)
	return addr
}

func hexToBytes(s string) []byte {
	s = strings.TrimPrefix(s, "0x")
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}
