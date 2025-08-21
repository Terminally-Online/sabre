package backend

import (
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

	store.Put(key, data, ttl, chainID)

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

	store.Put(key, data, ttl, chainID)
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
	store.Put(key, data, ttl, chainID)

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

	store.Put(key, data, ttl, chainID)

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

	store.Put(key, data, ttl, chainID)
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
		store.Put(tc.key, tc.data, ttl, tc.chainID)
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

			store.Put(key, data, ttl, chainID)

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

	store.Put(key, data, ttl, chainID)
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

	store.Put(key, data, ttl, chainID)
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

	store.Put(key, data, ttl, chainID)

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

	store.Put(key, data, ttl, chainID)
	retrieved, found := store.Get(key)
	if found {
		t.Error("expected data not to be found with negative TTL")
	}
	if retrieved != nil {
		t.Error("expected nil data with negative TTL")
	}
}
