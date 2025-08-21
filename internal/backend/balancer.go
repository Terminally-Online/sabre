package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

type LoadBalancer struct {
	mu       sync.RWMutex
	backends map[string][]*Backend
	weights  map[string][]float64
	seed     uint64
}

func NewLoadBalancer(cfg Config) *LoadBalancer {
	lb := &LoadBalancer{
		backends: make(map[string][]*Backend, len(cfg.BackendsCt)),
		weights:  make(map[string][]float64, len(cfg.BackendsCt)),
		seed:     uint64(time.Now().UnixNano()),
	}

	for _, b := range cfg.Backends {
		lb.backends[b.Chain] = append(lb.backends[b.Chain], b)

		b.PerformanceLatencyHistory = make([]time.Duration, 0, cfg.Performance.Samples)
		b.PerformanceWeight = 1.0
		b.PeformanceRequestCount.Store(0)
		b.PeformanceLastRequest.Store(time.Time{})
	}

	for chain := range lb.backends {
		lb.weights[chain] = make([]float64, len(lb.backends[chain]))
		for i := range lb.weights[chain] {
			lb.weights[chain][i] = 1.0
		}
	}

	return lb
}

func (lb *LoadBalancer) Pick(ctx context.Context, chain, scheme string) (*Backend, error) {
	lb.mu.RLock()
	bes := lb.backends[chain]
	weights := lb.weights[chain]

	// Take a snapshot of weights to avoid race conditions
	weightsSnapshot := make([]float64, len(weights))
	copy(weightsSnapshot, weights)
	lb.mu.RUnlock()

	if len(bes) == 0 {
		return nil, fmt.Errorf("no backends for chain %q", chain)
	}

	ready := make([]int, 0, len(bes))
	readyWeights := make([]float64, 0, len(bes))

	for i := range bes {
		b := bes[i]
		if !b.HealthUp.Load() {
			continue
		}

		if scheme == "ws" && b.WSURL == nil {
			continue
		}
		if scheme == "http" && b.URL == nil {
			continue
		}

		if b.limiter == nil || b.limiter.Allow() {
			ready = append(ready, i)
			readyWeights = append(readyWeights, weightsSnapshot[i])
		}
	}

	if len(ready) == 0 {
		minDelay := time.Duration(math.MaxInt64)
		minIdx := -1
		var keep rate.Reservation

		for i := range bes {
			b := bes[i]
			if !b.HealthUp.Load() {
				continue
			}

			if scheme == "ws" && b.WSURL == nil {
				continue
			}
			if scheme == "http" && b.URL == nil {
				continue
			}

			if b.limiter == nil {
				minDelay = 0
				minIdx = i
				break
			}
			r := b.limiter.Reserve()
			d := r.Delay()
			if d < minDelay {
				if keep.OK() {
					keep.Cancel()
				}
				keep = *r
				minDelay = d
				minIdx = i
			} else {
				r.Cancel()
			}
		}

		if minIdx == -1 {
			if scheme == "ws" {
				return nil, fmt.Errorf("no WebSocket-enabled backends for chain %q", chain)
			}
			if scheme == "http" {
				return nil, fmt.Errorf("no HTTP-enabled backends for chain %q", chain)
			}
			return nil, fmt.Errorf("no candidate backend")
		}

		if minDelay > 0 {
			t := time.NewTimer(minDelay)
			select {
			case <-ctx.Done():
				if keep.OK() {
					keep.Cancel()
				}
				return nil, ctx.Err()
			case <-t.C:
			}
		}

		return bes[minIdx], nil
	}

	if len(ready) == 1 {
		return bes[ready[0]], nil
	}

	totalWeight := 0.0
	for _, w := range readyWeights {
		totalWeight += w
	}

	if totalWeight <= 0 {
		return bes[ready[lb.randN(len(ready))]], nil
	}

	r := lb.randFloat() * totalWeight
	currentWeight := 0.0

	for i, idx := range ready {
		currentWeight += readyWeights[i]
		if r <= currentWeight {
			return bes[idx], nil
		}
	}

	return bes[ready[len(ready)-1]], nil
}

func (lb *LoadBalancer) UpdateLatency(backend *Backend, latency time.Duration, cfg PerformanceConfig) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	if len(backend.PerformanceLatencyHistory) >= cfg.Samples {
		backend.PerformanceLatencyHistory = backend.PerformanceLatencyHistory[1:]
	}
	backend.PerformanceLatencyHistory = append(backend.PerformanceLatencyHistory, latency)

	total := time.Duration(0)
	for _, l := range backend.PerformanceLatencyHistory {
		total += l
	}
	backend.PerformanceAvgLatency = total / time.Duration(len(backend.PerformanceLatencyHistory))

	backend.PeformanceRequestCount.Add(1)
	backend.PeformanceLastRequest.Store(time.Now())

	lb.weigh(backend.Chain, cfg)
}

func (lb *LoadBalancer) GetBackends(chain string) []*Backend {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	bes := lb.backends[chain]
	result := make([]*Backend, len(bes))
	copy(result, bes)
	return result
}

func (lb *LoadBalancer) GetWeights(chain string) []float64 {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	weights := lb.weights[chain]
	result := make([]float64, len(weights))
	copy(result, weights)
	return result
}

func (lb *LoadBalancer) Monitor(ctx context.Context, cfg Config, cacheStore *Store) {
	if !cfg.Health.Enabled {
		return
	}

	body, _ := json.Marshal(&struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
	}{"2.0", 1, cfg.Health.SampleMethod, nil})

	t := time.NewTicker(cfg.Health.TTLCheck * time.Millisecond)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				lb.mu.Lock()
				for _, bes := range lb.backends {
					for i := range bes {
						b := bes[i]
						ctx, cancel := context.WithTimeout(
							context.Background(),
							cfg.Health.Timeout*time.Millisecond,
						)

						req, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.URL.String(), bytes.NewReader(body))
						req.Header.Set("Content-Type", "application/json")
						start := time.Now()
						resp, err := b.Client.Do(req)
						d := time.Since(start)
						cancel()

						if err != nil {
							streak := b.HealthFailStreak.Add(1)
							b.HealthPassStreak.Store(0)
							b.HealthLastErr.Store(err.Error())
							if streak >= int32(cfg.Health.FailsToDown) {
								b.HealthUp.Store(false)
							}
							continue
						}
						defer resp.Body.Close()

						if resp.StatusCode >= 200 && resp.StatusCode < 300 {
							lb.UpdateLatency(b, d, cfg.Performance)

							if cacheStore != nil {
								respBody, _ := io.ReadAll(resp.Body)
								cacheStore.CheckReorgFromHealthResponse(respBody, b.Chain)
								resp.Body = io.NopCloser(bytes.NewReader(respBody))
							}

							streak := b.HealthPassStreak.Add(1)
							b.HealthFailStreak.Store(0)
							b.HealthLastOK.Store(time.Now())
							if streak >= int32(cfg.Health.PassesToUp) {
								b.HealthUp.Store(true)
							}
							continue
						}

						streak := b.HealthFailStreak.Add(1)
						b.HealthPassStreak.Store(0)
						b.HealthLastErr.Store(resp.Status)
						if streak >= int32(cfg.Health.FailsToDown) {
							b.HealthUp.Store(false)
						}
					}
				}
				lb.mu.Unlock()
			}
		}
	}()
}

func (lb *LoadBalancer) weigh(chain string, cfg PerformanceConfig) {
	bes := lb.backends[chain]
	if len(bes) == 0 {
		return
	}
	weights := lb.weights[chain]

	fastestLatency := time.Duration(math.MaxInt64)
	for _, b := range bes {
		if b.HealthUp.Load() && b.PerformanceAvgLatency > 0 && b.PerformanceAvgLatency < fastestLatency {
			fastestLatency = b.PerformanceAvgLatency
		}
	}
	if fastestLatency == time.Duration(math.MaxInt64) {
		for i := range weights {
			weights[i] = 1.0
		}
		return
	}

	for i, b := range bes {
		if !b.HealthUp.Load() || b.PerformanceAvgLatency <= 0 {
			weights[i] = 1.0
			continue
		}
		latencyRatio := float64(b.PerformanceAvgLatency) / float64(fastestLatency)
		weight := math.Pow(latencyRatio, -cfg.Gamma)
		if weight < 0.1 {
			weight = 0.1
		}

		weights[i] = weight
	}
}

func (lb *LoadBalancer) randN(n int) int {
	if n <= 1 {
		return 0
	}
	x := atomic.AddUint64(&lb.seed, 0x9e3779b97f4a7c15)
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	x *= 0x2545F4914F6CDD1D
	return int(x % uint64(n))
}

func (lb *LoadBalancer) randFloat() float64 {
	x := atomic.AddUint64(&lb.seed, 0x9e3779b97f4a7c15)
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	x *= 0x2545F4914F6CDD1D
	return float64(x) / float64(^uint64(0))
}
