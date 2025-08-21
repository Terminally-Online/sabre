package backend

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseConfig_DefaultValues(t *testing.T) {
	configContent := `
[sabre]
listen = ":3000"
max_attempts = 3

[performance]
timeout_ms = 2000
samples = 100
gamma = 0.9
max_idle_conns = 8192
max_idle_conns_per_host = 2048
idle_conn_timeout_ms = 90000
disable_keep_alives = false
enable_http2 = true
max_concurrent_streams = 250
enable_compression = true
compression_level = 6

[subscriptions]
ttl_block_ms = 13000
max_connections_per_backend = 100
max_subscriptions_per_connection = 50
ping_interval_ms = 30000
pong_wait_ms = 10000
write_wait_ms = 10000
read_wait_ms = 60000
enable_compression = true
max_message_size = 1048576

[batch]
enabled = true
max_batch_size = 10
max_batch_wait_ms = 50
max_batch_workers = 4

[health]
enabled = true
ttl_check_ms = 1000
timeout_ms = 1500
fails_to_down = 2
passes_to_up = 2
method = "eth_blockNumber"

[cache]
enabled = true
path = "./.data/sabre"
mem_entries = 100000
ttl_latest_ms = 250
ttl_block_ms = 86400000
max_reorg_depth = 100
clean = true

[quicknode.1]
url = "https://api.example.com"
ws_url = "wss://api.example.com"
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg := ParseConfig(configPath)

	if cfg.Sabre.Listen != ":3000" {
		t.Errorf("expected default listen address :3000, got %s", cfg.Sabre.Listen)
	}

	if cfg.Sabre.MaxAttempts != 3 {
		t.Errorf("expected default max attempts 3, got %d", cfg.Sabre.MaxAttempts)
	}

	if !cfg.Health.Enabled {
		t.Error("expected health to be enabled by default")
	}

	if cfg.Health.TTLCheck != 1000*time.Millisecond {
		t.Errorf("expected default TTL check 1000ms, got %v", cfg.Health.TTLCheck)
	}

	if cfg.Health.Timeout != 1500*time.Millisecond {
		t.Errorf("expected default timeout 1500ms, got %v", cfg.Health.Timeout)
	}

	if cfg.Health.FailsToDown != 2 {
		t.Errorf("expected default fails to down 2, got %d", cfg.Health.FailsToDown)
	}

	if cfg.Health.PassesToUp != 2 {
		t.Errorf("expected default passes to up 2, got %d", cfg.Health.PassesToUp)
	}

	if cfg.Health.SampleMethod != "eth_blockNumber" {
		t.Errorf("expected default sample method eth_blockNumber, got %s", cfg.Health.SampleMethod)
	}

	if cfg.Performance.Timeout != 2000*time.Millisecond {
		t.Errorf("expected default performance timeout 2000ms, got %v", cfg.Performance.Timeout)
	}

	if cfg.Performance.Samples != 100 {
		t.Errorf("expected default samples 100, got %d", cfg.Performance.Samples)
	}

	if cfg.Performance.Gamma != 0.9 {
		t.Errorf("expected default gamma 0.9, got %f", cfg.Performance.Gamma)
	}

	if cfg.Performance.MaxIdleConns != 8192 {
		t.Errorf("expected default max idle conns 8192, got %d", cfg.Performance.MaxIdleConns)
	}

	if cfg.Performance.MaxIdleConnsPerHost != 2048 {
		t.Errorf("expected default max idle conns per host 2048, got %d", cfg.Performance.MaxIdleConnsPerHost)
	}

	if cfg.Performance.IdleConnTimeout != 90*time.Second {
		t.Errorf("expected default idle conn timeout 90s, got %v", cfg.Performance.IdleConnTimeout)
	}

	if cfg.Performance.MaxConcurrentStreams != 250 {
		t.Errorf("expected default max concurrent streams 250, got %d", cfg.Performance.MaxConcurrentStreams)
	}

	if cfg.Performance.CompressionLevel != 6 {
		t.Errorf("expected default compression level 6, got %d", cfg.Performance.CompressionLevel)
	}

	if cfg.Subscriptions.MaxConnectionsPerBackend != 100 {
		t.Errorf("expected default max connections per backend 100, got %d", cfg.Subscriptions.MaxConnectionsPerBackend)
	}

	if cfg.Subscriptions.MaxSubscriptionsPerConnection != 50 {
		t.Errorf("expected default max subscriptions per connection 50, got %d", cfg.Subscriptions.MaxSubscriptionsPerConnection)
	}

	if cfg.Subscriptions.PingInterval != 30*time.Second {
		t.Errorf("expected default ping interval 30s, got %v", cfg.Subscriptions.PingInterval)
	}

	if cfg.Subscriptions.PongWait != 10*time.Second {
		t.Errorf("expected default pong wait 10s, got %v", cfg.Subscriptions.PongWait)
	}

	if cfg.Subscriptions.WriteWait != 10*time.Second {
		t.Errorf("expected default write wait 10s, got %v", cfg.Subscriptions.WriteWait)
	}

	if cfg.Subscriptions.ReadWait != 60*time.Second {
		t.Errorf("expected default read wait 60s, got %v", cfg.Subscriptions.ReadWait)
	}

	if cfg.Subscriptions.MaxMessageSize != 1048576 {
		t.Errorf("expected default max message size 1048576, got %d", cfg.Subscriptions.MaxMessageSize)
	}

	if cfg.Subscriptions.TTLBlock != 13000*time.Millisecond {
		t.Errorf("expected default TTL block 13000ms, got %v", cfg.Subscriptions.TTLBlock)
	}

	if cfg.Batch.MaxBatchSize != 10 {
		t.Errorf("expected default max batch size 10, got %d", cfg.Batch.MaxBatchSize)
	}

	if cfg.Batch.MaxBatchWaitTime != 50*time.Millisecond {
		t.Errorf("expected default max batch wait time 50ms, got %v", cfg.Batch.MaxBatchWaitTime)
	}

	if cfg.Batch.MaxBatchWorkers != 4 {
		t.Errorf("expected default max batch workers 4, got %d", cfg.Batch.MaxBatchWorkers)
	}

	if cfg.Cache.Path != "./.data/sabre" {
		t.Errorf("expected default cache path ./.data/sabre, got %s", cfg.Cache.Path)
	}

	if cfg.Cache.MemEntries != 100_000 {
		t.Errorf("expected default mem entries 100000, got %d", cfg.Cache.MemEntries)
	}

	if cfg.Cache.TTLLatest != 250*time.Millisecond {
		t.Errorf("expected default TTL latest 250ms, got %v", cfg.Cache.TTLLatest)
	}

	if cfg.Cache.TTLBlock != 86400000*time.Millisecond {
		t.Errorf("expected default TTL block 86400000ms, got %v", cfg.Cache.TTLBlock)
	}

	if cfg.Cache.MaxReorgDepth != 100 {
		t.Errorf("expected default max reorg depth 100, got %d", cfg.Cache.MaxReorgDepth)
	}
}

func TestParseConfig_CustomValues(t *testing.T) {
	configContent := `
[sabre]
listen = ":8080"
max_attempts = 5

[health]
enabled = false
ttl_check_ms = 2000
timeout_ms = 3000
fails_to_down = 3
passes_to_up = 1
method = "eth_chainId"

[performance]
timeout_ms = 5000
samples = 200
gamma = 0.8
max_idle_conns = 4096
max_idle_conns_per_host = 1024
idle_conn_timeout_ms = 120000
disable_keep_alives = true
enable_http2 = false
max_concurrent_streams = 100
enable_compression = false
compression_level = 9

[subscriptions]
ttl_block_ms = 15000
max_connections_per_backend = 50
max_subscriptions_per_connection = 25
ping_interval_ms = 60000
pong_wait_ms = 20000
write_wait_ms = 20000
read_wait_ms = 120000
enable_compression = false
max_message_size = 2097152

[batch]
enabled = true
max_batch_size = 20
max_batch_wait_ms = 100
max_batch_workers = 8

[cache]
enabled = true
path = "/tmp/sabre_cache"
mem_entries = 50000
ttl_latest_ms = 500
ttl_block_ms = 172800000
max_reorg_depth = 200
clean = true

[quicknode.1]
url = "https://api.example.com"
ws_url = "wss://api.example.com"
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg := ParseConfig(configPath)

	if cfg.Sabre.Listen != ":8080" {
		t.Errorf("expected custom listen address :8080, got %s", cfg.Sabre.Listen)
	}

	if cfg.Sabre.MaxAttempts != 5 {
		t.Errorf("expected custom max attempts 5, got %d", cfg.Sabre.MaxAttempts)
	}

	if cfg.Health.Enabled {
		t.Error("expected health to be disabled")
	}

	if cfg.Health.TTLCheck != 2000*time.Millisecond {
		t.Errorf("expected custom TTL check 2000ms, got %v", cfg.Health.TTLCheck)
	}

	if cfg.Health.Timeout != 3000*time.Millisecond {
		t.Errorf("expected custom timeout 3000ms, got %v", cfg.Health.Timeout)
	}

	if cfg.Health.FailsToDown != 3 {
		t.Errorf("expected custom fails to down 3, got %d", cfg.Health.FailsToDown)
	}

	if cfg.Health.PassesToUp != 1 {
		t.Errorf("expected custom passes to up 1, got %d", cfg.Health.PassesToUp)
	}

	if cfg.Health.SampleMethod != "eth_chainId" {
		t.Errorf("expected custom sample method eth_chainId, got %s", cfg.Health.SampleMethod)
	}

	if cfg.Performance.Timeout != 5000*time.Millisecond {
		t.Errorf("expected custom performance timeout 5000ms, got %v", cfg.Performance.Timeout)
	}

	if cfg.Performance.Samples != 200 {
		t.Errorf("expected custom samples 200, got %d", cfg.Performance.Samples)
	}

	if cfg.Performance.Gamma != 0.8 {
		t.Errorf("expected custom gamma 0.8, got %f", cfg.Performance.Gamma)
	}

	if cfg.Performance.MaxIdleConns != 4096 {
		t.Errorf("expected custom max idle conns 4096, got %d", cfg.Performance.MaxIdleConns)
	}

	if cfg.Performance.MaxIdleConnsPerHost != 1024 {
		t.Errorf("expected custom max idle conns per host 1024, got %d", cfg.Performance.MaxIdleConnsPerHost)
	}

	if cfg.Performance.IdleConnTimeout != 120*time.Second {
		t.Errorf("expected custom idle conn timeout 120s, got %v", cfg.Performance.IdleConnTimeout)
	}

	if !cfg.Performance.DisableKeepAlives {
		t.Error("expected keep alives to be disabled")
	}

	if cfg.Performance.EnableHTTP2 {
		t.Error("expected HTTP2 to be disabled")
	}

	if cfg.Performance.MaxConcurrentStreams != 100 {
		t.Errorf("expected custom max concurrent streams 100, got %d", cfg.Performance.MaxConcurrentStreams)
	}

	if cfg.Performance.EnableCompression {
		t.Error("expected compression to be disabled")
	}

	if cfg.Performance.CompressionLevel != 9 {
		t.Errorf("expected custom compression level 9, got %d", cfg.Performance.CompressionLevel)
	}

	if cfg.Subscriptions.TTLBlock != 15000*time.Millisecond {
		t.Errorf("expected custom TTL block 15000ms, got %v", cfg.Subscriptions.TTLBlock)
	}

	if cfg.Subscriptions.MaxConnectionsPerBackend != 50 {
		t.Errorf("expected custom max connections per backend 50, got %d", cfg.Subscriptions.MaxConnectionsPerBackend)
	}

	if cfg.Subscriptions.MaxSubscriptionsPerConnection != 25 {
		t.Errorf("expected custom max subscriptions per connection 25, got %d", cfg.Subscriptions.MaxSubscriptionsPerConnection)
	}

	if cfg.Subscriptions.PingInterval != 60*time.Second {
		t.Errorf("expected custom ping interval 60s, got %v", cfg.Subscriptions.PingInterval)
	}

	if cfg.Subscriptions.PongWait != 20*time.Second {
		t.Errorf("expected custom pong wait 20s, got %v", cfg.Subscriptions.PongWait)
	}

	if cfg.Subscriptions.WriteWait != 20*time.Second {
		t.Errorf("expected custom write wait 20s, got %v", cfg.Subscriptions.WriteWait)
	}

	if cfg.Subscriptions.ReadWait != 120*time.Second {
		t.Errorf("expected custom read wait 120s, got %v", cfg.Subscriptions.ReadWait)
	}

	if cfg.Subscriptions.EnableCompression {
		t.Error("expected subscriptions compression to be disabled")
	}

	if cfg.Subscriptions.MaxMessageSize != 2097152 {
		t.Errorf("expected custom max message size 2097152, got %d", cfg.Subscriptions.MaxMessageSize)
	}

	if !cfg.Batch.Enabled {
		t.Error("expected batch to be enabled")
	}

	if cfg.Batch.MaxBatchSize != 20 {
		t.Errorf("expected custom max batch size 20, got %d", cfg.Batch.MaxBatchSize)
	}

	if cfg.Batch.MaxBatchWaitTime != 100*time.Millisecond {
		t.Errorf("expected custom max batch wait time 100ms, got %v", cfg.Batch.MaxBatchWaitTime)
	}

	if cfg.Batch.MaxBatchWorkers != 8 {
		t.Errorf("expected custom max batch workers 8, got %d", cfg.Batch.MaxBatchWorkers)
	}

	if !cfg.Cache.Enabled {
		t.Error("expected cache to be enabled")
	}

	if cfg.Cache.Path != "/tmp/sabre_cache" {
		t.Errorf("expected custom cache path /tmp/sabre_cache, got %s", cfg.Cache.Path)
	}

	if cfg.Cache.MemEntries != 50000 {
		t.Errorf("expected custom mem entries 50000, got %d", cfg.Cache.MemEntries)
	}

	if cfg.Cache.TTLLatest != 500*time.Millisecond {
		t.Errorf("expected custom TTL latest 500ms, got %v", cfg.Cache.TTLLatest)
	}

	if cfg.Cache.TTLBlock != 172800000*time.Millisecond {
		t.Errorf("expected custom TTL block 172800000ms, got %v", cfg.Cache.TTLBlock)
	}

	if cfg.Cache.MaxReorgDepth != 200 {
		t.Errorf("expected custom max reorg depth 200, got %d", cfg.Cache.MaxReorgDepth)
	}

	if !cfg.Cache.Clean {
		t.Error("expected cache clean to be enabled")
	}
}

func TestParseConfig_BackendCreation(t *testing.T) {
	configContent := `
[quicknode]
max_per_second = 50
ethereum = { url = "https://api.example.com", ws_url = "wss://api.example.com" }

[alchemy]
max_per_second = 100
ethereum = { url = "https://eth-mainnet.alchemyapi.io/v2/KEY", ws_url = "wss://eth-mainnet.ws.alchemyapi.io/v2/KEY" }
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg := ParseConfig(configPath)

	if len(cfg.Backends) != 2 {
		t.Errorf("expected 2 backends, got %d", len(cfg.Backends))
	}

	foundQuicknode := false
	for _, backend := range cfg.Backends {
		if backend.Name == "quicknode" {
			foundQuicknode = true
			if backend.URL.String() != "https://api.example.com" {
				t.Errorf("expected quicknode URL https://api.example.com, got %s", backend.URL.String())
			}
			if backend.WSURL.String() != "wss://api.example.com" {
				t.Errorf("expected quicknode WS URL wss://api.example.com, got %s", backend.WSURL.String())
			}
			if backend.limiter == nil {
				t.Error("expected quicknode backend to have rate limiter")
			}
			break
		}
	}
	if !foundQuicknode {
		t.Error("quicknode backend not found")
	}

	foundAlchemy := false
	for _, backend := range cfg.Backends {
		if backend.Name == "alchemy" {
			foundAlchemy = true
			if backend.URL.String() != "https://eth-mainnet.alchemyapi.io/v2/KEY" {
				t.Errorf("expected alchemy URL https://eth-mainnet.alchemyapi.io/v2/KEY, got %s", backend.URL.String())
			}
			if backend.WSURL.String() != "wss://eth-mainnet.ws.alchemyapi.io/v2/KEY" {
				t.Errorf("expected alchemy WS URL wss://eth-mainnet.ws.alchemyapi.io/v2/KEY, got %s", backend.WSURL.String())
			}
			if backend.limiter == nil {
				t.Error("expected alchemy backend to have rate limiter")
			}
			break
		}
	}
	if !foundAlchemy {
		t.Error("alchemy backend not found")
	}

	if cfg.BackendsCt["ethereum"] != 2 {
		t.Errorf("expected 2 ethereum backends, got %d", cfg.BackendsCt["ethereum"])
	}

	for _, backend := range cfg.Backends {
		if !backend.HealthUp.Load() {
			t.Errorf("expected backend %s to be healthy by default", backend.Name)
		}
		if backend.HealthPassStreak.Load() != 0 {
			t.Errorf("expected backend %s pass streak to be 0, got %d", backend.Name, backend.HealthPassStreak.Load())
		}
		if backend.HealthFailStreak.Load() != 0 {
			t.Errorf("expected backend %s fail streak to be 0, got %d", backend.Name, backend.HealthFailStreak.Load())
		}
	}

	if !cfg.HasWebSocket {
		t.Error("expected WebSocket to be enabled")
	}
}

func TestParseConfig_ChainNameReplacement(t *testing.T) {
	configContent := `
[quicknode]
ethereum = { url = "https://api.example.com/CHAIN_NAME", ws_url = "wss://api.example.com/CHAIN_NAME" }

[multi]
"polygon,arbitrum" = { url = "https://api.example.com/CHAIN_NAME", ws_url = "wss://api.example.com/CHAIN_NAME" }
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg := ParseConfig(configPath)

	if len(cfg.Backends) != 3 {
		t.Errorf("expected 3 backends, got %d", len(cfg.Backends))
	}

	foundEthereum := false
	for _, backend := range cfg.Backends {
		if backend.Chain == "ethereum" {
			foundEthereum = true
			if backend.URL.String() != "https://api.example.com/ethereum" {
				t.Errorf("expected ethereum URL https://api.example.com/ethereum, got %s", backend.URL.String())
			}
			if backend.WSURL.String() != "wss://api.example.com/ethereum" {
				t.Errorf("expected ethereum WS URL wss://api.example.com/ethereum, got %s", backend.WSURL.String())
			}
			break
		}
	}
	if !foundEthereum {
		t.Error("ethereum backend not found")
	}

	foundPolygon := false
	for _, backend := range cfg.Backends {
		if backend.Chain == "polygon" {
			foundPolygon = true
			if backend.URL.String() != "https://api.example.com/polygon" {
				t.Errorf("expected polygon URL https://api.example.com/polygon, got %s", backend.URL.String())
			}
			if backend.WSURL.String() != "wss://api.example.com/polygon" {
				t.Errorf("expected polygon WS URL wss://api.example.com/polygon, got %s", backend.WSURL.String())
			}
			break
		}
	}
	if !foundPolygon {
		t.Error("polygon backend not found")
	}

	foundArbitrum := false
	for _, backend := range cfg.Backends {
		if backend.Chain == "arbitrum" {
			foundArbitrum = true
			if backend.URL.String() != "https://api.example.com/arbitrum" {
				t.Errorf("expected arbitrum URL https://api.example.com/arbitrum, got %s", backend.URL.String())
			}
			if backend.WSURL.String() != "wss://api.example.com/arbitrum" {
				t.Errorf("expected arbitrum WS URL wss://api.example.com/arbitrum, got %s", backend.WSURL.String())
			}
			break
		}
	}
	if !foundArbitrum {
		t.Error("arbitrum backend not found")
	}
}

func TestParseConfig_EnvironmentVariableExpansion(t *testing.T) {
	os.Setenv("API_URL", "https://api.example.com")
	os.Setenv("WS_URL", "wss://api.example.com")
	os.Setenv("API_KEY", "test-key")
	defer os.Unsetenv("API_URL")
	defer os.Unsetenv("WS_URL")
	defer os.Unsetenv("API_KEY")

	configContent := `
[sabre]
listen = "${API_URL}:3000"

[quicknode]
ethereum = { url = "${API_URL}/v1/${API_KEY}", ws_url = "${WS_URL}/v1/${API_KEY}" }
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg := ParseConfig(configPath)

	if cfg.Sabre.Listen != "https://api.example.com:3000" {
		t.Errorf("expected expanded listen address https://api.example.com:3000, got %s", cfg.Sabre.Listen)
	}

	if len(cfg.Backends) != 1 {
		t.Errorf("expected 1 backend, got %d", len(cfg.Backends))
	}

	backend := cfg.Backends[0]
	if backend.URL.String() != "https://api.example.com/v1/test-key" {
		t.Errorf("expected expanded URL https://api.example.com/v1/test-key, got %s", backend.URL.String())
	}

	if backend.WSURL.String() != "wss://api.example.com/v1/test-key" {
		t.Errorf("expected expanded WS URL wss://api.example.com/v1/test-key, got %s", backend.WSURL.String())
	}
}

func TestParseConfig_BatchProcessorCreation(t *testing.T) {
	configContent := `
[batch]
enabled = true
max_batch_size = 15
max_batch_wait_ms = 75
max_batch_workers = 6

[quicknode]
ethereum = { url = "https://api.example.com" }
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg := ParseConfig(configPath)

	if cfg.BatchProcessor == nil {
		t.Fatal("expected batch processor to be created")
	}

	if !cfg.Batch.Enabled {
		t.Error("expected batch to be enabled")
	}
}

func TestParseConfig_NoBackends(t *testing.T) {
	configContent := `
[sabre]
listen = ":3000"
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when no backends are configured")
		}
	}()

	ParseConfig(configPath)
}

func TestParseConfig_InvalidURL(t *testing.T) {
	configContent := `
[quicknode]
ethereum = { url = "://invalid-url" }
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when URL is invalid")
		}
	}()

	ParseConfig(configPath)
}

func TestParseConfig_InvalidWSURL(t *testing.T) {
	configContent := `
[quicknode]
ethereum = { url = "https://api.example.com", ws_url = "://invalid-ws-url" }
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when WS URL is invalid")
		}
	}()

	ParseConfig(configPath)
}

func TestParseConfig_MissingURL(t *testing.T) {
	configContent := `
[quicknode]
ethereum = { ws_url = "wss://api.example.com" }
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when URL is missing")
		}
	}()

	ParseConfig(configPath)
}

func TestParseConfig_CacheClean(t *testing.T) {
	tmpDir := t.TempDir()
	cachePath := filepath.Join(tmpDir, "cache")

	err := os.MkdirAll(cachePath, 0755)
	if err != nil {
		t.Fatalf("failed to create cache directory: %v", err)
	}

	err = os.WriteFile(filepath.Join(cachePath, "test.txt"), []byte("test"), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	configContent := `
[cache]
enabled = true
path = "` + cachePath + `"
clean = true

[quicknode]
ethereum = { url = "https://api.example.com" }
`

	configPath := filepath.Join(tmpDir, "config.toml")
	err = os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg := ParseConfig(configPath)

	if !cfg.Cache.Enabled {
		t.Error("expected cache to be enabled")
	}

	if cfg.Cache.Path != cachePath {
		t.Errorf("expected cache path %s, got %s", cachePath, cfg.Cache.Path)
	}

	if _, err := os.Stat(filepath.Join(cachePath, "test.txt")); err == nil {
		t.Error("expected test file to be removed during cache clean")
	}

	if _, err := os.Stat(cachePath); err != nil {
		t.Error("expected cache directory to exist after clean")
	}
}

func TestParseConfig_UnsafeCachePath(t *testing.T) {
	configContent := `
[cache]
enabled = true
path = "/"
clean = true

[quicknode]
ethereum = { url = "https://api.example.com" }
`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when cache path is unsafe")
		}
	}()

	ParseConfig(configPath)
}

func TestAppendCount(t *testing.T) {
	result := appendCount(nil, "test")
	if result == nil {
		t.Error("expected non-nil map")
	}
	if result["test"] != 1 {
		t.Errorf("expected count 1, got %d", result["test"])
	}

	existing := map[string]int{"test": 5, "other": 10}
	result = appendCount(existing, "test")
	if result["test"] != 6 {
		t.Errorf("expected count 6, got %d", result["test"])
	}
	if result["other"] != 10 {
		t.Errorf("expected other count 10, got %d", result["other"])
	}

	result = appendCount(existing, "new")
	if result["new"] != 1 {
		t.Errorf("expected new count 1, got %d", result["new"])
	}
}
