package backend

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// blockParamIndex maps each method that pins a request to a block number to
// the position of its block-tag parameter. eth_call carries an optional state
// override after the tag, so positions are fixed rather than "last".
var blockParamIndex = map[string]int{
	"eth_getBalance":                       1,
	"eth_getCode":                          1,
	"eth_getTransactionCount":              1,
	"eth_getStorageAt":                     2,
	"eth_call":                             1,
	"eth_getProof":                         2,
	"eth_getBlockByNumber":                 0,
	"eth_getBlockTransactionCountByNumber": 0,
	"debug_traceBlockByNumber":             0,
}

var blockUnavailableRegex = regexp.MustCompile(`(?i)header not found|missing trie node|unknown block|block not found|block .* not found|state not available|cannot query unfinalized`)

// RequiredHead returns the block number a request is pinned to, or 0 when the
// request carries no numeric block tag (named tags, unknown methods, malformed
// params). A batch caller takes the max over its items.
func RequiredHead(method string, params json.RawMessage) uint64 {
	if method == "eth_getLogs" {
		var filters []struct {
			ToBlock json.RawMessage `json:"toBlock"`
		}
		if json.Unmarshal(params, &filters) != nil || len(filters) == 0 {
			return 0
		}
		return quantityFromRaw(filters[0].ToBlock)
	}
	idx, ok := blockParamIndex[method]
	if !ok {
		return 0
	}
	var items []json.RawMessage
	if json.Unmarshal(params, &items) != nil || idx >= len(items) {
		return 0
	}
	return quantityFromRaw(items[idx])
}

// HeadFromResponse extracts a block height from a JSON-RPC response whose
// result is either a hex quantity (eth_blockNumber) or a block object.
func HeadFromResponse(response []byte) (uint64, bool) {
	var resp struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response, &resp); err != nil {
		return 0, false
	}
	if n := quantityFromRaw(resp.Result); n > 0 {
		return n, true
	}
	if n, _ := ExtractBlockInfo(response); n > 0 {
		return n, true
	}
	return 0, false
}

// BlockUnavailable reports whether a 200 response (single object or batch)
// carries a JSON-RPC error indicating the upstream has not imported, or has
// pruned, the block the request was pinned to.
func BlockUnavailable(response []byte) bool {
	trimmed := bytes.TrimSpace(response)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(trimmed, &items) != nil {
			return false
		}
		for _, item := range items {
			if blockUnavailableSingle(item) {
				return true
			}
		}
		return false
	}
	return blockUnavailableSingle(trimmed)
}

func blockUnavailableSingle(response []byte) bool {
	errBody, ok := errorField(response)
	if !ok {
		return false
	}
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(errBody, &e) != nil {
		return false
	}
	return blockUnavailableRegex.MatchString(e.Message)
}

// IsErrorResponse reports whether a single JSON-RPC response object carries a
// non-null error member.
func IsErrorResponse(response []byte) bool {
	_, ok := errorField(response)
	return ok
}

func errorField(response []byte) (json.RawMessage, bool) {
	var r struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(response, &r) != nil {
		return nil, false
	}
	e := bytes.TrimSpace(r.Error)
	if len(e) == 0 || bytes.Equal(e, []byte("null")) {
		return nil, false
	}
	return e, true
}

func quantityFromRaw(raw json.RawMessage) uint64 {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0
	}
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return 0
	}
	n, err := strconv.ParseUint(s[2:], 16, 64)
	if err != nil {
		return 0
	}
	return n
}
