package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func getTestCachePath() string {
	cwd, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("failed to get working directory: %v", err))
	}

	if filepath.Base(cwd) == "backend" || filepath.Base(cwd) == "router" {
		cwd = filepath.Dir(filepath.Dir(cwd))
	}

	return filepath.Join(cwd, ".data", "sabre", "test")
}

func getUniqueTestCachePath(t *testing.T) string {
	basePath := getTestCachePath()
	return filepath.Join(basePath, t.Name())
}

func createUniqueTestCacheConfig(t *testing.T) CacheConfig {
	return CacheConfig{
		Enabled:       true,
		Path:          getUniqueTestCachePath(t),
		MemEntries:    1000,
		TTLLatest:     250 * time.Millisecond,
		TTLBlock:      24 * time.Hour,
		Clean:         true,
		MaxReorgDepth: 100,
	}
}

func cleanupTestCache(t *testing.T, cfg CacheConfig) {
	t.Helper()
	if err := os.RemoveAll(cfg.Path); err != nil {
		t.Logf("warning: failed to cleanup test cache: %v", err)
	}
}

func TestNewStore(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	defer cleanupTestCache(t, cfg)

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("expected no error creating store, got %v", err)
	}
	defer store.Close()

	if store == nil {
		t.Fatal("expected store to be created")
	}

	storeConfig := store.Config()
	if storeConfig.Enabled != cfg.Enabled {
		t.Errorf("expected enabled %v, got %v", cfg.Enabled, storeConfig.Enabled)
	}
	if storeConfig.Path != cfg.Path {
		t.Errorf("expected path %s, got %s", cfg.Path, storeConfig.Path)
	}
}

func TestStore_GetPut(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	defer cleanupTestCache(t, cfg)

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("expected no error creating store, got %v", err)
	}
	defer store.Close()

	key := "test-key"
	data := []byte("test-data")
	ttl := 1 * time.Second
	chainID := "ethereum"

	store.Put(key, data, ttl, chainID, 0, "")

	retrieved, found := store.Get(key)
	if !found {
		t.Error("expected data to be found")
	}
	if string(retrieved) != string(data) {
		t.Errorf("expected data %s, got %s", string(data), string(retrieved))
	}
}

func TestStore_TTLExpiration(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	defer cleanupTestCache(t, cfg)

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("expected no error creating store, got %v", err)
	}
	defer store.Close()

	key := "test-key"
	data := []byte("test-data")
	ttl := 10 * time.Millisecond
	chainID := "ethereum"

	store.Put(key, data, ttl, chainID, 0, "")
	retrieved, found := store.Get(key)
	if !found {
		t.Error("expected data to be found immediately")
	}
	if string(retrieved) != string(data) {
		t.Errorf("expected data %s, got %s", string(data), string(retrieved))
	}

	time.Sleep(20 * time.Millisecond)
	retrieved, found = store.Get(key)
	if found {
		t.Error("expected data to be expired")
	}
	if retrieved != nil {
		t.Error("expected nil data for expired entry")
	}
}

func TestStore_DisabledCache(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	cfg.Enabled = false
	defer cleanupTestCache(t, cfg)

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("expected no error creating store, got %v", err)
	}
	defer store.Close()

	key := "test-key"
	data := []byte("test-data")
	ttl := 1 * time.Second
	chainID := "ethereum"
	store.Put(key, data, ttl, chainID, 0, "")

	retrieved, found := store.Get(key)
	if found {
		t.Error("expected data not to be found when cache is disabled")
	}
	if retrieved != nil {
		t.Error("expected nil data when cache is disabled")
	}
}

func TestStore_BlockHashValidation(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	defer cleanupTestCache(t, cfg)

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("expected no error creating store, got %v", err)
	}
	defer store.Close()

	chainID := "ethereum"
	blockNum := uint64(12345)
	blockHash := "0x1234567890abcdef"

	blockData := []byte(`{"result":"0x1234567890abcdef"}`)
	store.UpdateLatestBlock(chainID, blockNum, blockHash, blockData)

	key := "test-key"
	data := []byte("test-data")
	ttl := 1 * time.Second

	store.Put(key, data, ttl, chainID, 0, "")

	_, found := store.Get(key)
	if !found {
		t.Error("expected data to be found with valid block hash")
	}

	invalidHash := "0xinvalid"
	store.UpdateLatestBlock(chainID, blockNum, invalidHash, blockData)

	_, found = store.Get(key)
	if found {
		t.Error("expected data to be purged with invalid block hash")
	}
}

func TestStore_ReorgDetection(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	defer cleanupTestCache(t, cfg)

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("expected no error creating store, got %v", err)
	}
	defer store.Close()

	chainID := "ethereum"
	blockNum := uint64(12345)
	originalHash := "0x1234567890abcdef"
	newHash := "0xfedcba0987654321"

	blockData := []byte(`{"result":"0x1234567890abcdef"}`)
	store.UpdateLatestBlock(chainID, blockNum, originalHash, blockData)
	store.UpdateLatestBlock(chainID, blockNum, newHash, blockData)

	key := "test-key"
	data := []byte("test-data")
	ttl := 1 * time.Second

	store.Put(key, data, ttl, chainID, 0, "")
	_, _ = store.Get(key)
}

func TestStore_MultipleChains(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	defer cleanupTestCache(t, cfg)

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("expected no error creating store, got %v", err)
	}
	defer store.Close()

	testCases := []struct {
		chainID string
		key     string
		data    []byte
	}{
		{"ethereum", "eth-key", []byte("ethereum-data")},
		{"base", "base-key", []byte("base-data")},
		{"polygon", "polygon-key", []byte("polygon-data")},
	}

	ttl := 1 * time.Second

	for _, tc := range testCases {
		store.Put(tc.key, tc.data, ttl, tc.chainID, 0, "")
	}

	for _, tc := range testCases {
		retrieved, found := store.Get(tc.key)
		if !found {
			t.Errorf("expected data to be found for chain %s", tc.chainID)
		}
		if string(retrieved) != string(tc.data) {
			t.Errorf("expected data %s for chain %s, got %s", string(tc.data), tc.chainID, string(retrieved))
		}
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	defer cleanupTestCache(t, cfg)

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("expected no error creating store, got %v", err)
	}
	defer store.Close()

	done := make(chan bool, 10)
	chainID := "ethereum"
	ttl := 1 * time.Second

	for i := range 5 {
		go func(id int) {
			key := fmt.Sprintf("concurrent-key-%d", id)
			data := fmt.Appendf(nil, "concurrent-data-%d", id)

			store.Put(key, data, ttl, chainID, 0, "")

			retrieved, found := store.Get(key)
			if !found {
				t.Errorf("expected data to be found for key %s", key)
			}
			if string(retrieved) != string(data) {
				t.Errorf("expected data %s, got %s", string(data), string(retrieved))
			}

			done <- true
		}(i)
	}

	for range 5 {
		<-done
	}
}

func TestStore_BlockHashValidationEdgeCases(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	defer cleanupTestCache(t, cfg)

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("expected no error creating store, got %v", err)
	}
	defer store.Close()

	chainID := "ethereum"

	key := "test-key"
	data := []byte("test-data")
	ttl := 1 * time.Second

	store.Put(key, data, ttl, chainID, 0, "")
	_, found := store.Get(key)
	if !found {
		t.Error("expected data to be found with empty block hash")
	}

	store.UpdateLatestBlock(chainID, 0, "0xhash", []byte("data"))
	_, found = store.Get(key)
	if !found {
		t.Error("expected data to be found with zero block number")
	}
}

func TestStore_CleanupOldBlockHashes(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	cfg.MaxReorgDepth = 5
	defer cleanupTestCache(t, cfg)

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("expected no error creating store, got %v", err)
	}
	defer store.Close()

	chainID := "ethereum"

	for i := uint64(1); i <= 20; i++ {
		hash := fmt.Sprintf("0xhash%d", i)
		blockData := fmt.Appendf(nil, `{"result":"%s"}`, hash)
		store.UpdateLatestBlock(chainID, i, hash, blockData)
	}

	key := "test-key"
	data := []byte("test-data")
	ttl := 1 * time.Second

	store.Put(key, data, ttl, chainID, 0, "")
	_, found := store.Get(key)
	if !found {
		t.Error("expected data to be found after cleanup")
	}
}

func TestStore_ZeroTTL(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	defer cleanupTestCache(t, cfg)

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("expected no error creating store, got %v", err)
	}
	defer store.Close()

	key := "test-key"
	data := []byte("test-data")
	ttl := 0 * time.Second
	chainID := "ethereum"

	store.Put(key, data, ttl, chainID, 0, "")

	retrieved, found := store.Get(key)
	if found {
		t.Error("expected data not to be found with zero TTL")
	}
	if retrieved != nil {
		t.Error("expected nil data with zero TTL")
	}
}

func TestStore_NegativeTTL(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	defer cleanupTestCache(t, cfg)

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("expected no error creating store, got %v", err)
	}
	defer store.Close()

	key := "test-key"
	data := []byte("test-data")
	ttl := -1 * time.Second
	chainID := "ethereum"

	store.Put(key, data, ttl, chainID, 0, "")
	retrieved, found := store.Get(key)
	if found {
		t.Error("expected data not to be found with negative TTL")
	}
	if retrieved != nil {
		t.Error("expected nil data with negative TTL")
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	cfg := createUniqueTestCacheConfig(t)
	t.Cleanup(func() { cleanupTestCache(t, cfg) })
	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func multicallParams(t *testing.T, blockTag string, calls []call3) json.RawMessage {
	t.Helper()
	callObj, _ := json.Marshal(map[string]string{
		"to":   "0xca11bde05977b3631167028862be2a173976ca11",
		"data": fmt.Sprintf("0x%x", encodeAggregate3(calls)),
	})
	tag, _ := json.Marshal(blockTag)
	params, _ := json.Marshal([]json.RawMessage{callObj, tag})
	return params
}

func multicallResponse(t *testing.T, results []multicallResult) []byte {
	t.Helper()
	body, _ := json.Marshal(rpcResult{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Result:  fmt.Sprintf("0x%x", encodeAggregate3Result(results)),
	})
	return body
}

var (
	callDecimals = call3{Target: hexToAddress("0xaa"), CallData: []byte{0x31, 0x3c, 0xe5, 0x67}}
	callSymbol   = call3{Target: hexToAddress("0xaa"), CallData: []byte{0x95, 0xd8, 0x9b, 0x41}}
	callName     = call3{Target: hexToAddress("0xaa"), CallData: []byte{0x06, 0xfd, 0xde, 0x03}}
	callSupply   = call3{Target: hexToAddress("0xaa"), CallData: []byte{0x18, 0x16, 0x0d, 0xdd}} // totalSupply
)

// TestStore_ImmutableMulticallAcrossBlocks is the cross-reset guarantee: once a
// metadata batch is cached, an identical-content multicall pinned to a
// different block is served from cache and carries the new request's id.
func TestStore_ImmutableMulticallAcrossBlocks(t *testing.T) {
	store := openTestStore(t)
	calls := []call3{callName, callSymbol, callDecimals}

	if _, ok := store.Lookup("1", "eth_call", multicallParams(t, "latest", calls), json.RawMessage(`1`), nil); ok {
		t.Fatal("expected a miss before the cache is populated")
	}

	name, symbol, decimals := []byte("Wrapped Ether"), []byte("WETH"), []byte{0x12}
	store.Store("1", "eth_call", multicallParams(t, "latest", calls),
		multicallResponse(t, []multicallResult{{true, name}, {true, symbol}, {true, decimals}}), nil)

	body, ok := store.Lookup("1", "eth_call", multicallParams(t, "0x1312d00", calls), json.RawMessage(`7`), nil)
	if !ok {
		t.Fatal("expected a hit across a different block tag")
	}

	var resp rpcResult
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal synthesized response: %v", err)
	}
	if string(resp.ID) != "7" {
		t.Fatalf("synthesized response must carry the request id, got %s", resp.ID)
	}
	results, err := decodeAggregate3Result(resp.Result)
	if err != nil {
		t.Fatalf("decode synthesized result: %v", err)
	}
	if len(results) != 3 || !bytes.Equal(results[0].ReturnData, name) ||
		!bytes.Equal(results[1].ReturnData, symbol) || !bytes.Equal(results[2].ReturnData, decimals) {
		t.Fatal("synthesized result must match the cached sub-calls")
	}
}

func TestStore_MulticallMutableNotCached(t *testing.T) {
	store := openTestStore(t)

	supply := []call3{callSupply}
	store.Store("1", "eth_call", multicallParams(t, "latest", supply),
		multicallResponse(t, []multicallResult{{true, []byte{0x64}}}), nil)
	if _, ok := store.Lookup("1", "eth_call", multicallParams(t, "latest", supply), json.RawMessage(`1`), nil); ok {
		t.Fatal("a mutable totalSupply read must never be served from cache")
	}

	store.Store("1", "eth_call", multicallParams(t, "latest", []call3{callDecimals}),
		multicallResponse(t, []multicallResult{{true, []byte{0x12}}}), nil)
	mixed := []call3{callDecimals, callSupply}
	if _, ok := store.Lookup("1", "eth_call", multicallParams(t, "latest", mixed), json.RawMessage(`1`), nil); ok {
		t.Fatal("a multicall with any mutable sub-call must not be served from cache")
	}
}

func TestStore_ImmutableMulticallSurvivesReorg(t *testing.T) {
	store := openTestStore(t)
	calls := []call3{callDecimals}
	store.Store("1", "eth_call", multicallParams(t, "latest", calls),
		multicallResponse(t, []multicallResult{{true, []byte{0x06}}}), nil)

	store.UpdateLatestBlock("1", 1000, "0xabc", nil)
	store.UpdateLatestBlock("1", 1000, "0xdifferent", nil) // reorg → in-memory purge

	if _, ok := store.Lookup("1", "eth_call", multicallParams(t, "latest", calls), json.RawMessage(`1`), nil); !ok {
		t.Fatal("immutable sub-calls must survive a reorg purge")
	}
}

func TestStore_StandaloneImmutableForeverCache(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	defer cleanupTestCache(t, cfg)
	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	chain := "1"
	token := "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"
	// eth_call decimals() at latest
	params := json.RawMessage(fmt.Sprintf(`[{"to":%q,"data":"0x313ce567"},"latest"]`, token))
	id := json.RawMessage(`1`)
	resp := []byte(`{"jsonrpc":"2.0","id":1,"result":"0x0000000000000000000000000000000000000000000000000000000000000006"}`)

	if _, ok := store.Lookup(chain, "eth_call", params, id, nil); ok {
		t.Fatal("expected miss before store")
	}
	store.Store(chain, "eth_call", params, resp, nil)

	// Served from forever-cache even past TTLLatest.
	time.Sleep(300 * time.Millisecond)
	got, ok := store.Lookup(chain, "eth_call", params, id, nil)
	if !ok {
		t.Fatal("expected hit after store (immortal, past TTLLatest)")
	}
	if !bytes.Contains(got, []byte("0x0000000000000000000000000000000000000000000000000000000000000006")) {
		t.Fatalf("wrong served body: %s", got)
	}

	// Shares the key with the multicall sub-call path: a standalone store warms
	// an aggregate3 read of the same (target, selector).
	calls, _ := decodeAggregate3Calls(hexToBytes("0x82ad56cb" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000001" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"000000000000000000000000a0b86991c6218b36c1d19d4a2e9eb0ce3606eb48" +
		"0000000000000000000000000000000000000000000000000000000000000001" +
		"0000000000000000000000000000000000000000000000000000000000000060" +
		"0000000000000000000000000000000000000000000000000000000000000004" +
		"313ce56700000000000000000000000000000000000000000000000000000000"))
	if _, ok := store.serveMulticall(chain, id, calls); !ok {
		t.Fatal("standalone store should warm the multicall sub-call cache (shared key)")
	}
}

func TestStore_StandaloneImmutableRevertNegativeCacheOnlyCode3(t *testing.T) {
	cfg := createUniqueTestCacheConfig(t)
	defer cleanupTestCache(t, cfg)
	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	chain := "1"
	mk := func(addr string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`[{"to":%q,"data":"0x313ce567"},"latest"]`, addr))
	}
	id := json.RawMessage(`1`)

	// Transient error must NOT poison.
	transient := "0x1111111111111111111111111111111111111111"
	store.Store(chain, "eth_call", mk(transient), []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"timeout"}}`), nil)
	if _, ok := store.Lookup(chain, "eth_call", mk(transient), id, nil); ok {
		t.Fatal("transient error must not be cached")
	}

	// Genuine revert (code 3) negative-caches and is served without upstream.
	reverter := "0x2222222222222222222222222222222222222222"
	store.Store(chain, "eth_call", mk(reverter), []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":3,"message":"execution reverted"}}`), nil)
	got, ok := store.Lookup(chain, "eth_call", mk(reverter), id, nil)
	if !ok {
		t.Fatal("code-3 revert should be negative-cached and served")
	}
	if !bytes.Contains(got, []byte("execution reverted")) {
		t.Fatalf("expected revert response, got %s", got)
	}
}
