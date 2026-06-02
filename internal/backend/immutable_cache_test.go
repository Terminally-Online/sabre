package backend

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"testing"
)

var (
	selDecimals = [4]byte{0x31, 0x3c, 0xe5, 0x67}
	selSymbol   = [4]byte{0x95, 0xd8, 0x9b, 0x41}
	selName     = [4]byte{0x06, 0xfd, 0xde, 0x03}
	selSupply   = [4]byte{0x18, 0x16, 0x0d, 0xdd} // totalSupply() — mutable
)

const testMC3 = "0xca11bde05977b3631167028862be2a173976ca11"

func mcParams(t *testing.T, to, blockTag string, calls []call3) json.RawMessage {
	t.Helper()
	data := "0x" + hex.EncodeToString(encodeAggregate3(calls))
	callObj, _ := json.Marshal(map[string]string{"to": to, "data": data})
	tag, _ := json.Marshal(blockTag)
	params, _ := json.Marshal([]json.RawMessage{callObj, json.RawMessage(tag)})
	return params
}

func mcResponse(t *testing.T, results []multicallResult) []byte {
	t.Helper()
	resultHex := "0x" + hex.EncodeToString(encodeAggregate3Result(results))
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": resultHex})
	return body
}

func word(v uint64) []byte {
	b := make([]byte, 32)
	binary.BigEndian.PutUint64(b[24:], v)
	return b
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(CacheConfig{Enabled: true, Path: t.TempDir(), MemEntries: 1000, MaxReorgDepth: 100})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestImmutableMulticallServedAcrossBlocks is the core promise: once a metadata
// batch's sub-calls are cached, an identical-content multicall pinned to a
// DIFFERENT block (a reindex after a DB reset) is served entirely from cache.
func TestImmutableMulticallServedAcrossBlocks(t *testing.T) {
	store := openTestStore(t)
	tok := hexToAddress("0x00000000000000000000000000000000000000aa")
	calls := []call3{
		{Target: tok, CallData: selName[:]},
		{Target: tok, CallData: selSymbol[:]},
		{Target: tok, CallData: selDecimals[:]},
	}

	// Cold: nothing cached yet.
	if _, ok := store.ServeImmutableMulticall("1", "eth_call", mcParams(t, testMC3, "latest", calls)); ok {
		t.Fatal("must miss before the cache is populated")
	}

	nameRet := []byte("Wrapped Ether-padded-return-data!!")
	symRet := []byte("WETH")
	decRet := word(18)
	store.PopulateImmutableMulticall("1", mcParams(t, testMC3, "latest", calls),
		mcResponse(t, []multicallResult{{true, nameRet}, {true, symRet}, {true, decRet}}))

	// Warm: same content, DIFFERENT block tag → full hit.
	out, ok := store.ServeImmutableMulticall("1", "eth_call", mcParams(t, testMC3, "0x1312d00", calls))
	if !ok {
		t.Fatal("must serve from cache across a different block tag")
	}
	got, err := decodeAggregate3Result(out)
	if err != nil {
		t.Fatalf("synthesized result must decode: %v", err)
	}
	if len(got) != 3 || !got[0].Success || !got[1].Success || !got[2].Success {
		t.Fatalf("expected 3 successful results, got %+v", got)
	}
	if !bytes.Equal(got[0].ReturnData, nameRet) || !bytes.Equal(got[1].ReturnData, symRet) || !bytes.Equal(got[2].ReturnData, decRet) {
		t.Fatal("synthesized returnData must match what was cached")
	}
}

func TestImmutableMulticallRejectsMutable(t *testing.T) {
	store := openTestStore(t)
	tok := hexToAddress("0x00000000000000000000000000000000000000aa")

	// Pure totalSupply: populate stores nothing, serve always misses.
	supplyCalls := []call3{{Target: tok, CallData: selSupply[:]}}
	store.PopulateImmutableMulticall("1", mcParams(t, testMC3, "latest", supplyCalls),
		mcResponse(t, []multicallResult{{true, word(1_000_000)}}))
	if _, ok := store.ServeImmutableMulticall("1", "eth_call", mcParams(t, testMC3, "latest", supplyCalls)); ok {
		t.Fatal("mutable totalSupply must never be served from cache")
	}

	// Mixed: decimals cached, but a mutable sibling forces the whole call upstream.
	store.PopulateImmutableMulticall("1", mcParams(t, testMC3, "latest", []call3{{Target: tok, CallData: selDecimals[:]}}),
		mcResponse(t, []multicallResult{{true, word(18)}}))
	mixed := []call3{{Target: tok, CallData: selDecimals[:]}, {Target: tok, CallData: selSupply[:]}}
	if _, ok := store.ServeImmutableMulticall("1", "eth_call", mcParams(t, testMC3, "latest", mixed)); ok {
		t.Fatal("a multicall containing any mutable sub-call must not be served from cache")
	}
}

func TestImmutableMulticallReorgSurvival(t *testing.T) {
	store := openTestStore(t)
	tok := hexToAddress("0x00000000000000000000000000000000000000aa")
	calls := []call3{{Target: tok, CallData: selDecimals[:]}}
	store.PopulateImmutableMulticall("1", mcParams(t, testMC3, "latest", calls),
		mcResponse(t, []multicallResult{{true, word(6)}}))

	// A reorg purges the in-memory cache; immortal sub-calls carry no block
	// hash, so they survive (re-read from pebble) and still serve.
	store.UpdateLatestBlock("1", 1000, "0xabc", nil)
	store.UpdateLatestBlock("1", 1000, "0xdifferent", nil)

	if _, ok := store.ServeImmutableMulticall("1", "eth_call", mcParams(t, testMC3, "latest", calls)); !ok {
		t.Fatal("immutable sub-calls must survive a reorg purge")
	}
}

func TestIsAggregate3Multicall(t *testing.T) {
	tok := hexToAddress("0x00000000000000000000000000000000000000aa")
	mc := mcParams(t, testMC3, "latest", []call3{{Target: tok, CallData: selDecimals[:]}})
	if !IsAggregate3Multicall("eth_call", mc) {
		t.Fatal("aggregate3 multicall not recognized")
	}
	// A plain balanceOf eth_call is not a multicall.
	plain, _ := json.Marshal([]json.RawMessage{
		json.RawMessage(`{"to":"0xaa","data":"0x70a08231"}`),
		json.RawMessage(`"latest"`),
	})
	if IsAggregate3Multicall("eth_call", plain) {
		t.Fatal("plain eth_call must not be classified as a multicall")
	}
}

func TestAggregate3ResultEncodeDecodeRoundTrip(t *testing.T) {
	cases := [][]multicallResult{
		{{true, []byte("ab")}},
		{{true, word(18)}, {false, nil}, {true, make([]byte, 40)}},
		{{true, word(1)}, {true, word(2)}, {true, word(3)}, {true, word(4)}},
	}
	for ci, in := range cases {
		encoded := "0x" + hex.EncodeToString(encodeAggregate3Result(in))
		out, err := decodeAggregate3Result(encoded)
		if err != nil {
			t.Fatalf("case %d decode: %v", ci, err)
		}
		if len(out) != len(in) {
			t.Fatalf("case %d length: got %d want %d", ci, len(out), len(in))
		}
		for i := range in {
			if out[i].Success != in[i].Success || !bytes.Equal(out[i].ReturnData, in[i].ReturnData) {
				t.Fatalf("case %d elem %d mismatch: got %+v want %+v", ci, i, out[i], in[i])
			}
		}
	}
}
