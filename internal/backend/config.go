package backend

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/time/rate"
)

var reservedKeys = map[string]bool{
	"sabre":         true,
	"health":        true,
	"performance":   true,
	"subscriptions": true,
	"batch":         true,
	"cache":         true,
	"listen":        true,
	"max_attempts":  true,
}

// SabreConfig holds the main configuration for the Sabre load balancer.
type SabreConfig struct {
	Listen      string `toml:"listen"`
	MaxAttempts int    `toml:"max_attempts"`
}

// HealthConfig holds the configuration for health checks.
type HealthConfig struct {
	Enabled      bool          `toml:"enabled"`
	TTLCheck     time.Duration `toml:"ttl_check_ms"`
	Timeout      time.Duration `toml:"timeout_ms"`
	FailsToDown  int           `toml:"fails_to_down"`
	PassesToUp   int           `toml:"passes_to_up"`
	SampleMethod string        `toml:"method"`
}

// PerformanceConfig holds the configuration for HTTP/2 performance.
type PerformanceConfig struct {
	Timeout              time.Duration `toml:"timeout_ms"`
	Samples              int           `toml:"samples"`
	Gamma                float64       `toml:"gamma"`
	MaxIdleConns         int           `toml:"max_idle_conns"`
	MaxIdleConnsPerHost  int           `toml:"max_idle_conns_per_host"`
	IdleConnTimeout      time.Duration `toml:"idle_conn_timeout_ms"`
	DisableKeepAlives    bool          `toml:"disable_keep_alives"`
	EnableHTTP2          bool          `toml:"enable_http2"`
	MaxConcurrentStreams int           `toml:"max_concurrent_streams"`
	EnableCompression    bool          `toml:"enable_compression"`
	CompressionLevel     int           `toml:"compression_level"`
}

// SubscriptionsConfig holds the configuration for WebSocket subscriptions.
type SubscriptionsConfig struct {
	TTLBlock                      time.Duration `toml:"ttl_block_ms"`
	MaxConnectionsPerBackend      int           `toml:"max_connections_per_backend"`
	MaxSubscriptionsPerConnection int           `toml:"max_subscriptions_per_connection"`
	PingInterval                  time.Duration `toml:"ping_interval_ms"`
	PongWait                      time.Duration `toml:"pong_wait_ms"`
	WriteWait                     time.Duration `toml:"write_wait_ms"`
	ReadWait                      time.Duration `toml:"read_wait_ms"`
	EnableCompression             bool          `toml:"enable_compression"`
	MaxMessageSize                int64         `toml:"max_message_size"`
}

// BatchConfig holds the configuration for batching.
type BatchConfig struct {
	Enabled          bool          `toml:"enabled"`
	MaxBatchSize     int           `toml:"max_batch_size"`
	MaxBatchWaitTime time.Duration `toml:"max_batch_wait_ms"`
	MaxBatchWorkers  int           `toml:"max_batch_workers"`
}

type providerOnly struct {
	MaxPerSecond *int `toml:"max_per_second"`
}

type chainOnly struct {
	URL          string `toml:"url"`
	WSURL        string `toml:"ws_url"`
	MaxPerSecond *int   `toml:"max_per_second"`
}

// Config holds the complete configuration for the Sabre load balancer.
type Config struct {
	Sabre          SabreConfig
	Health         HealthConfig
	Performance    PerformanceConfig
	Subscriptions  SubscriptionsConfig
	Batch          BatchConfig
	Cache          CacheConfig
	Backends       []*Backend
	BackendsCt     map[string]int
	BatchProcessor *BatchProcessor
	HasWebSocket   bool
	Stream         *Stream
}

// ParseConfig reads and parses a TOML configuration file.
func ParseConfig(path string) Config {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("reading config file: %v", err)
	}

	var raw map[string]any
	expanded := os.ExpandEnv(string(b))
	if err := toml.Unmarshal([]byte(expanded), &raw); err != nil {
		log.Fatalf("parsing config file: %v", err)
	}

	// Check for backends after parsing all sections
	hasBackends := false
	for key := range raw {
		if !reservedKeys[key] {
			hasBackends = true
			break
		}
	}
	if !hasBackends {
		panic("no backends configured, at least one backend must be defined")
	}

	var cfg Config
	if s, ok := raw["sabre"]; ok {
		var sc SabreConfig
		raw, _ := toml.Marshal(s)
		_ = toml.Unmarshal(raw, &sc)
		cfg.Sabre = sc
	}

	// Handle top-level sabre config as well
	if listen, ok := raw["listen"]; ok {
		if listenStr, ok := listen.(string); ok {
			cfg.Sabre.Listen = listenStr
		}
	}
	if maxAttempts, ok := raw["max_attempts"]; ok {
		if maxAttemptsInt, ok := maxAttempts.(int64); ok {
			cfg.Sabre.MaxAttempts = int(maxAttemptsInt)
		}
	}

	if cfg.Sabre.Listen == "" {
		cfg.Sabre.Listen = ":3000"
	}

	if h, ok := raw["health"]; ok {
		var hc HealthConfig
		raw, _ := toml.Marshal(h)
		_ = toml.Unmarshal(raw, &hc)
		if hc.TTLCheck <= 0 {
			hc.TTLCheck = 1000 * time.Millisecond
		} else {
			hc.TTLCheck = hc.TTLCheck * time.Millisecond
		}
		if hc.Timeout <= 0 {
			hc.Timeout = 1500 * time.Millisecond
		} else {
			hc.Timeout = hc.Timeout * time.Millisecond
		}
		if hc.FailsToDown <= 0 {
			hc.FailsToDown = 2
		}
		if hc.PassesToUp <= 0 {
			hc.PassesToUp = 2
		}
		if hc.SampleMethod == "" {
			hc.SampleMethod = "eth_blockNumber"
		}

		cfg.Health = hc
	} else {
		// Set health defaults when section is missing
		cfg.Health = HealthConfig{
			Enabled:      true,
			TTLCheck:     1000 * time.Millisecond,
			Timeout:      1500 * time.Millisecond,
			FailsToDown:  2,
			PassesToUp:   2,
			SampleMethod: "eth_blockNumber",
		}
	}

	if p, ok := raw["performance"]; ok {
		var pc PerformanceConfig
		raw, _ := toml.Marshal(p)
		_ = toml.Unmarshal(raw, &pc)
		if pc.Timeout <= 0 {
			pc.Timeout = 2000 * time.Millisecond
		} else {
			pc.Timeout = pc.Timeout * time.Millisecond
		}
		if pc.Samples <= 0 {
			pc.Samples = 100
		}
		if pc.Gamma <= 0 {
			pc.Gamma = 0.9
		}

		if pc.MaxIdleConns <= 0 {
			pc.MaxIdleConns = 8192
		}
		if pc.MaxIdleConnsPerHost <= 0 {
			pc.MaxIdleConnsPerHost = 2048
		}
		if pc.IdleConnTimeout <= 0 {
			pc.IdleConnTimeout = 90 * time.Second
		} else {
			pc.IdleConnTimeout = pc.IdleConnTimeout * time.Millisecond
		}
		if pc.MaxConcurrentStreams <= 0 {
			pc.MaxConcurrentStreams = 250
		}
		if pc.CompressionLevel <= 0 {
			pc.CompressionLevel = 6
		}

		cfg.Performance = pc
	} else {
		// Set performance defaults when section is missing
		cfg.Performance = PerformanceConfig{
			Timeout:              2000 * time.Millisecond,
			Samples:              100,
			Gamma:                0.9,
			MaxIdleConns:         8192,
			MaxIdleConnsPerHost:  2048,
			IdleConnTimeout:      90 * time.Second,
			DisableKeepAlives:    false,
			EnableHTTP2:          true,
			MaxConcurrentStreams: 250,
			EnableCompression:    true,
			CompressionLevel:     6,
		}
	}

	if s, ok := raw["subscriptions"]; ok {
		var sc SubscriptionsConfig
		raw, _ := toml.Marshal(s)
		_ = toml.Unmarshal(raw, &sc)

		if sc.MaxConnectionsPerBackend <= 0 {
			sc.MaxConnectionsPerBackend = 100
		}
		if sc.MaxSubscriptionsPerConnection <= 0 {
			sc.MaxSubscriptionsPerConnection = 50
		}
		if sc.PingInterval <= 0 {
			sc.PingInterval = 30 * time.Second
		} else {
			sc.PingInterval = sc.PingInterval * time.Millisecond
		}
		if sc.PongWait <= 0 {
			sc.PongWait = 10 * time.Second
		} else {
			sc.PongWait = sc.PongWait * time.Millisecond
		}
		if sc.WriteWait <= 0 {
			sc.WriteWait = 10 * time.Second
		} else {
			sc.WriteWait = sc.WriteWait * time.Millisecond
		}
		if sc.ReadWait <= 0 {
			sc.ReadWait = 60 * time.Second
		} else {
			sc.ReadWait = sc.ReadWait * time.Millisecond
		}
		if sc.MaxMessageSize <= 0 {
			sc.MaxMessageSize = 1048576 // 1MB
		}
		if sc.TTLBlock <= 0 {
			sc.TTLBlock = 13000 * time.Millisecond
		} else {
			sc.TTLBlock = sc.TTLBlock * time.Millisecond
		}

		cfg.Subscriptions = sc
	} else {
		cfg.Subscriptions = SubscriptionsConfig{
			MaxConnectionsPerBackend:      100,
			MaxSubscriptionsPerConnection: 50,
			PingInterval:                  30 * time.Second,
			PongWait:                      10 * time.Second,
			WriteWait:                     10 * time.Second,
			ReadWait:                      60 * time.Second,
			EnableCompression:             true,
			MaxMessageSize:                1048576, // 1MB
			TTLBlock:                      13000 * time.Millisecond,
		}
	}

	if b, ok := raw["batch"]; ok {
		var bc BatchConfig
		raw, _ := toml.Marshal(b)
		_ = toml.Unmarshal(raw, &bc)

		if bc.MaxBatchSize <= 0 {
			bc.MaxBatchSize = 10
		}
		if bc.MaxBatchWaitTime <= 0 {
			bc.MaxBatchWaitTime = 50 * time.Millisecond
		} else {
			bc.MaxBatchWaitTime = bc.MaxBatchWaitTime * time.Millisecond
		}
		if bc.MaxBatchWorkers <= 0 {
			bc.MaxBatchWorkers = 4
		}

		cfg.Batch = bc

		if bc.Enabled {
			cfg.BatchProcessor = NewBatchProcessor(
				bc.MaxBatchSize,
				bc.MaxBatchWaitTime,
				bc.MaxBatchWorkers,
			)
		}
	} else {
		cfg.Batch = BatchConfig{
			Enabled:          false,
			MaxBatchSize:     10,
			MaxBatchWaitTime: 50 * time.Millisecond,
			MaxBatchWorkers:  4,
		}
	}

	if c, ok := raw["cache"]; ok {
		var cc CacheConfig
		raw, _ := toml.Marshal(c)
		_ = toml.Unmarshal(raw, &cc)
		if cc.Path == "" {
			cc.Path = "./.data/sabre"
		}
		if cc.MemEntries <= 0 {
			cc.MemEntries = 100_000
		}
		if cc.Enabled && cc.Path == "" {
			log.Fatal("cache is enabled but no path is configured")
		}
		if cc.TTLLatest <= 0 {
			cc.TTLLatest = 250 * time.Millisecond
		} else {
			cc.TTLLatest = cc.TTLLatest * time.Millisecond
		}
		if cc.TTLBlock <= 0 {
			cc.TTLBlock = 86_400_000 * time.Millisecond
		} else {
			cc.TTLBlock = cc.TTLBlock * time.Millisecond
		}

		if cc.MaxReorgDepth <= 0 {
			cc.MaxReorgDepth = 100
		}

		p := filepath.Clean(cc.Path)
		if p == "" || p == "." || p == "/" {
			panic(fmt.Sprintf("refusing to use unsafe cache path: %q", p))
		}

		if cc.Clean {
			if err := os.RemoveAll(p); err != nil {
				panic(fmt.Sprintf("failed to clean cache path %q: %v", p, err))
			}
		}

		if err := os.MkdirAll(p, 0755); err != nil {
			panic(fmt.Sprintf("failed to create cache path %q: %v", p, err))
		}
		cfg.Cache = cc
	} else {
		cfg.Cache = CacheConfig{
			Enabled:       false,
			Path:          "./.data/sabre",
			MemEntries:    100_000,
			TTLLatest:     250 * time.Millisecond,
			TTLBlock:      86_400_000 * time.Millisecond,
			Clean:         false,
			MaxReorgDepth: 100,
		}
	}

	for provName, v := range raw {
		if reservedKeys[provName] {
			continue
		}
		var p providerOnly
		x, _ := toml.Marshal(v)
		_ = toml.Unmarshal(x, &p)

		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		for chainName, vv := range vm {
			if chainName == "max_per_second" {
				continue
			}

			subMap, ok := vv.(map[string]any)
			if !ok {
				continue
			}

			var c chainOnly
			y, _ := toml.Marshal(subMap)
			if err := toml.Unmarshal(y, &c); err != nil {
				log.Fatalf("parsing chain config for %s: %v", provName, err)
			}
			if c.URL == "" {
				panic(fmt.Sprintf("backend %s chain %s has no URL configured", provName, chainName))
			}

			baseURL := c.URL
			baseWSURL := c.WSURL

			split := strings.Split(chainName, ",")
			names := make([]string, 0, len(split))
			for _, name := range split {
				c := strings.TrimSpace(name)
				if c != "" {
					names = append(names, c)
				}
			}

			var lim *rate.Limiter
			if c.MaxPerSecond != nil && *c.MaxPerSecond > 0 {
				lim = rate.NewLimiter(rate.Limit(*c.MaxPerSecond), 1)
			} else if p.MaxPerSecond != nil && *p.MaxPerSecond > 0 {
				lim = rate.NewLimiter(rate.Limit(*p.MaxPerSecond), 1)
			}

			tr := &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     cfg.Performance.EnableHTTP2,
				MaxIdleConns:          cfg.Performance.MaxIdleConns,
				MaxIdleConnsPerHost:   cfg.Performance.MaxIdleConnsPerHost,
				IdleConnTimeout:       cfg.Performance.IdleConnTimeout,
				TLSHandshakeTimeout:   4 * time.Second,
				ExpectContinueTimeout: 0,
				DisableCompression:    !cfg.Performance.EnableCompression,
				DisableKeepAlives:     cfg.Performance.DisableKeepAlives,
			}
			client := &http.Client{
				Transport: tr,
				Timeout:   30 * time.Second,
			}

			for _, chain := range names {
				chainURL := strings.ReplaceAll(baseURL, "CHAIN_NAME", chain)
				chainWSURL := strings.ReplaceAll(baseWSURL, "CHAIN_NAME", chain)

				u, err := url.Parse(chainURL)
				if err != nil {
					panic(fmt.Sprintf("parsing URL for backend %s chain %s: %v", provName, chain, err))
				}

				var wsu *url.URL
				if chainWSURL != "" {
					wsu, err = url.Parse(chainWSURL)
					if err != nil {
						panic(fmt.Sprintf("parsing WS URL for backend %s chain %s: %v", provName, chain, err))
					}
				}

				cfg.Backends = append(cfg.Backends, &Backend{
					Name:    provName,
					Chain:   chain,
					URL:     u,
					WSURL:   wsu,
					Client:  client,
					limiter: lim,
				})
				cfg.BackendsCt = appendCount(cfg.BackendsCt, chain)
			}
		}
	}

	if len(cfg.Backends) == 0 {
		panic("no backends configured")
	}

	if cfg.Sabre.MaxAttempts <= 0 {
		cfg.Sabre.MaxAttempts = len(cfg.Backends)
	}

	for i := range cfg.Backends {
		cfg.Backends[i].HealthUp.Store(true)
		cfg.Backends[i].HealthPassStreak.Store(0)
		cfg.Backends[i].HealthFailStreak.Store(0)
	}

	hasWebSocket := false
	for i := range cfg.Backends {
		if cfg.Backends[i].WSURL != nil {
			hasWebSocket = true
			break
		}
	}

	cfg.HasWebSocket = hasWebSocket

	return cfg
}

func appendCount(m map[string]int, k string) map[string]int {
	if m == nil {
		m = make(map[string]int)
	}
	m[k]++
	return m
}
