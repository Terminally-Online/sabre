package backend

import (
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
	"rapier":      true,
	"health":      true,
	"performance": true,
	"batch":       true,
	"cache":       true,
}

type RapierConfig struct {
	Listen      string `toml:"listen"`
	MaxAttempts int    `toml:"max_attempts"`
}

type HealthConfig struct {
	Enabled      bool          `toml:"enabled"`
	TTLCheck     time.Duration `toml:"ttl_check_ms"`
	Timeout      time.Duration `toml:"timeout_ms"`
	FailsToDown  int           `toml:"fails_to_down"`
	PassesToUp   int           `toml:"passes_to_up"`
	SampleMethod string        `toml:"method"`
}

type PerformanceConfig struct {
	Timeout              time.Duration `toml:"timeout_ms"`
	TTLBlock             time.Duration `toml:"ttl_block_ms"`
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

type Config struct {
	Rapier         RapierConfig
	Health         HealthConfig
	Performance    PerformanceConfig
	Batch          BatchConfig
	Cache          CacheConfig
	Backends       []Backend
	BackendsCt     map[string]int
	BatchProcessor *BatchProcessor
}

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

	if len(raw) <= len(reservedKeys) {
		log.Fatal("no backends configured, at least one backend must be defined")
	}

	var cfg Config
	if s, ok := raw["rapier"]; ok {
		var sc RapierConfig
		raw, _ := toml.Marshal(s)
		_ = toml.Unmarshal(raw, &sc)
		cfg.Rapier = sc
	}
	if cfg.Rapier.Listen == "" {
		cfg.Rapier.Listen = ":3000"
	}

	if h, ok := raw["health"]; ok {
		var hc HealthConfig
		raw, _ := toml.Marshal(h)
		_ = toml.Unmarshal(raw, &hc)
		if hc.TTLCheck <= 0 {
			hc.TTLCheck = 1000
		}
		if hc.Timeout <= 0 {
			hc.Timeout = 1500
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
	}

	if p, ok := raw["performance"]; ok {
		var pc PerformanceConfig
		raw, _ := toml.Marshal(p)
		_ = toml.Unmarshal(raw, &pc)
		if pc.Timeout <= 0 {
			pc.Timeout = 2000
		}
		if pc.TTLBlock <= 0 {
			pc.TTLBlock = 13000
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
		}
		if pc.MaxConcurrentStreams <= 0 {
			pc.MaxConcurrentStreams = 250
		}
		if pc.CompressionLevel <= 0 {
			pc.CompressionLevel = 6
		}

		cfg.Performance = pc
	}

	if b, ok := raw["batch"]; ok {
		var bc BatchConfig
		raw, _ := toml.Marshal(b)
		_ = toml.Unmarshal(raw, &bc)

		// Set batching defaults
		if bc.MaxBatchSize <= 0 {
			bc.MaxBatchSize = 10
		}
		if bc.MaxBatchWaitTime <= 0 {
			bc.MaxBatchWaitTime = 50 * time.Millisecond
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
	}

	if c, ok := raw["cache"]; ok {
		var cc CacheConfig
		raw, _ := toml.Marshal(c)
		_ = toml.Unmarshal(raw, &cc)
		if cc.Path == "" {
			cc.Path = "./.data/rapier"
		}
		if cc.MemEntries <= 0 {
			cc.MemEntries = 100_000
		}
		if cc.Enabled && cc.Path == "" {
			log.Fatal("cache is enabled but no path is configured")
		}
		if cc.TTLLatest <= 0 {
			cc.TTLLatest = 250
		}
		if cc.TTLBlock <= 0 {
			cc.TTLBlock = 86_400_000
		}

		// Set re-org protection defaults
		if cc.MaxReorgDepth <= 0 {
			cc.MaxReorgDepth = 100
		}

		if cc.Clean {
			p := filepath.Clean(cc.Path)
			if p == "" || p == "." || p == "/" {
				log.Fatalf("refusing to clean unsafe cache path: %q", p)
			}
			if err := os.RemoveAll(p); err != nil {
				log.Fatalf("failed to clean cache path %q: %v", p, err)
			}
			if err := os.MkdirAll(p, 0755); err != nil {
				log.Fatalf("failed to create cache path %q: %v", p, err)
			}
		}
		cfg.Cache = cc
	} else {
		cfg.Cache = CacheConfig{
			Enabled: false,
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
				log.Fatalf("backend %s chain %s has no URL configured", provName, chainName)
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
					log.Fatalf("parsing URL for backend %s chain %s: %v", provName, chain, err)
				}

				var wsu *url.URL
				if chainWSURL != "" {
					wsu, err = url.Parse(chainWSURL)
					if err != nil {
						log.Fatalf("parsing WS URL for backend %s chain %s: %v", provName, chain, err)
					}
				}

				cfg.Backends = append(cfg.Backends, Backend{
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
		log.Fatal("no backends configured")
	}

	if cfg.Rapier.MaxAttempts <= 0 {
		cfg.Rapier.MaxAttempts = len(cfg.Backends)
	}

	for i := range cfg.Backends {
		cfg.Backends[i].HealthUp.Store(true)
		cfg.Backends[i].HealthPassStreak.Store(0)
		cfg.Backends[i].HealthFailStreak.Store(0)
	}

	return cfg
}

func appendCount(m map[string]int, k string) map[string]int {
	if m == nil {
		m = make(map[string]int)
	}
	m[k]++
	return m
}
