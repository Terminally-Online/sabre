package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func createTestBackend(name, chain, urlStr string) *Backend {
	return CreateMockBackend(name, chain, urlStr)
}

func createTestConfig() Config {
	backend1 := createTestBackend("backend1", "ethereum", "https://api1.example.com")
	backend2 := createTestBackend("backend2", "ethereum", "https://api2.example.com")
	backend3 := createTestBackend("backend3", "base", "https://api3.example.com")

	backend1.HealthUp.Store(true)
	backend2.HealthUp.Store(true)
	backend3.HealthUp.Store(true)

	return Config{
		Sabre: SabreConfig{
			Listen:      ":3000",
			MaxAttempts: 3,
		},
		Health: HealthConfig{
			Enabled:     true,
			TTLCheck:    1000 * time.Millisecond,
			Timeout:     1500 * time.Millisecond,
			FailsToDown: 2,
			PassesToUp:  2,
		},
		Performance: PerformanceConfig{
			Timeout:              2000 * time.Millisecond,
			Samples:              100,
			Gamma:                0.9,
			MaxIdleConns:         8192,
			MaxIdleConnsPerHost:  2048,
			IdleConnTimeout:      90000 * time.Millisecond,
			DisableKeepAlives:    false,
			EnableHTTP2:          true,
			MaxConcurrentStreams: 250,
			EnableCompression:    true,
			CompressionLevel:     6,
		},
		Backends: []*Backend{backend1, backend2, backend3},
		BackendsCt: map[string]int{
			"ethereum": 2,
			"base":     1,
		},
	}
}

func TestNewLoadBalancer(t *testing.T) {
	cfg := createTestConfig()
	lb := NewLoadBalancer(cfg)

	if lb == nil {
		t.Fatal("expected load balancer to be created")
	}

	ethereumBackends := lb.GetBackends("ethereum")
	if len(ethereumBackends) != 2 {
		t.Errorf("expected 2 ethereum backends, got %d", len(ethereumBackends))
	}

	baseBackends := lb.GetBackends("base")
	if len(baseBackends) != 1 {
		t.Errorf("expected 1 base backend, got %d", len(baseBackends))
	}

	ethereumWeights := lb.GetWeights("ethereum")
	if len(ethereumWeights) != 2 {
		t.Errorf("expected 2 ethereum weights, got %d", len(ethereumWeights))
	}

	for i, weight := range ethereumWeights {
		if weight != 1.0 {
			t.Errorf("expected initial weight 1.0 for backend %d, got %f", i, weight)
		}
	}
}

func TestLoadBalancer_Pick(t *testing.T) {
	cfg := createTestConfig()
	lb := NewLoadBalancer(cfg)

	ctx := context.Background()

	backend, err := lb.Pick(ctx, "ethereum", "http")
	if err != nil {
		t.Errorf("expected no error picking ethereum backend, got %v", err)
	}
	if backend == nil {
		t.Fatal("expected backend to be returned")
	}
	if backend.Chain != "ethereum" {
		t.Errorf("expected ethereum backend, got %s", backend.Chain)
	}

	backend, err = lb.Pick(ctx, "base", "http")
	if err != nil {
		t.Errorf("expected no error picking base backend, got %v", err)
	}
	if backend == nil {
		t.Fatal("expected backend to be returned")
	}
	if backend.Chain != "base" {
		t.Errorf("expected base backend, got %s", backend.Chain)
	}

	backend, err = lb.Pick(ctx, "nonexistent", "http")
	if err == nil {
		t.Error("expected error picking from non-existent chain")
	}
	if backend != nil {
		t.Error("expected nil backend for non-existent chain")
	}
}

func TestLoadBalancer_PickWithHealthStatus(t *testing.T) {
	cfg := createTestConfig()
	lb := NewLoadBalancer(cfg)

	ctx := context.Background()

	ethereumBackends := lb.GetBackends("ethereum")
	if len(ethereumBackends) < 2 {
		t.Fatal("need at least 2 backends for this test")
	}

	ethereumBackends[0].HealthUp.Store(false)

	backend, err := lb.Pick(ctx, "ethereum", "http")
	if err != nil {
		t.Errorf("expected no error picking healthy backend, got %v", err)
	}
	if backend == nil {
		t.Fatal("expected backend to be returned")
	}
	if !backend.HealthUp.Load() {
		t.Error("expected healthy backend to be picked")
	}

	for _, b := range ethereumBackends {
		b.HealthUp.Store(false)
	}

	backend, err = lb.Pick(ctx, "ethereum", "http")
	if err == nil {
		t.Error("expected error when no healthy backends exist")
	}
	if backend != nil {
		t.Error("expected nil backend when no healthy backends exist")
	}
}

func TestLoadBalancer_PickWithRateLimiting(t *testing.T) {
	cfg := createTestConfig()
	lb := NewLoadBalancer(cfg)

	ctx := context.Background()

	ethereumBackends := lb.GetBackends("ethereum")
	if len(ethereumBackends) < 1 {
		t.Fatal("need at least 1 backend for this test")
	}

	ethereumBackends[0].limiter = rate.NewLimiter(rate.Limit(1), 1)

	backend, err := lb.Pick(ctx, "ethereum", "http")
	if err != nil {
		t.Errorf("expected no error on first request, got %v", err)
	}
	if backend == nil {
		t.Fatal("expected backend to be returned")
	}

	backend2, err := lb.Pick(ctx, "ethereum", "http")
	if err != nil {
		t.Errorf("expected no error on second request, got %v", err)
	}
	if backend2 == nil {
		t.Fatal("expected backend to be returned")
	}
}

func TestLoadBalancer_UpdateLatency(t *testing.T) {
	cfg := createTestConfig()
	lb := NewLoadBalancer(cfg)

	ethereumBackends := lb.GetBackends("ethereum")
	if len(ethereumBackends) < 1 {
		t.Fatal("need at least 1 backend for this test")
	}

	backend := ethereumBackends[0]

	if len(backend.PerformanceLatencyHistory) != 0 {
		t.Errorf("expected empty latency history, got %d entries", len(backend.PerformanceLatencyHistory))
	}

	latency := 100 * time.Millisecond
	lb.UpdateLatency(backend, latency, cfg.Performance)

	if len(backend.PerformanceLatencyHistory) != 1 {
		t.Errorf("expected 1 latency entry, got %d", len(backend.PerformanceLatencyHistory))
	}

	if backend.PerformanceLatencyHistory[0] != latency {
		t.Errorf("expected latency %v, got %v", latency, backend.PerformanceLatencyHistory[0])
	}

	if backend.PerformanceAvgLatency != latency {
		t.Errorf("expected average latency %v, got %v", latency, backend.PerformanceAvgLatency)
	}

	if backend.PeformanceRequestCount.Load() != 1 {
		t.Errorf("expected request count 1, got %d", backend.PeformanceRequestCount.Load())
	}
}

func TestLoadBalancer_WeightCalculation(t *testing.T) {
	cfg := createTestConfig()
	lb := NewLoadBalancer(cfg)

	ethereumBackends := lb.GetBackends("ethereum")
	if len(ethereumBackends) < 2 {
		t.Fatal("need at least 2 backends for this test")
	}

	backend1 := ethereumBackends[0]
	backend2 := ethereumBackends[1]

	backend1.HealthUp.Store(true)
	backend2.HealthUp.Store(true)

	backend1.PerformanceAvgLatency = 100 * time.Millisecond
	backend2.PerformanceAvgLatency = 200 * time.Millisecond

	lb.weigh("ethereum", cfg.Performance)

	weights := lb.GetWeights("ethereum")

	if weights[0] <= weights[1] {
		t.Errorf("expected faster backend to have higher weight, got %f vs %f", weights[0], weights[1])
	}

	for i, weight := range weights {
		if weight <= 0 {
			t.Errorf("expected positive weight for backend %d, got %f", i, weight)
		}
	}
}

func TestLoadBalancer_WeightCalculationWithUnhealthyBackends(t *testing.T) {
	cfg := createTestConfig()
	lb := NewLoadBalancer(cfg)

	ethereumBackends := lb.GetBackends("ethereum")
	if len(ethereumBackends) < 2 {
		t.Fatal("need at least 2 backends for this test")
	}

	ethereumBackends[0].HealthUp.Store(false)
	ethereumBackends[1].HealthUp.Store(true)

	ethereumBackends[0].PerformanceAvgLatency = 100 * time.Millisecond
	ethereumBackends[1].PerformanceAvgLatency = 200 * time.Millisecond

	lb.weigh("ethereum", cfg.Performance)

	weights := lb.GetWeights("ethereum")

	if weights[0] != 1.0 {
		t.Errorf("expected unhealthy backend weight 1.0, got %f", weights[0])
	}

	if weights[1] != 1.0 {
		t.Errorf("expected healthy backend weight 1.0 when only healthy backend, got %f", weights[1])
	}

	ethereumBackends[0].HealthUp.Store(true)
	ethereumBackends[0].PerformanceAvgLatency = 100 * time.Millisecond
	ethereumBackends[1].PerformanceAvgLatency = 200 * time.Millisecond

	lb.weigh("ethereum", cfg.Performance)

	weights = lb.GetWeights("ethereum")

	if weights[0] <= weights[1] {
		t.Errorf("expected faster backend to have higher weight, got %f vs %f", weights[0], weights[1])
	}

	for i, weight := range weights {
		if weight <= 0 {
			t.Errorf("expected positive weight for backend %d, got %f", i, weight)
		}
	}
}

func TestLoadBalancer_RandomFunctions(t *testing.T) {
	cfg := createTestConfig()
	lb := NewLoadBalancer(cfg)

	n := 10
	result := lb.randN(n)
	if result < 0 || result >= n {
		t.Errorf("expected randN result between 0 and %d, got %d", n-1, result)
	}

	resultFloat := lb.randFloat()
	if resultFloat < 0.0 || resultFloat >= 1.0 {
		t.Errorf("expected randFloat result between 0.0 and 1.0, got %f", resultFloat)
	}

	results := make(map[int]bool)
	for range 100 {
		results[lb.randN(n)] = true
	}
	if len(results) < 5 {
		t.Error("expected randN to produce varied results")
	}
}

func TestLoadBalancer_GetBackends(t *testing.T) {
	cfg := createTestConfig()
	lb := NewLoadBalancer(cfg)

	ethereumBackends := lb.GetBackends("ethereum")
	if len(ethereumBackends) != 2 {
		t.Errorf("expected 2 ethereum backends, got %d", len(ethereumBackends))
	}

	baseBackends := lb.GetBackends("base")
	if len(baseBackends) != 1 {
		t.Errorf("expected 1 base backend, got %d", len(baseBackends))
	}

	nonexistentBackends := lb.GetBackends("nonexistent")
	if len(nonexistentBackends) != 0 {
		t.Errorf("expected 0 backends for non-existent chain, got %d", len(nonexistentBackends))
	}
}

func TestLoadBalancer_GetWeights(t *testing.T) {
	cfg := createTestConfig()
	lb := NewLoadBalancer(cfg)

	ethereumWeights := lb.GetWeights("ethereum")
	if len(ethereumWeights) != 2 {
		t.Errorf("expected 2 ethereum weights, got %d", len(ethereumWeights))
	}

	baseWeights := lb.GetWeights("base")
	if len(baseWeights) != 1 {
		t.Errorf("expected 1 base weight, got %d", len(baseWeights))
	}

	nonexistentWeights := lb.GetWeights("nonexistent")
	if len(nonexistentWeights) != 0 {
		t.Errorf("expected 0 weights for non-existent chain, got %d", len(nonexistentWeights))
	}

	ethereumWeights[0] = 999.0
	originalWeights := lb.GetWeights("ethereum")
	if originalWeights[0] == 999.0 {
		t.Error("expected returned slice to be a copy, not a reference")
	}
}

func TestLoadBalancer_PickAtLeast(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		heads    [2]uint64
		minHead  uint64
		wantName map[string]bool
	}{
		{
			name:     "excludes backend with known lower head",
			heads:    [2]uint64{100, 200},
			minHead:  150,
			wantName: map[string]bool{"backend2": true},
		},
		{
			name:     "keeps backend with unknown head",
			heads:    [2]uint64{0, 100},
			minHead:  150,
			wantName: map[string]bool{"backend1": true},
		},
		{
			name:     "head equal to minHead is eligible",
			heads:    [2]uint64{150, 100},
			minHead:  150,
			wantName: map[string]bool{"backend1": true},
		},
		{
			name:     "falls back to any healthy backend when all known heads are below",
			heads:    [2]uint64{100, 120},
			minHead:  150,
			wantName: map[string]bool{"backend1": true, "backend2": true},
		},
		{
			name:     "zero minHead applies no exclusion",
			heads:    [2]uint64{1, 2},
			minHead:  0,
			wantName: map[string]bool{"backend1": true, "backend2": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := createTestConfig()
			lb := NewLoadBalancer(cfg)
			bes := lb.GetBackends("ethereum")
			bes[0].Head.Store(tt.heads[0])
			bes[1].Head.Store(tt.heads[1])

			for range 50 {
				b, err := lb.PickAtLeast(ctx, "ethereum", "http", tt.minHead)
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				if !tt.wantName[b.Name] {
					t.Fatalf("picked %s, want one of %v", b.Name, tt.wantName)
				}
			}
		})
	}
}

func TestLoadBalancer_PickAtLeastNoHealthyBackends(t *testing.T) {
	cfg := createTestConfig()
	lb := NewLoadBalancer(cfg)
	for _, b := range lb.GetBackends("ethereum") {
		b.HealthUp.Store(false)
	}

	b, err := lb.PickAtLeast(context.Background(), "ethereum", "http", 10)
	if err == nil {
		t.Error("expected error when no healthy backends exist")
	}
	if b != nil {
		t.Error("expected nil backend when no healthy backends exist")
	}
}

func TestBackend_ObserveHead(t *testing.T) {
	tests := []struct {
		name    string
		initial uint64
		observe uint64
		want    uint64
	}{
		{name: "sets from unknown", initial: 0, observe: 10, want: 10},
		{name: "raises", initial: 10, observe: 20, want: 20},
		{name: "never lowers", initial: 20, observe: 10, want: 20},
		{name: "equal is a no-op", initial: 20, observe: 20, want: 20},
		{name: "zero observation is ignored", initial: 20, observe: 0, want: 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := createTestBackend("b", "ethereum", "https://api.example.com")
			b.Head.Store(tt.initial)
			b.ObserveHead(tt.observe)
			if got := b.Head.Load(); got != tt.want {
				t.Errorf("head %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBackend_HeadBelow(t *testing.T) {
	tests := []struct {
		name    string
		initial uint64
		below   uint64
		want    uint64
	}{
		{name: "lowers unknown head", initial: 0, below: 100, want: 99},
		{name: "lowers head at or above", initial: 150, below: 100, want: 99},
		{name: "lowers head equal", initial: 100, below: 100, want: 99},
		{name: "keeps head already below", initial: 50, below: 100, want: 50},
		{name: "zero is a no-op", initial: 50, below: 0, want: 50},
		{name: "below one leaves head unknown", initial: 5, below: 1, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := createTestBackend("b", "ethereum", "https://api.example.com")
			b.Head.Store(tt.initial)
			b.HeadBelow(tt.below)
			if got := b.Head.Load(); got != tt.want {
				t.Errorf("head %d, want %d", got, tt.want)
			}
		})
	}
}

// headNode answers the way a node at block 0x10 does: eth_blockNumber with the
// height, eth_getBlockByNumber("latest") with the head block.
func headNode(t *testing.T, probes *atomic.Int64, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []any           `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == "eth_blockNumber":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":"0x10"}`, req.ID)
		case req.Method == "eth_getBlockByNumber" && len(req.Params) > 0 && req.Params[0] == "latest":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"number":"0x10","hash":"0x%064x","parentHash":"0x%064x"}}`, req.ID, 16, 15)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, req.ID)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func parseHealthConfig(t *testing.T, upstream string, ttlCheckMs, timeoutMs int) Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := fmt.Sprintf(`
[health]
enabled = true
ttl_check_ms = %d
timeout_ms = %d
fails_to_down = 2
passes_to_up = 2

[cache]
enabled = true
path = %q

[node.1]
url = %q
`, ttlCheckMs, timeoutMs, filepath.Join(dir, "cache"), upstream)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return ParseConfig(path)
}

func waitFor(t *testing.T, within time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func TestMonitor_ProbesOnTheConfiguredIntervalAndAdvancesTheChainTip(t *testing.T) {
	var probes atomic.Int64
	srv := headNode(t, &probes, 0)
	cfg := parseHealthConfig(t, srv.URL, 20, 500)

	store, err := Open(cfg.Cache)
	if err != nil {
		t.Fatalf("opening cache: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lb := NewLoadBalancer(cfg)
	lb.Monitor(ctx, cfg, store)

	if !waitFor(t, 2*time.Second, func() bool { return probes.Load() >= 3 }) {
		t.Fatalf("%d probes reached the upstream in 2s with ttl_check_ms = 20", probes.Load())
	}

	b := lb.GetBackends("1")[0]
	if !waitFor(t, time.Second, func() bool { return b.Head.Load() == 0x10 }) {
		t.Errorf("backend head %d, want 16", b.Head.Load())
	}

	wantHash := fmt.Sprintf("0x%064x", 16)
	if !waitFor(t, time.Second, func() bool {
		store.mu.RLock()
		defer store.mu.RUnlock()
		return store.latestBlocks["1"] == 0x10 && store.latestHashes["1"] == wantHash
	}) {
		store.mu.RLock()
		t.Errorf("chain tip %d %q, want 16 %q", store.latestBlocks["1"], store.latestHashes["1"], wantHash)
		store.mu.RUnlock()
	}
}

func TestMonitor_ProbeTimesOutAtTheConfiguredTimeout(t *testing.T) {
	var probes atomic.Int64
	srv := headNode(t, &probes, 5*time.Second)
	cfg := parseHealthConfig(t, srv.URL, 20, 50)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lb := NewLoadBalancer(cfg)
	lb.Monitor(ctx, cfg, nil)

	b := lb.GetBackends("1")[0]
	if !waitFor(t, 2*time.Second, func() bool { return !b.HealthUp.Load() }) {
		t.Fatalf("backend still up after 2s of probes that never answer, with timeout_ms = 50 (%d probes sent)", probes.Load())
	}
}
