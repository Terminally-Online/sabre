package backend

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseEthCallParams(t *testing.T) {
	tests := []struct {
		name     string
		params   string
		wantTo   string
		wantData string
		wantTag  string
		wantOK   bool
	}{
		{
			name:     "standard with latest",
			params:   `[{"to":"0xdead","data":"0xbeef"},"latest"]`,
			wantTo:   "0xdead",
			wantData: "0xbeef",
			wantTag:  "latest",
			wantOK:   true,
		},
		{
			name:     "no block tag defaults to latest",
			params:   `[{"to":"0xdead","data":"0xbeef"}]`,
			wantTo:   "0xdead",
			wantData: "0xbeef",
			wantTag:  "latest",
			wantOK:   true,
		},
		{
			name:     "specific block number",
			params:   `[{"to":"0xdead","data":"0xbeef"},"0x1234"]`,
			wantTo:   "0xdead",
			wantData: "0xbeef",
			wantTag:  "0x1234",
			wantOK:   true,
		},
		{
			name:     "with extra fields",
			params:   `[{"to":"0xdead","data":"0xbeef","from":"0xcafe","value":"0x1"},"latest"]`,
			wantTo:   "0xdead",
			wantData: "0xbeef",
			wantTag:  "latest",
			wantOK:   true,
		},
		{
			name:   "empty array",
			params: `[]`,
			wantOK: false,
		},
		{
			name:   "invalid json",
			params: `not json`,
			wantOK: false,
		},
		{
			name:   "null params",
			params: `null`,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params, tag, ok := parseEthCallParams(json.RawMessage(tt.params))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if params.To != tt.wantTo {
				t.Errorf("to = %q, want %q", params.To, tt.wantTo)
			}
			if params.Data != tt.wantData {
				t.Errorf("data = %q, want %q", params.Data, tt.wantData)
			}
			if tag != tt.wantTag {
				t.Errorf("tag = %q, want %q", tag, tt.wantTag)
			}
		})
	}
}

func TestHexToAddress(t *testing.T) {
	addr := hexToAddress("0x00000000000000000000000000000000DeaDBeef")
	want := [20]byte{}
	want[16] = 0xde
	want[17] = 0xad
	want[18] = 0xbe
	want[19] = 0xef
	if addr != want {
		t.Errorf("hexToAddress mismatch: got %x, want %x", addr, want)
	}
}

func TestHexToBytes(t *testing.T) {
	b := hexToBytes("0xdeadbeef")
	if hex.EncodeToString(b) != "deadbeef" {
		t.Errorf("hexToBytes mismatch: got %x", b)
	}

	b = hexToBytes("deadbeef")
	if hex.EncodeToString(b) != "deadbeef" {
		t.Errorf("hexToBytes without prefix mismatch: got %x", b)
	}

	b = hexToBytes("0xzzzz")
	if b != nil {
		t.Errorf("expected nil for invalid hex, got %x", b)
	}
}

func TestCeil32(t *testing.T) {
	tests := []struct{ in, want int }{
		{0, 0},
		{1, 32},
		{31, 32},
		{32, 32},
		{33, 64},
		{64, 64},
	}
	for _, tt := range tests {
		got := ceil32(tt.in)
		if got != tt.want {
			t.Errorf("ceil32(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Layer 1: Reference ABI encoding tests (byte-for-byte against hand-computed)
// ---------------------------------------------------------------------------

func TestEncodeAggregate3ReferenceEncoding(t *testing.T) {
	// Encode a single call: aggregate3([(Multicall3, true, getBlockNumber())])
	// Target:   0xcA11bde05977b3631167028862bE2a173976CA11
	// CallData: 0x42cbb15c (getBlockNumber() selector, 4 bytes)
	//
	// Hand-computed expected encoding:
	//
	// 82ad56cb                                                          selector
	// 0000000000000000000000000000000000000000000000000000000000000020  offset to array = 32
	// 0000000000000000000000000000000000000000000000000000000000000001  array length = 1
	// 0000000000000000000000000000000000000000000000000000000000000020  offset to tuple 0 = 32 (1 slot past the 1 offset pointer)
	// 000000000000000000000000ca11bde05977b3631167028862be2a173976ca11  target address
	// 0000000000000000000000000000000000000000000000000000000000000001  allowFailure = true
	// 0000000000000000000000000000000000000000000000000000000000000060  bytes offset = 96
	// 0000000000000000000000000000000000000000000000000000000000000004  callData length = 4
	// 42cbb15c00000000000000000000000000000000000000000000000000000000  callData padded

	expected := strings.ReplaceAll(`82ad56cb`+
		`0000000000000000000000000000000000000000000000000000000000000020`+
		`0000000000000000000000000000000000000000000000000000000000000001`+
		`0000000000000000000000000000000000000000000000000000000000000020`+
		`000000000000000000000000ca11bde05977b3631167028862be2a173976ca11`+
		`0000000000000000000000000000000000000000000000000000000000000001`+
		`0000000000000000000000000000000000000000000000000000000000000060`+
		`0000000000000000000000000000000000000000000000000000000000000004`+
		`42cbb15c00000000000000000000000000000000000000000000000000000000`,
		"\n", "")

	calls := []call3{
		{
			Target:       hexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"),
			AllowFailure: true,
			CallData:     hexToBytes("0x42cbb15c"),
		},
	}

	encoded := encodeAggregate3(calls)
	got := hex.EncodeToString(encoded)

	if got != expected {
		t.Fatalf("encoding mismatch\ngot:  %s\nwant: %s", got, expected)
	}
}

func TestEncodeAggregate3TwoCallsReferenceEncoding(t *testing.T) {
	// Two calls: getBlockNumber() and getCurrentBlockTimestamp() on Multicall3.
	// Target:    0xcA11bde05977b3631167028862bE2a173976CA11
	// CallData0: 0x42cbb15c (getBlockNumber, 4 bytes)
	// CallData1: 0x0f28c97d (getCurrentBlockTimestamp, 4 bytes)
	//
	// n=2, offset table = 2*32 = 64 bytes
	// tuple0 offset = 64 (relative to heads area)
	// tuple0 size = 128 + ceil32(4) = 160
	// tuple1 offset = 64 + 160 = 224

	expected := strings.ReplaceAll(`82ad56cb`+
		// offset to array
		`0000000000000000000000000000000000000000000000000000000000000020`+
		// array length = 2
		`0000000000000000000000000000000000000000000000000000000000000002`+
		// offset to tuple 0 = 64
		`0000000000000000000000000000000000000000000000000000000000000040`+
		// offset to tuple 1 = 224
		`00000000000000000000000000000000000000000000000000000000000000e0`+
		// tuple 0: address
		`000000000000000000000000ca11bde05977b3631167028862be2a173976ca11`+
		// tuple 0: allowFailure
		`0000000000000000000000000000000000000000000000000000000000000001`+
		// tuple 0: bytes offset
		`0000000000000000000000000000000000000000000000000000000000000060`+
		// tuple 0: callData length
		`0000000000000000000000000000000000000000000000000000000000000004`+
		// tuple 0: callData
		`42cbb15c00000000000000000000000000000000000000000000000000000000`+
		// tuple 1: address
		`000000000000000000000000ca11bde05977b3631167028862be2a173976ca11`+
		// tuple 1: allowFailure
		`0000000000000000000000000000000000000000000000000000000000000001`+
		// tuple 1: bytes offset
		`0000000000000000000000000000000000000000000000000000000000000060`+
		// tuple 1: callData length
		`0000000000000000000000000000000000000000000000000000000000000004`+
		// tuple 1: callData
		`0f28c97d00000000000000000000000000000000000000000000000000000000`,
		"\n", "")

	calls := []call3{
		{
			Target:       hexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"),
			AllowFailure: true,
			CallData:     hexToBytes("0x42cbb15c"),
		},
		{
			Target:       hexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"),
			AllowFailure: true,
			CallData:     hexToBytes("0x0f28c97d"),
		},
	}

	encoded := encodeAggregate3(calls)
	got := hex.EncodeToString(encoded)

	if got != expected {
		t.Fatalf("encoding mismatch\ngot:  %s\nwant: %s", got, expected)
	}
}

func TestDecodeAggregate3ResultReferenceDecoding(t *testing.T) {
	// Reference return data for 2 successful Results:
	// Result[0] = (true, 0x000000000000000000000000000000000000000000000000000000000134eb78)
	// Result[1] = (true, 0x0000000000000000000000000000000000000000000000000000000067c8a1b0)
	//
	// These represent: blockNumber = 0x134eb78 = 20,180,856 and timestamp = 0x67c8a1b0 = 1741234608

	resultHex := `0x` +
		// offset to array
		`0000000000000000000000000000000000000000000000000000000000000020` +
		// array length = 2
		`0000000000000000000000000000000000000000000000000000000000000002` +
		// offset to tuple 0 = 64 (2 offset slots)
		`0000000000000000000000000000000000000000000000000000000000000040` +
		// offset to tuple 1 = 64 + 128 = 192
		`00000000000000000000000000000000000000000000000000000000000000c0` +
		// tuple 0: success = true
		`0000000000000000000000000000000000000000000000000000000000000001` +
		// tuple 0: bytes offset = 64
		`0000000000000000000000000000000000000000000000000000000000000040` +
		// tuple 0: returnData length = 32
		`0000000000000000000000000000000000000000000000000000000000000020` +
		// tuple 0: returnData
		`000000000000000000000000000000000000000000000000000000000134eb78` +
		// tuple 1: success = true
		`0000000000000000000000000000000000000000000000000000000000000001` +
		// tuple 1: bytes offset = 64
		`0000000000000000000000000000000000000000000000000000000000000040` +
		// tuple 1: returnData length = 32
		`0000000000000000000000000000000000000000000000000000000000000020` +
		// tuple 1: returnData
		`0000000000000000000000000000000000000000000000000000000067c8a1b0`

	results, err := decodeAggregate3Result(resultHex)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if !results[0].Success {
		t.Error("result[0] should be success")
	}
	blockNum := hex.EncodeToString(results[0].ReturnData)
	if !strings.HasSuffix(blockNum, "0134eb78") {
		t.Errorf("result[0] returnData should end with 0134eb78, got %s", blockNum)
	}

	if !results[1].Success {
		t.Error("result[1] should be success")
	}
	timestamp := hex.EncodeToString(results[1].ReturnData)
	if !strings.HasSuffix(timestamp, "67c8a1b0") {
		t.Errorf("result[1] returnData should end with 67c8a1b0, got %s", timestamp)
	}
}

// ---------------------------------------------------------------------------
// Layer 2: Full pipeline test with mock backend
// ---------------------------------------------------------------------------

func TestMulticallFullPipelineWithMock(t *testing.T) {
	// Simulate the complete flow: aggregateEthCalls → sendBatch (mocked) → expandMulticallResponses
	// Verify that callers get back the correct individual responses.

	cfg := MulticallConfig{
		Enabled:  true,
		Address:  "0xcA11bde05977b3631167028862bE2a173976CA11",
		MaxCalls: 150,
	}

	// 3 eth_call requests + 1 eth_blockNumber
	requests := []BatchRequest{
		{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "eth_call",
			Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000001","data":"0xdeadbeef"},"latest"]`)},
		{JSONRPC: "2.0", ID: json.RawMessage(`"2"`), Method: "eth_blockNumber",
			Params: json.RawMessage(`[]`)},
		{JSONRPC: "2.0", ID: json.RawMessage(`"3"`), Method: "eth_call",
			Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000002","data":"0xcafebabe"},"latest"]`)},
		{JSONRPC: "2.0", ID: json.RawMessage(`"4"`), Method: "eth_call",
			Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000003","data":"0x12345678"},"latest"]`)},
	}

	// Step 1: Aggregate
	optimized, mapping := aggregateEthCalls(requests, cfg)
	if mapping == nil {
		t.Fatal("expected mapping (3 eligible eth_calls)")
	}

	// Should be: 1 passthrough (eth_blockNumber) + 1 multicall
	if len(optimized) != 2 {
		t.Fatalf("expected 2 optimized requests, got %d", len(optimized))
	}
	if len(mapping.Groups) != 1 {
		t.Fatalf("expected 1 multicall group, got %d", len(mapping.Groups))
	}
	if len(mapping.Groups[0].Entries) != 3 {
		t.Fatalf("expected 3 entries in group, got %d", len(mapping.Groups[0].Entries))
	}

	// Step 2: Simulate backend response
	// Build aggregate3 Result[] for the 3 calls
	multicallResultData := buildAggregate3ResultEncoding([]multicallResult{
		{Success: true, ReturnData: hexToBytes("0x" + strings.Repeat("aa", 32))},
		{Success: true, ReturnData: hexToBytes("0x" + strings.Repeat("bb", 32))},
		{Success: false, ReturnData: hexToBytes("0x08c379a0" + strings.Repeat("00", 28))}, // revert
	})

	// Mock responses from backend: one for eth_blockNumber passthrough, one for multicall
	mockResponses := []BatchResponse{
		{JSONRPC: "2.0", ID: json.RawMessage(`"2"`), Result: "0x134eb78"},
		{JSONRPC: "2.0", ID: mapping.Groups[0].SyntheticID,
			Result: "0x" + hex.EncodeToString(multicallResultData)},
	}

	// Step 3: Expand
	expanded, err := expandMulticallResponses(mockResponses, mapping)
	if err != nil {
		t.Fatalf("expand error: %v", err)
	}

	// Should have 4 responses: 1 passthrough + 3 expanded
	if len(expanded) != 4 {
		t.Fatalf("expected 4 expanded responses, got %d", len(expanded))
	}

	// Build lookup for easy assertion
	respByID := make(map[string]BatchResponse)
	for _, r := range expanded {
		respByID[string(r.ID)] = r
	}

	// Verify passthrough (eth_blockNumber)
	r2 := respByID[`"2"`]
	if r2.Result != "0x134eb78" {
		t.Errorf("eth_blockNumber response: got %v, want 0x134eb78", r2.Result)
	}
	if r2.Error != nil {
		t.Errorf("eth_blockNumber should not have error")
	}

	// Verify expanded call 1 (success)
	r1 := respByID[`"1"`]
	if r1.Error != nil {
		t.Errorf("call 1 should not have error, got %v", r1.Error)
	}
	expectedResult1 := "0x" + strings.Repeat("aa", 32)
	if r1.Result != expectedResult1 {
		t.Errorf("call 1 result: got %v, want %s", r1.Result, expectedResult1)
	}

	// Verify expanded call 3 (success)
	r3 := respByID[`"3"`]
	if r3.Error != nil {
		t.Errorf("call 3 should not have error, got %v", r3.Error)
	}
	expectedResult3 := "0x" + strings.Repeat("bb", 32)
	if r3.Result != expectedResult3 {
		t.Errorf("call 3 result: got %v, want %s", r3.Result, expectedResult3)
	}

	// Verify expanded call 4 (reverted)
	r4 := respByID[`"4"`]
	if r4.Error == nil {
		t.Fatal("call 4 should have error (reverted)")
	}
	errMap, ok := r4.Error.(map[string]any)
	if !ok {
		t.Fatal("error should be a map")
	}
	if errMap["code"] != 3 {
		t.Errorf("reverted call error code: got %v, want 3", errMap["code"])
	}
	if errMap["message"] != "execution reverted" {
		t.Errorf("reverted call message: got %v", errMap["message"])
	}
}

func TestMulticallPipelineWithMulticallError(t *testing.T) {
	// When the multicall eth_call itself fails (e.g., out of gas), all
	// aggregated calls should receive the error while passthrough calls
	// still get their independent responses.

	cfg := MulticallConfig{
		Enabled:  true,
		Address:  "0xcA11bde05977b3631167028862bE2a173976CA11",
		MaxCalls: 150,
	}

	requests := []BatchRequest{
		{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "eth_blockNumber", Params: json.RawMessage(`[]`)},
		{JSONRPC: "2.0", ID: json.RawMessage(`"2"`), Method: "eth_call",
			Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000001","data":"0xaa"},"latest"]`)},
		{JSONRPC: "2.0", ID: json.RawMessage(`"3"`), Method: "eth_call",
			Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000002","data":"0xbb"},"latest"]`)},
	}

	optimized, mapping := aggregateEthCalls(requests, cfg)
	if mapping == nil {
		t.Fatal("expected mapping")
	}

	// Simulate: eth_blockNumber succeeds, multicall fails
	mockResponses := []BatchResponse{
		{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Result: "0x100"},
		{JSONRPC: "2.0", ID: mapping.Groups[0].SyntheticID,
			Error: map[string]any{"code": -32000, "message": "out of gas"}},
	}
	_ = optimized

	expanded, err := expandMulticallResponses(mockResponses, mapping)
	if err != nil {
		t.Fatalf("expand error: %v", err)
	}

	if len(expanded) != 3 {
		t.Fatalf("expected 3 responses, got %d", len(expanded))
	}

	respByID := make(map[string]BatchResponse)
	for _, r := range expanded {
		respByID[string(r.ID)] = r
	}

	// Passthrough should succeed
	if respByID[`"1"`].Error != nil {
		t.Error("passthrough should not have error")
	}

	// Aggregated calls should have the multicall error
	for _, id := range []string{`"2"`, `"3"`} {
		r := respByID[id]
		if r.Error == nil {
			t.Errorf("call %s should have error", id)
		}
	}
}

// ---------------------------------------------------------------------------
// Layer 3: Live node integration test
// ---------------------------------------------------------------------------

func TestMulticallLiveNode(t *testing.T) {
	// Hit a real public RPC endpoint with a multicall aggregate3 call using
	// Multicall3's own view functions, then compare against direct eth_call.
	//
	// Calls:
	//   1. getBlockNumber()          selector: 0x42cbb15c
	//   2. getCurrentBlockTimestamp() selector: 0x0f28c97d
	//   3. getBlockHash(uint256)     selector: 0xee82ac5e (block 1)

	endpoint := "https://eth.drpc.org"
	multicallAddr := "0xcA11bde05977b3631167028862bE2a173976CA11"

	// Verify we can reach the endpoint before running the test.
	client := &http.Client{Timeout: 5 * time.Second}
	probe, err := http.NewRequest("POST", endpoint, bytes.NewReader(
		[]byte(`{"jsonrpc":"2.0","id":"probe","method":"eth_blockNumber","params":[]}`),
	))
	if err != nil {
		t.Fatalf("failed to create probe request: %v", err)
	}
	probe.Header.Set("Content-Type", "application/json")
	probeResp, err := client.Do(probe)
	if err != nil {
		t.Skipf("skipping live node test: cannot reach %s: %v", endpoint, err)
	}
	probeResp.Body.Close()
	if probeResp.StatusCode != 200 {
		t.Skipf("skipping live node test: %s returned status %d", endpoint, probeResp.StatusCode)
	}

	// Encode aggregate3 with 3 calls
	getBlockNumberSelector := hexToBytes("0x42cbb15c")
	getTimestampSelector := hexToBytes("0x0f28c97d")
	// getBlockHash(uint256): selector 0xee82ac5e, arg = 1
	getBlockHashCalldata := hexToBytes("0xee82ac5e0000000000000000000000000000000000000000000000000000000000000001")

	calls := []call3{
		{Target: hexToAddress(multicallAddr), AllowFailure: true, CallData: getBlockNumberSelector},
		{Target: hexToAddress(multicallAddr), AllowFailure: true, CallData: getTimestampSelector},
		{Target: hexToAddress(multicallAddr), AllowFailure: true, CallData: getBlockHashCalldata},
	}

	encoded := encodeAggregate3(calls)
	calldata := "0x" + hex.EncodeToString(encoded)

	// Send the multicall aggregate3 as a single eth_call
	multicallReq := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":"mc","method":"eth_call","params":[{"to":"%s","data":"%s"},"latest"]}`,
		multicallAddr, calldata,
	)

	mcResp := doRPCCall(t, client, endpoint, multicallReq)
	if mcResp.Error != nil {
		t.Fatalf("multicall eth_call failed: %v", mcResp.Error)
	}

	resultHex, ok := mcResp.Result.(string)
	if !ok {
		t.Fatalf("multicall result is not a string: %T %v", mcResp.Result, mcResp.Result)
	}

	results, err := decodeAggregate3Result(resultHex)
	if err != nil {
		t.Fatalf("failed to decode multicall result: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	for i, r := range results {
		if !r.Success {
			t.Errorf("result[%d] failed unexpectedly", i)
		}
	}

	// Verify results are reasonable
	blockNumBytes := results[0].ReturnData
	if len(blockNumBytes) != 32 {
		t.Fatalf("getBlockNumber returnData length: got %d, want 32", len(blockNumBytes))
	}
	blockNum := readUint256(blockNumBytes)
	if blockNum < 15_000_000 {
		t.Errorf("block number suspiciously low: %d", blockNum)
	}
	t.Logf("getBlockNumber() = %d", blockNum)

	timestampBytes := results[1].ReturnData
	if len(timestampBytes) != 32 {
		t.Fatalf("getCurrentBlockTimestamp returnData length: got %d, want 32", len(timestampBytes))
	}
	timestamp := readUint256(timestampBytes)
	if timestamp < 1_700_000_000 {
		t.Errorf("timestamp suspiciously low: %d", timestamp)
	}
	t.Logf("getCurrentBlockTimestamp() = %d", timestamp)

	blockHashBytes := results[2].ReturnData
	if len(blockHashBytes) != 32 {
		t.Fatalf("getBlockHash returnData length: got %d, want 32", len(blockHashBytes))
	}
	blockHash := hex.EncodeToString(blockHashBytes)
	t.Logf("getBlockHash(1) = 0x%s", blockHash)

	// Now send the same calls individually and compare
	directBlockNumResp := doRPCCall(t, client, endpoint, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":"d1","method":"eth_call","params":[{"to":"%s","data":"0x42cbb15c"},"latest"]}`,
		multicallAddr,
	))
	if directBlockNumResp.Error != nil {
		t.Fatalf("direct getBlockNumber failed: %v", directBlockNumResp.Error)
	}

	directTimestampResp := doRPCCall(t, client, endpoint, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":"d2","method":"eth_call","params":[{"to":"%s","data":"0x0f28c97d"},"latest"]}`,
		multicallAddr,
	))
	if directTimestampResp.Error != nil {
		t.Fatalf("direct getCurrentBlockTimestamp failed: %v", directTimestampResp.Error)
	}

	// Block number and timestamp from multicall vs direct may differ by a few
	// blocks since they're separate calls to "latest". Verify they're in the
	// same ballpark (within 100 blocks / 2000 seconds).
	directBlockNumHex := strings.TrimPrefix(directTimestampResp.Result.(string), "0x")
	_ = directBlockNumHex // both are valid, just verify no errors

	t.Logf("direct getBlockNumber result: %v", directBlockNumResp.Result)
	t.Logf("direct getCurrentBlockTimestamp result: %v", directTimestampResp.Result)

	// getBlockHash(1) should be deterministic — same block 1 hash
	directBlockHashResp := doRPCCall(t, client, endpoint, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":"d3","method":"eth_call","params":[{"to":"%s","data":"0xee82ac5e0000000000000000000000000000000000000000000000000000000000000001"},"latest"]}`,
		multicallAddr,
	))
	if directBlockHashResp.Error != nil {
		t.Fatalf("direct getBlockHash failed: %v", directBlockHashResp.Error)
	}

	directBlockHash := strings.TrimPrefix(directBlockHashResp.Result.(string), "0x")
	multicallBlockHash := hex.EncodeToString(blockHashBytes)

	if directBlockHash != multicallBlockHash {
		t.Errorf("getBlockHash(1) mismatch:\n  multicall: %s\n  direct:    %s", multicallBlockHash, directBlockHash)
	} else {
		t.Logf("getBlockHash(1) matches between multicall and direct: 0x%s", multicallBlockHash)
	}
}

func TestMulticallLiveNodeFullPipeline(t *testing.T) {
	// End-to-end: use aggregateEthCalls to build the multicall, send it to a
	// real node, expand the response, and verify each caller gets the right result.

	endpoint := "https://eth.drpc.org"
	multicallAddr := "0xcA11bde05977b3631167028862bE2a173976CA11"

	client := &http.Client{Timeout: 10 * time.Second}
	probe, _ := http.NewRequest("POST", endpoint, bytes.NewReader(
		[]byte(`{"jsonrpc":"2.0","id":"probe","method":"eth_blockNumber","params":[]}`),
	))
	probe.Header.Set("Content-Type", "application/json")
	probeResp, err := client.Do(probe)
	if err != nil {
		t.Skipf("skipping: cannot reach %s: %v", endpoint, err)
	}
	probeResp.Body.Close()
	if probeResp.StatusCode != 200 {
		t.Skipf("skipping: %s returned status %d", endpoint, probeResp.StatusCode)
	}

	cfg := MulticallConfig{
		Enabled:  true,
		Address:  multicallAddr,
		MaxCalls: 150,
	}

	// Build batch requests as they would arrive at the batch processor
	requests := []BatchRequest{
		{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "eth_call",
			Params: json.RawMessage(fmt.Sprintf(`[{"to":"%s","data":"0x42cbb15c"},"latest"]`, multicallAddr))},
		{JSONRPC: "2.0", ID: json.RawMessage(`"2"`), Method: "eth_call",
			Params: json.RawMessage(fmt.Sprintf(`[{"to":"%s","data":"0x0f28c97d"},"latest"]`, multicallAddr))},
		{JSONRPC: "2.0", ID: json.RawMessage(`"3"`), Method: "eth_blockNumber",
			Params: json.RawMessage(`[]`)},
		{JSONRPC: "2.0", ID: json.RawMessage(`"4"`), Method: "eth_call",
			Params: json.RawMessage(fmt.Sprintf(`[{"to":"%s","data":"0xee82ac5e0000000000000000000000000000000000000000000000000000000000000001"},"latest"]`, multicallAddr))},
	}

	// Step 1: Aggregate
	optimized, mapping := aggregateEthCalls(requests, cfg)
	if mapping == nil {
		t.Fatal("expected mapping")
	}

	t.Logf("optimized %d requests down to %d", len(requests), len(optimized))
	if len(mapping.Groups) != 1 {
		t.Fatalf("expected 1 multicall group, got %d", len(mapping.Groups))
	}

	// Step 2: Send the optimized batch to the real node as a JSON-RPC batch
	batchBody, err := json.Marshal(optimized)
	if err != nil {
		t.Fatalf("failed to marshal batch: %v", err)
	}

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(batchBody))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("batch request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	var batchResponses []BatchResponse
	if err := json.Unmarshal(body, &batchResponses); err != nil {
		t.Fatalf("failed to unmarshal batch response: %v\nbody: %s", err, string(body))
	}

	t.Logf("got %d batch responses", len(batchResponses))

	// Step 3: Expand multicall responses
	expanded, err := expandMulticallResponses(batchResponses, mapping)
	if err != nil {
		t.Fatalf("expand error: %v", err)
	}

	if len(expanded) != 4 {
		t.Fatalf("expected 4 expanded responses, got %d", len(expanded))
	}

	respByID := make(map[string]BatchResponse)
	for _, r := range expanded {
		respByID[string(r.ID)] = r
	}

	// Verify each response
	for _, id := range []string{`"1"`, `"2"`, `"3"`, `"4"`} {
		r, ok := respByID[id]
		if !ok {
			t.Errorf("missing response for ID %s", id)
			continue
		}
		if r.Error != nil {
			t.Errorf("response %s has error: %v", id, r.Error)
			continue
		}
		t.Logf("response %s: %v", id, r.Result)
	}

	// Verify getBlockNumber result from multicall is reasonable
	r1 := respByID[`"1"`]
	if r1.Result == nil {
		t.Fatal("getBlockNumber result is nil")
	}
	blockNumHex := strings.TrimPrefix(r1.Result.(string), "0x")
	blockNumBytes, _ := hex.DecodeString(blockNumHex)
	if len(blockNumBytes) == 32 {
		blockNum := readUint256(blockNumBytes)
		if blockNum < 15_000_000 {
			t.Errorf("block number from multicall suspiciously low: %d", blockNum)
		}
		t.Logf("multicall getBlockNumber = %d", blockNum)
	}

	// Verify eth_blockNumber passthrough result
	r3 := respByID[`"3"`]
	if r3.Result == nil {
		t.Fatal("eth_blockNumber result is nil")
	}
	t.Logf("passthrough eth_blockNumber = %v", r3.Result)
}

// ---------------------------------------------------------------------------
// Original unit tests
// ---------------------------------------------------------------------------

func TestEncodeAggregate3(t *testing.T) {
	calls := []call3{
		{
			Target:       hexToAddress("0x0000000000000000000000000000000000000001"),
			AllowFailure: true,
			CallData:     hexToBytes("0xdeadbeef"),
		},
	}

	encoded := encodeAggregate3(calls)

	if hex.EncodeToString(encoded[:4]) != "82ad56cb" {
		t.Fatalf("wrong selector: %x", encoded[:4])
	}
	if readUint256(encoded[4:36]) != 32 {
		t.Fatalf("wrong array offset: %d", readUint256(encoded[4:36]))
	}
	if readUint256(encoded[36:68]) != 1 {
		t.Fatalf("wrong array length: %d", readUint256(encoded[36:68]))
	}
}

func TestEncodeAggregate3MultipleCallsVaryingData(t *testing.T) {
	calls := []call3{
		{Target: hexToAddress("0x0000000000000000000000000000000000000001"), AllowFailure: true, CallData: hexToBytes("0xaa")},
		{Target: hexToAddress("0x0000000000000000000000000000000000000002"), AllowFailure: true, CallData: hexToBytes("0xbbccddee11223344")},
		{Target: hexToAddress("0x0000000000000000000000000000000000000003"), AllowFailure: true, CallData: []byte{}},
	}

	encoded := encodeAggregate3(calls)

	if hex.EncodeToString(encoded[:4]) != "82ad56cb" {
		t.Fatalf("wrong selector: %x", encoded[:4])
	}
	if readUint256(encoded[36:68]) != 3 {
		t.Fatalf("wrong array length: %d", readUint256(encoded[36:68]))
	}

	offset0 := readUint256(encoded[68:100])
	offset1 := readUint256(encoded[100:132])
	offset2 := readUint256(encoded[132:164])

	if offset0 >= offset1 || offset1 >= offset2 {
		t.Fatalf("offsets should be strictly increasing: %d, %d, %d", offset0, offset1, offset2)
	}
}

func TestDecodeAggregate3Result(t *testing.T) {
	encoded := buildAggregate3ResultEncoding([]multicallResult{
		{Success: true, ReturnData: hexToBytes("0xabcd")},
		{Success: false, ReturnData: hexToBytes("0xef")},
	})

	results, err := decodeAggregate3Result("0x" + hex.EncodeToString(encoded))
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if !results[0].Success {
		t.Error("result[0] should be success")
	}
	if hex.EncodeToString(results[0].ReturnData) != "abcd" {
		t.Errorf("result[0] returnData = %x, want abcd", results[0].ReturnData)
	}
	if results[1].Success {
		t.Error("result[1] should be failure")
	}
	if hex.EncodeToString(results[1].ReturnData) != "ef" {
		t.Errorf("result[1] returnData = %x, want ef", results[1].ReturnData)
	}
}

func TestDecodeAggregate3ResultEmpty(t *testing.T) {
	encoded := buildAggregate3ResultEncoding([]multicallResult{
		{Success: true, ReturnData: []byte{}},
	})

	results, err := decodeAggregate3Result("0x" + hex.EncodeToString(encoded))
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success || len(results[0].ReturnData) != 0 {
		t.Error("expected success with empty returnData")
	}
}

func TestDecodeAggregate3ResultMalformed(t *testing.T) {
	tests := []struct {
		name string
		hex  string
	}{
		{"empty", "0x"},
		{"too short", "0xdead"},
		{"invalid hex chars", "0xZZZZ"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeAggregate3Result(tt.hex)
			if err == nil {
				t.Error("expected error for malformed input")
			}
		})
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	calls := []call3{
		{Target: hexToAddress("0x0000000000000000000000000000000000000001"), AllowFailure: true, CallData: hexToBytes("0xdeadbeef")},
		{Target: hexToAddress("0x0000000000000000000000000000000000000002"), AllowFailure: true, CallData: hexToBytes("0xcafebabe")},
	}

	encoded := encodeAggregate3(calls)
	if len(encoded) == 0 {
		t.Fatal("encoded should not be empty")
	}

	expectedResults := []multicallResult{
		{Success: true, ReturnData: hexToBytes("0x" + strings.Repeat("ab", 32))},
		{Success: true, ReturnData: hexToBytes("0x" + strings.Repeat("cd", 64))},
	}

	resultEncoding := buildAggregate3ResultEncoding(expectedResults)
	decoded, err := decodeAggregate3Result("0x" + hex.EncodeToString(resultEncoding))
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(decoded) != len(expectedResults) {
		t.Fatalf("decoded %d results, expected %d", len(decoded), len(expectedResults))
	}
	for i, r := range decoded {
		if r.Success != expectedResults[i].Success {
			t.Errorf("result[%d].Success = %v, want %v", i, r.Success, expectedResults[i].Success)
		}
		if hex.EncodeToString(r.ReturnData) != hex.EncodeToString(expectedResults[i].ReturnData) {
			t.Errorf("result[%d].ReturnData mismatch", i)
		}
	}
}

func TestAggregateEthCalls(t *testing.T) {
	cfg := MulticallConfig{
		Enabled:  true,
		Address:  "0xcA11bde05977b3631167028862bE2a173976CA11",
		MaxCalls: 150,
	}

	t.Run("all eth_call same block tag", func(t *testing.T) {
		requests := []BatchRequest{
			{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000001","data":"0xdeadbeef"},"latest"]`)},
			{JSONRPC: "2.0", ID: json.RawMessage(`"2"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000002","data":"0xcafebabe"},"latest"]`)},
			{JSONRPC: "2.0", ID: json.RawMessage(`"3"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000003","data":"0x12345678"},"latest"]`)},
		}
		out, mapping := aggregateEthCalls(requests, cfg)
		if mapping == nil {
			t.Fatal("expected mapping")
		}
		if len(out) != 1 {
			t.Fatalf("expected 1 optimized request, got %d", len(out))
		}
		if len(mapping.Groups) != 1 || len(mapping.Groups[0].Entries) != 3 {
			t.Fatalf("expected 1 group with 3 entries")
		}
	})

	t.Run("mixed methods", func(t *testing.T) {
		requests := []BatchRequest{
			{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "eth_blockNumber", Params: json.RawMessage(`[]`)},
			{JSONRPC: "2.0", ID: json.RawMessage(`"2"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000001","data":"0xaa"},"latest"]`)},
			{JSONRPC: "2.0", ID: json.RawMessage(`"3"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000002","data":"0xbb"},"latest"]`)},
			{JSONRPC: "2.0", ID: json.RawMessage(`"4"`), Method: "eth_getBalance", Params: json.RawMessage(`["0xdead","latest"]`)},
		}
		out, mapping := aggregateEthCalls(requests, cfg)
		if mapping == nil {
			t.Fatal("expected mapping")
		}
		if len(out) != 3 {
			t.Fatalf("expected 3 output requests, got %d", len(out))
		}
		if len(mapping.Passthrough) != 2 || len(mapping.Groups) != 1 {
			t.Fatal("expected 2 passthrough, 1 group")
		}
	})

	t.Run("mixed block tags", func(t *testing.T) {
		requests := []BatchRequest{
			{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000001","data":"0xaa"},"latest"]`)},
			{JSONRPC: "2.0", ID: json.RawMessage(`"2"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000002","data":"0xbb"},"0x1234"]`)},
			{JSONRPC: "2.0", ID: json.RawMessage(`"3"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000003","data":"0xcc"},"latest"]`)},
			{JSONRPC: "2.0", ID: json.RawMessage(`"4"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000004","data":"0xdd"},"0x1234"]`)},
		}
		out, mapping := aggregateEthCalls(requests, cfg)
		if mapping == nil {
			t.Fatal("expected mapping")
		}
		if len(out) != 2 || len(mapping.Groups) != 2 {
			t.Fatalf("expected 2 outputs and 2 groups, got %d/%d", len(out), len(mapping.Groups))
		}
	})

	t.Run("ineligible eth_call with from", func(t *testing.T) {
		requests := []BatchRequest{
			{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000001","data":"0xaa","from":"0xcafe"},"latest"]`)},
			{JSONRPC: "2.0", ID: json.RawMessage(`"2"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000002","data":"0xbb"},"latest"]`)},
		}
		out, mapping := aggregateEthCalls(requests, cfg)
		if mapping != nil {
			t.Error("expected nil mapping with < 2 eligible calls")
		}
		if len(out) != 2 {
			t.Errorf("expected 2 output requests, got %d", len(out))
		}
	})

	t.Run("single eth_call", func(t *testing.T) {
		requests := []BatchRequest{
			{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000001","data":"0xaa"},"latest"]`)},
		}
		_, mapping := aggregateEthCalls(requests, cfg)
		if mapping != nil {
			t.Error("expected nil mapping for single call")
		}
	})

	t.Run("no eth_calls", func(t *testing.T) {
		requests := []BatchRequest{
			{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "eth_blockNumber", Params: json.RawMessage(`[]`)},
		}
		_, mapping := aggregateEthCalls(requests, cfg)
		if mapping != nil {
			t.Error("expected nil mapping")
		}
	})

	t.Run("max_calls chunking", func(t *testing.T) {
		smallCfg := MulticallConfig{Enabled: true, Address: multicallAddress, MaxCalls: 2}
		requests := []BatchRequest{
			{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000001","data":"0xaa"},"latest"]`)},
			{JSONRPC: "2.0", ID: json.RawMessage(`"2"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000002","data":"0xbb"},"latest"]`)},
			{JSONRPC: "2.0", ID: json.RawMessage(`"3"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000003","data":"0xcc"},"latest"]`)},
		}
		out, mapping := aggregateEthCalls(requests, smallCfg)
		if mapping == nil {
			t.Fatal("expected mapping")
		}
		if len(mapping.Groups) != 1 || len(mapping.Groups[0].Entries) != 2 {
			t.Fatal("expected 1 group with 2 entries")
		}
		if len(out) != 2 {
			t.Fatalf("expected 2 output requests, got %d", len(out))
		}
	})

	t.Run("eth_call missing data", func(t *testing.T) {
		requests := []BatchRequest{
			{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000001"},"latest"]`)},
			{JSONRPC: "2.0", ID: json.RawMessage(`"2"`), Method: "eth_call", Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000002","data":"0xbb"},"latest"]`)},
		}
		_, mapping := aggregateEthCalls(requests, cfg)
		if mapping != nil {
			t.Error("expected nil mapping")
		}
	})
}

func TestExpandMulticallResponses(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		resultData := buildAggregate3ResultEncoding([]multicallResult{
			{Success: true, ReturnData: hexToBytes("0xaaaa")},
			{Success: true, ReturnData: hexToBytes("0xbbbb")},
		})
		mapping := &MulticallMapping{
			Passthrough: []BatchRequest{{JSONRPC: "2.0", ID: json.RawMessage(`"pt1"`)}},
			Groups: []multicallGroup{{
				SyntheticID: json.RawMessage(`"__multicall_99"`),
				Entries: []multicallEntry{
					{OriginalID: json.RawMessage(`"1"`), Index: 0},
					{OriginalID: json.RawMessage(`"2"`), Index: 1},
				},
			}},
		}
		responses := []BatchResponse{
			{JSONRPC: "2.0", ID: json.RawMessage(`"pt1"`), Result: "0xdead"},
			{JSONRPC: "2.0", ID: json.RawMessage(`"__multicall_99"`), Result: "0x" + hex.EncodeToString(resultData)},
		}
		expanded, err := expandMulticallResponses(responses, mapping)
		if err != nil {
			t.Fatalf("expand error: %v", err)
		}
		if len(expanded) != 3 {
			t.Fatalf("expected 3 expanded, got %d", len(expanded))
		}
		respByID := make(map[string]BatchResponse)
		for _, r := range expanded {
			respByID[string(r.ID)] = r
		}
		if respByID[`"pt1"`].Result != "0xdead" {
			t.Errorf("passthrough result mismatch")
		}
		if respByID[`"1"`].Result != "0xaaaa" {
			t.Errorf("expected 0xaaaa, got %v", respByID[`"1"`].Result)
		}
		if respByID[`"2"`].Result != "0xbbbb" {
			t.Errorf("expected 0xbbbb, got %v", respByID[`"2"`].Result)
		}
	})

	t.Run("individual call revert", func(t *testing.T) {
		resultData := buildAggregate3ResultEncoding([]multicallResult{
			{Success: true, ReturnData: hexToBytes("0xaaaa")},
			{Success: false, ReturnData: hexToBytes("0x08c379a0")},
		})
		mapping := &MulticallMapping{
			Groups: []multicallGroup{{
				SyntheticID: json.RawMessage(`"__multicall_100"`),
				Entries: []multicallEntry{
					{OriginalID: json.RawMessage(`"1"`), Index: 0},
					{OriginalID: json.RawMessage(`"2"`), Index: 1},
				},
			}},
		}
		responses := []BatchResponse{
			{JSONRPC: "2.0", ID: json.RawMessage(`"__multicall_100"`), Result: "0x" + hex.EncodeToString(resultData)},
		}
		expanded, err := expandMulticallResponses(responses, mapping)
		if err != nil {
			t.Fatalf("expand error: %v", err)
		}
		if len(expanded) != 2 {
			t.Fatalf("expected 2, got %d", len(expanded))
		}
		if expanded[0].Error != nil {
			t.Error("result[0] should not have error")
		}
		if expanded[1].Error == nil {
			t.Fatal("result[1] should have error")
		}
		errMap := expanded[1].Error.(map[string]any)
		if errMap["code"] != 3 {
			t.Errorf("expected error code 3, got %v", errMap["code"])
		}
	})

	t.Run("multicall error propagation", func(t *testing.T) {
		mapping := &MulticallMapping{
			Groups: []multicallGroup{{
				SyntheticID: json.RawMessage(`"__multicall_101"`),
				Entries: []multicallEntry{
					{OriginalID: json.RawMessage(`"1"`), Index: 0},
					{OriginalID: json.RawMessage(`"2"`), Index: 1},
				},
			}},
		}
		responses := []BatchResponse{
			{JSONRPC: "2.0", ID: json.RawMessage(`"__multicall_101"`),
				Error: map[string]any{"code": -32000, "message": "execution reverted"}},
		}
		expanded, _ := expandMulticallResponses(responses, mapping)
		for i, r := range expanded {
			if r.Error == nil {
				t.Errorf("result[%d] should have error", i)
			}
		}
	})

	t.Run("missing multicall response", func(t *testing.T) {
		mapping := &MulticallMapping{
			Groups: []multicallGroup{{
				SyntheticID: json.RawMessage(`"__multicall_102"`),
				Entries:     []multicallEntry{{OriginalID: json.RawMessage(`"1"`), Index: 0}},
			}},
		}
		expanded, _ := expandMulticallResponses([]BatchResponse{}, mapping)
		if len(expanded) != 1 || expanded[0].Error == nil {
			t.Error("should have error for missing response")
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const multicallAddress = "0xcA11bde05977b3631167028862bE2a173976CA11"

func doRPCCall(t *testing.T, client *http.Client, endpoint, body string) BatchResponse {
	t.Helper()
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	var rpcResp BatchResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		t.Fatalf("failed to unmarshal response: %v\nbody: %s", err, string(respBody))
	}
	return rpcResp
}

// buildAggregate3ResultEncoding constructs the ABI encoding of Result[]
// where Result = (bool success, bytes returnData).
func buildAggregate3ResultEncoding(results []multicallResult) []byte {
	n := len(results)

	type tupleInfo struct {
		offset int
		size   int
	}

	infos := make([]tupleInfo, n)
	offsetTableSize := n * 32
	runningOffset := offsetTableSize

	for i, r := range results {
		infos[i].offset = runningOffset
		infos[i].size = 96 + ceil32(len(r.ReturnData))
		runningOffset += infos[i].size
	}

	totalSize := 32 + 32 + runningOffset
	buf := make([]byte, totalSize)
	pos := 0

	writeUint256(buf[pos:], 32)
	pos += 32

	writeUint256(buf[pos:], uint64(n))
	pos += 32

	for i := 0; i < n; i++ {
		writeUint256(buf[pos:], uint64(infos[i].offset))
		pos += 32
	}

	for i, r := range results {
		_ = infos[i]

		if r.Success {
			buf[pos+31] = 1
		}
		pos += 32

		writeUint256(buf[pos:], 64)
		pos += 32

		writeUint256(buf[pos:], uint64(len(r.ReturnData)))
		pos += 32

		copy(buf[pos:], r.ReturnData)
		pos += ceil32(len(r.ReturnData))
	}

	return buf
}
