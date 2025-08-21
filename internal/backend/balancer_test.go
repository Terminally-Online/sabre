package backend

import (
	"context"
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
			Enabled:      true,
			TTLCheck:     1000 * time.Millisecond,
			Timeout:      1500 * time.Millisecond,
			FailsToDown:  2,
			PassesToUp:   2,
			SampleMethod: "eth_blockNumber",
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
