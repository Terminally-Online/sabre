package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// IsAggregate3Multicall reports whether an eth_call wraps a Multicall3
// aggregate3 batch. Such calls are handled by the immutable sub-call cache
// rather than the normal whole-request cache.
func IsAggregate3Multicall(method string, params json.RawMessage) bool {
	if method != "eth_call" {
		return false
	}
	parsed, ok := parseEthCallParams(params)
	if !ok || parsed.Params.Data == "" {
		return false
	}
	calls, ok := decodeAggregate3Calls(hexToBytes(parsed.Params.Data))
	return ok && len(calls) > 0
}

// subCallKey is the block-independent cache key for a single immutable
// sub-call. Because the response can never change, the key intentionally
// excludes the block tag and any state overrides — only chain, target
// contract, and calldata matter — so a cached entry is reused across blocks
// and, critically, across client database resets and reindexes.
func subCallKey(chain string, target [20]byte, callData []byte) string {
	h := sha256.New()
	h.Write([]byte(chain))
	h.Write([]byte{0})
	h.Write(target[:])
	h.Write([]byte{0})
	h.Write(callData)
	return fmt.Sprintf("sub:v1:%x", h.Sum(nil))
}

// ServeImmutableMulticall attempts to satisfy an aggregate3 multicall entirely
// from the immutable sub-call cache. It decodes the inner calls and, only if
// every one targets an immutable selector AND is already cached, synthesizes
// and returns the complete aggregate3 result hex — no upstream call needed.
//
// This is what makes a gusher reindex after a database dump free: every token's
// decimals/symbol/name was cached on the first index ever, so subsequent runs
// are served from sabre regardless of how the tokens are re-batched. Returns
// ok=false the moment any sub-call is mutable or uncached, in which case the
// caller forwards the whole multicall upstream.
func (s *Store) ServeImmutableMulticall(chain, method string, params json.RawMessage) (string, bool) {
	if !s.cfg.Enabled || method != "eth_call" {
		return "", false
	}
	parsed, ok := parseEthCallParams(params)
	if !ok || parsed.Params.Data == "" {
		return "", false
	}
	calls, ok := decodeAggregate3Calls(hexToBytes(parsed.Params.Data))
	if !ok || len(calls) == 0 {
		return "", false
	}

	results := make([]multicallResult, len(calls))
	for i, c := range calls {
		if !isImmutableSelector(c.CallData) {
			return "", false
		}
		body, ok := s.Get(subCallKey(chain, c.Target, c.CallData))
		if !ok {
			return "", false
		}
		results[i] = multicallResult{Success: true, ReturnData: body}
	}

	return "0x" + hex.EncodeToString(encodeAggregate3Result(results)), true
}

// PopulateImmutableMulticall caches the immutable sub-calls of a successful
// aggregate3 multicall response so future batches containing the same reads
// can be served from cache. Mutable sub-calls and reverted sub-calls are never
// stored. It is a no-op for non-multicall requests.
func (s *Store) PopulateImmutableMulticall(chain string, params json.RawMessage, responseBody []byte) {
	if !s.cfg.Enabled {
		return
	}
	parsed, ok := parseEthCallParams(params)
	if !ok || parsed.Params.Data == "" {
		return
	}
	calls, ok := decodeAggregate3Calls(hexToBytes(parsed.Params.Data))
	if !ok || len(calls) == 0 {
		return
	}

	var envelope struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(responseBody, &envelope) != nil || envelope.Result == "" {
		return
	}
	results, err := decodeAggregate3Result(envelope.Result)
	if err != nil || len(results) != len(calls) {
		return
	}

	for i, c := range calls {
		if !isImmutableSelector(c.CallData) || !results[i].Success {
			continue
		}
		s.PutImmortal(subCallKey(chain, c.Target, c.CallData), results[i].ReturnData, chain)
	}
}
