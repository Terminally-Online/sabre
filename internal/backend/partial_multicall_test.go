package backend

import (
	"encoding/json"
	"testing"
)

func decimalsOn(hexAddr string) call3 {
	return call3{Target: hexToAddress(hexAddr), AllowFailure: true, CallData: []byte{0x31, 0x3c, 0xe5, 0x67}}
}

func decimalsReturn(v byte) []byte {
	out := make([]byte, 32)
	out[31] = v
	return out
}

// TestImmutableSelectors covers the view functions whose results are cached
// forever — including a pair's token0/token1, which are constructor-fixed.
func TestImmutableSelectors(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"decimals()", []byte{0x31, 0x3c, 0xe5, 0x67}, true},
		{"symbol()", []byte{0x95, 0xd8, 0x9b, 0x41}, true},
		{"name()", []byte{0x06, 0xfd, 0xde, 0x03}, true},
		{"token0()", []byte{0x0d, 0xfe, 0x16, 0x81}, true},
		{"token1()", []byte{0xd2, 0x12, 0x20, 0xa7}, true},
		{"balanceOf(addr)", []byte{0x70, 0xa0, 0x82, 0x31}, false},
		{"short", []byte{0x0d, 0xfe}, false},
	}
	for _, tc := range cases {
		if got := isImmutableSelector(tc.data); got != tc.want {
			t.Errorf("isImmutableSelector(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPartialMulticallServing verifies the core fix: a multicall of cached +
// uncached immutable sub-calls forwards ONLY the misses upstream, then merges the
// full result back in order and caches the newly-fetched ones.
func TestPartialMulticallServing(t *testing.T) {
	store := openTestStore(t)
	const chain = "1"
	a, b, c := decimalsOn("0xaa"), decimalsOn("0xbb"), decimalsOn("0xcc")

	// A and C cached (6, 8); B unknown.
	store.PutImmortal(subCallKey(chain, a.Target, a.CallData), decimalsReturn(6), chain)
	store.PutImmortal(subCallKey(chain, c.Target, c.CallData), decimalsReturn(8), chain)

	id := json.RawMessage(`7`)
	plan, ok := store.PlanMulticall(chain, "eth_call", multicallParams(t, "latest", []call3{a, b, c}), id)
	if !ok {
		t.Fatal("expected a partial plan (A,C cached; B missing)")
	}

	// Reduced request must carry ONLY B.
	var reduced struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(plan.ReducedBody, &reduced); err != nil {
		t.Fatalf("reduced body: %v", err)
	}
	parsed, _ := parseEthCallParams(reduced.Params)
	rc, ok := decodeAggregate3Calls(hexToBytes(parsed.Params.Data))
	if !ok || len(rc) != 1 || rc[0].Target != b.Target {
		t.Fatalf("reduced request should be exactly [B], got %d calls", len(rc))
	}

	// Upstream returns B's decimals = 18.
	full, ok := store.CompleteMulticall(chain, plan,
		multicallResponse(t, []multicallResult{{Success: true, ReturnData: decimalsReturn(18)}}))
	if !ok {
		t.Fatal("CompleteMulticall failed")
	}

	// Full result must be A=6, B=18, C=8 in original order.
	var env struct {
		Result string `json:"result"`
	}
	_ = json.Unmarshal(full, &env)
	res, err := decodeAggregate3Result(env.Result)
	if err != nil || len(res) != 3 {
		t.Fatalf("full result decode: %v (n=%d)", err, len(res))
	}
	for i, want := range []byte{6, 18, 8} {
		if !res[i].Success || res[i].ReturnData[31] != want {
			t.Fatalf("result[%d]=%d, want %d", i, res[i].ReturnData[31], want)
		}
	}

	// B is now cached: the whole multicall serves fully from cache.
	if _, served := store.serveMulticall(chain, id, []call3{a, b, c}); !served {
		t.Fatal("after completion all three should serve from cache")
	}
	// And a fresh plan finds nothing to forward (all cached) -> PlanMulticall ok=false.
	if _, ok := store.PlanMulticall(chain, "eth_call", multicallParams(t, "latest", []call3{a, b, c}), id); ok {
		t.Fatal("fully-cached multicall should not need a partial plan")
	}
}
