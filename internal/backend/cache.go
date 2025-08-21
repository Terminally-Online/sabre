package backend

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"
)

// CacheConfig holds configuration for the cache store.
type CacheConfig struct {
	Enabled       bool          `toml:"enabled"`
	Path          string        `toml:"path"`
	MemEntries    int           `toml:"mem_entries"`
	TTLLatest     time.Duration `toml:"ttl_latest_ms"`
	TTLBlock      time.Duration `toml:"ttl_block_ms"`
	Clean         bool          `toml:"clean"`
	MaxReorgDepth int           `toml:"max_reorg_depth"`
}

// Store provides persistent caching with TTL support and block hash validation.
type Store struct {
	cfg          CacheConfig
	db           *pebble.DB
	mem          *lru.Cache[string, entry]
	sf           singleflight.Group
	mu           sync.RWMutex
	blockHashes  map[string]map[uint64]string
	latestBlocks map[string]uint64
	latestHashes map[string]string
	reorgChecker *time.Ticker
}

// Config returns the current cache configuration.
func (s *Store) Config() CacheConfig {
	return s.cfg
}

// Close closes the cache store and releases all resources.
func (s *Store) Close() error {
	if s.reorgChecker != nil {
		s.reorgChecker.Stop()
	}
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// UpdateLatestBlock updates the latest block information for chain reorg detection.
func (s *Store) UpdateLatestBlock(chainID string, blockNum uint64, blockHash string, blockData []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.blockHashes == nil {
		s.blockHashes = make(map[string]map[uint64]string)
	}
	if s.latestBlocks == nil {
		s.latestBlocks = make(map[string]uint64)
	}
	if s.latestHashes == nil {
		s.latestHashes = make(map[string]string)
	}
	if s.blockHashes[chainID] == nil {
		s.blockHashes[chainID] = make(map[uint64]string)
	}

	reorgDetected := s.detectReorg(chainID, blockNum, blockHash, blockData)
	if reorgDetected && s.mem != nil {
		s.mem.Purge()
	}

	s.blockHashes[chainID][blockNum] = blockHash
	if blockNum > s.latestBlocks[chainID] {
		s.latestBlocks[chainID] = blockNum
		s.latestHashes[chainID] = blockHash
	}

	s.cleanupOldBlockHashes(chainID)
}

func (s *Store) detectReorg(chainID string, newBlockNum uint64, newBlockHash string, blockData []byte) bool {
	if existingHash, exists := s.blockHashes[chainID][newBlockNum]; exists {
		if existingHash != newBlockHash {
			return true
		}
	}

	if newBlockNum > 0 && newBlockNum <= s.latestBlocks[chainID] {
		if existingHash, exists := s.blockHashes[chainID][newBlockNum]; exists {
			if existingHash != newBlockHash {
				return true
			}
		}
	}

	if newBlockNum > 1 && blockData != nil {
		parentBlockNum := newBlockNum - 1
		if parentHash, exists := s.blockHashes[chainID][parentBlockNum]; exists {
			if newBlockParentHash := extractParentHashFromBlockData(blockData); newBlockParentHash != "" {
				if newBlockParentHash != parentHash {
					return true
				}
			}
		}
	}

	return false
}

func (s *Store) cleanupOldBlockHashes(chainID string) {
	chainHashes := s.blockHashes[chainID]
	if len(chainHashes) <= s.cfg.MaxReorgDepth*2 {
		return
	}

	cutoff := s.latestBlocks[chainID] - uint64(s.cfg.MaxReorgDepth*2)
	for blockNum := range chainHashes {
		if blockNum < cutoff {
			delete(chainHashes, blockNum)
		}
	}
}

// IsBlockHashValid checks if a cached block hash is still valid (no reorg detected).
func (s *Store) IsBlockHashValid(chainID string, entryBlockHash string, entryBlockNum uint64) bool {
	if entryBlockHash == "" {
		return true
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if chainHashes, exists := s.blockHashes[chainID]; exists {
		if currentHash, exists := chainHashes[entryBlockNum]; exists {
			return currentHash == entryBlockHash
		}
	}

	if s.latestBlocks[chainID] > 0 && s.latestBlocks[chainID]-entryBlockNum <= uint64(s.cfg.MaxReorgDepth) {
		return false
	}

	return true
}

// Get retrieves a value from the cache by key.
func (s *Store) Get(key string) ([]byte, bool) {
	if !s.cfg.Enabled {
		return nil, false
	}
	now := time.Now().UnixMilli()

	if e, ok := s.mem.Get(key); ok && e.Expiry >= now {
		if s.IsBlockHashValid(e.ChainID, e.BlockHash, e.BlockNum) {
			return e.Body, true
		}
		s.mem.Remove(key)
	}

	if s.db == nil {
		return nil, false
	}
	val, closer, err := s.db.Get([]byte(key))
	if err != nil {
		return nil, false
	}
	defer closer.Close()

	var e entry
	if json.Unmarshal(val, &e) == nil && e.Expiry >= now {
		if s.IsBlockHashValid(e.ChainID, e.BlockHash, e.BlockNum) {
			s.mem.Add(key, e)
			return e.Body, true
		}
		_ = s.db.Delete([]byte(key), pebble.Sync)
	}

	return nil, false
}

// Put stores a value in the cache with the specified TTL and chain ID.
func (s *Store) Put(key string, body []byte, ttl time.Duration, chainID string) {
	if !s.cfg.Enabled || ttl <= 0 {
		return
	}
	exp := time.Now().Add(ttl).UnixMilli()

	var blockHash string
	var blockNum uint64
	s.mu.RLock()
	blockHash = s.latestHashes[chainID]
	blockNum = s.latestBlocks[chainID]
	s.mu.RUnlock()

	e := entry{
		Expiry:    exp,
		Body:      body,
		ChainID:   chainID,
		BlockHash: blockHash,
		BlockNum:  blockNum,
	}

	if s.mem != nil {
		s.mem.Add(key, e)
	}
	if s.db != nil {
		buf, _ := json.Marshal(e)
		_ = s.db.Set([]byte(key), buf, pebble.Sync)
	}
}

// Do executes a function and caches the result if the key is not already cached.
func (s *Store) Do(key string, fn func() ([]byte, error)) ([]byte, error) {
	if b, ok := s.Get(key); ok {
		return b, nil
	}
	v, err, _ := s.sf.Do(key, func() (any, error) {
		if b, ok := s.Get(key); ok {
			return b, nil
		}
		return fn()
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

type entry struct {
	Expiry    int64  `json:"exp"`
	Body      []byte `json:"body"`
	ChainID   string `json:"chain_id,omitempty"`
	BlockHash string `json:"block_hash,omitempty"`
	BlockNum  uint64 `json:"block_num,omitempty"`
}

// Open creates and initializes a new cache store with the given configuration.
func Open(cfg CacheConfig) (*Store, error) {
	if cfg.MemEntries <= 0 {
		cfg.MemEntries = 100_000
	}
	if cfg.TTLLatest <= 0 {
		cfg.TTLLatest = 250 * time.Millisecond
	}
	if cfg.TTLBlock <= 0 {
		cfg.TTLBlock = 24 * time.Hour
	}

	if cfg.MaxReorgDepth <= 0 {
		cfg.MaxReorgDepth = 100
	}

	if !cfg.Enabled {
		return &Store{cfg: cfg}, nil
	}
	db, err := pebble.Open(cfg.Path, &pebble.Options{})
	if err != nil {
		return nil, err
	}
	mem, _ := lru.New[string, entry](cfg.MemEntries)
	return &Store{db: db, mem: mem, cfg: cfg}, nil
}

// CanonicalKey generates a canonical cache key for a JSON-RPC request.
func CanonicalKey(chainID, method string, params json.RawMessage) (string, error) {
	h := sha256.New()
	h.Write([]byte(chainID))
	h.Write([]byte{0})
	h.Write([]byte(method))
	h.Write([]byte{0})
	var anyParams any
	if len(params) > 0 {
		if err := json.Unmarshal(params, &anyParams); err != nil {
			return "", err
		}
		min, _ := json.Marshal(anyParams)
		h.Write(min)
	}
	return fmt.Sprintf("v1:%x", h.Sum(nil)), nil
}

// TTL determines the appropriate time-to-live for caching a specific RPC method.
func TTL(method string, params json.RawMessage, cfg CacheConfig, subsCfg *SubscriptionsConfig) time.Duration {
	switch method {
	case "eth_getBlockByNumber", "eth_getBalance", "eth_getCode", "eth_getStorageAt", "eth_call":
		if isBlockNumberTag(params) {
			if subsCfg != nil && subsCfg.TTLBlock > 0 {
				return subsCfg.TTLBlock
			}
			return cfg.TTLBlock
		}
		return cfg.TTLLatest
	case "eth_getBlockByHash", "eth_getTransactionByHash", "eth_getTransactionReceipt":
		return cfg.TTLLatest
	case "eth_getLogs":
		if logsNumericRange(params) {
			if subsCfg != nil && subsCfg.TTLBlock > 0 {
				return subsCfg.TTLBlock
			}
			return cfg.TTLBlock
		}
		return cfg.TTLLatest
	default:
		// NOTE: Do not cache mutating or tip-sensitive methods: sendRawTransaction,
		//       estimateGas, fee data, pending, subscriptions, etc.
		return 0
	}
}

var numRegex = regexp.MustCompile(`"[0-9]+"`)

func isBlockNumberTag(params json.RawMessage) bool {
	lower := bytes.ToLower(params)

	if bytes.Contains(lower, []byte(`"latest"`)) ||
		bytes.Contains(lower, []byte(`"pending"`)) ||
		bytes.Contains(lower, []byte(`"safe"`)) ||
		bytes.Contains(lower, []byte(`"finalized"`)) ||
		bytes.Contains(lower, []byte(`"earliest"`)) {
		return false
	}

	return bytes.Contains(lower, []byte(`"0x`)) || numRegex.Match(lower)
}

func logsNumericRange(params json.RawMessage) bool {
	low := bytes.Contains(params, []byte(`"fromBlock":"`))
	hi := bytes.Contains(params, []byte(`"toBlock":"`))
	return low && hi
}

// ExtractBlockInfo extracts block number and hash from a JSON-RPC response.
func ExtractBlockInfo(response []byte) (blockNum uint64, blockHash string) {
	var resp struct {
		Result json.RawMessage `json:"result"`
	}

	if err := json.Unmarshal(response, &resp); err != nil {
		return 0, ""
	}

	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return 0, ""
	}

	if num, ok := result["number"].(string); ok {
		if strings.HasPrefix(num, "0x") {
			if parsed, err := strconv.ParseUint(num[2:], 16, 64); err == nil {
				blockNum = parsed
			}
		}
	}

	if hash, ok := result["hash"].(string); ok {
		blockHash = hash
	}

	return blockNum, blockHash
}

func extractParentHashFromBlockData(blockData []byte) string {
	var resp struct {
		Result json.RawMessage `json:"result"`
	}

	if err := json.Unmarshal(blockData, &resp); err != nil {
		return ""
	}

	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return ""
	}

	if parentHash, ok := result["parentHash"].(string); ok {
		return parentHash
	}

	return ""
}

// CheckReorgFromHealthResponse checks for chain reorgs using health response data.
func (s *Store) CheckReorgFromHealthResponse(response []byte, chainID string) {
	if blockNum, blockHash := ExtractBlockInfo(response); blockNum > 0 {
		s.UpdateLatestBlock(chainID, blockNum, blockHash, response)
	}
}
