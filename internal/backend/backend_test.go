package backend

import (
	"net/url"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestBackend_NewBackend(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wsURL    string
		expected bool
	}{
		{
			name:     "valid HTTP URL",
			url:      "https://api.example.com",
			expected: true,
		},
		{
			name:     "valid HTTP and WebSocket URLs",
			url:      "https://api.example.com",
			wsURL:    "wss://api.example.com",
			expected: true,
		},
		{
			name:     "invalid URL",
			url:      "://invalid-url",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var backendURL, wsBackendURL *url.URL
			var err error

			if tt.url != "" {
				backendURL, err = url.Parse(tt.url)
				if tt.expected && err != nil {
					t.Errorf("expected valid URL, got error: %v", err)
					return
				}
			}

			if tt.wsURL != "" {
				wsBackendURL, err = url.Parse(tt.wsURL)
				if tt.expected && err != nil {
					t.Errorf("expected valid WebSocket URL, got error: %v", err)
					return
				}
			}

			backend := &Backend{
				Name:  "test-backend",
				Chain: "ethereum",
				URL:   backendURL,
				WSURL: wsBackendURL,
			}

			if backend.Name != "test-backend" {
				t.Errorf("expected name 'test-backend', got '%s'", backend.Name)
			}

			if backend.Chain != "ethereum" {
				t.Errorf("expected chain 'ethereum', got '%s'", backend.Chain)
			}

			if tt.expected && backendURL != nil && backend.URL.String() != tt.url {
				t.Errorf("expected URL '%s', got '%s'", tt.url, backend.URL.String())
			}

			if tt.expected && wsBackendURL != nil && backend.WSURL.String() != tt.wsURL {
				t.Errorf("expected WebSocket URL '%s', got '%s'", tt.wsURL, backend.WSURL.String())
			}
		})
	}
}

func TestBackend_PerformanceTracking(t *testing.T) {
	backend := &Backend{}

	if backend.PeformanceRequestCount.Load() != 0 {
		t.Errorf("expected initial request count 0, got %d", backend.PeformanceRequestCount.Load())
	}

	backend.PeformanceRequestCount.Add(1)
	if backend.PeformanceRequestCount.Load() != 1 {
		t.Errorf("expected request count 1, got %d", backend.PeformanceRequestCount.Load())
	}

	now := time.Now()
	backend.PeformanceLastRequest.Store(now)
	storedTime := backend.PeformanceLastRequest.Load().(time.Time)
	if !storedTime.Equal(now) {
		t.Errorf("expected last request time %v, got %v", now, storedTime)
	}
}

func TestBackend_HealthTracking(t *testing.T) {
	backend := &Backend{}

	if backend.HealthUp.Load() {
		t.Error("expected initial health state to be false")
	}

	backend.HealthUp.Store(true)
	if !backend.HealthUp.Load() {
		t.Error("expected health state to be true after setting")
	}

	backend.HealthFailStreak.Add(1)
	if backend.HealthFailStreak.Load() != 1 {
		t.Errorf("expected fail streak 1, got %d", backend.HealthFailStreak.Load())
	}

	backend.HealthPassStreak.Add(1)
	if backend.HealthPassStreak.Load() != 1 {
		t.Errorf("expected pass streak 1, got %d", backend.HealthPassStreak.Load())
	}

	now := time.Now()
	backend.HealthLastOK.Store(now)
	storedTime := backend.HealthLastOK.Load().(time.Time)
	if !storedTime.Equal(now) {
		t.Errorf("expected last OK time %v, got %v", now, storedTime)
	}

	errorMsg := "connection timeout"
	backend.HealthLastErr.Store(errorMsg)
	storedError := backend.HealthLastErr.Load().(string)
	if storedError != errorMsg {
		t.Errorf("expected last error '%s', got '%s'", errorMsg, storedError)
	}
}

func TestBackend_RateLimiting(t *testing.T) {
	backend := &Backend{}

	if backend.limiter != nil {
		t.Error("expected no rate limiter initially")
	}

	backend.limiter = rate.NewLimiter(rate.Limit(10), 1)
	if backend.limiter == nil {
		t.Error("expected rate limiter to be set")
	}

	if !backend.limiter.Allow() {
		t.Error("expected rate limiter to allow first request")
	}

	if backend.limiter.Allow() {
		t.Error("expected rate limiter to block second request in burst")
	}
}

func TestBackend_LatencyHistory(t *testing.T) {
	backend := &Backend{}

	if len(backend.PerformanceLatencyHistory) != 0 {
		t.Errorf("expected empty latency history, got %d entries", len(backend.PerformanceLatencyHistory))
	}

	latencies := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		150 * time.Millisecond,
	}

	backend.PerformanceLatencyHistory = append(backend.PerformanceLatencyHistory, latencies...)

	if len(backend.PerformanceLatencyHistory) != 3 {
		t.Errorf("expected 3 latency entries, got %d", len(backend.PerformanceLatencyHistory))
	}

	total := time.Duration(0)
	for _, l := range backend.PerformanceLatencyHistory {
		total += l
	}
	expectedAvg := total / time.Duration(len(backend.PerformanceLatencyHistory))
	backend.PerformanceAvgLatency = expectedAvg

	if backend.PerformanceAvgLatency != 150*time.Millisecond {
		t.Errorf("expected average latency 150ms, got %v", backend.PerformanceAvgLatency)
	}
}

func TestBackend_WeightCalculation(t *testing.T) {
	backend := &Backend{}

	if backend.PerformanceWeight != 0 {
		t.Errorf("expected initial weight 0, got %f", backend.PerformanceWeight)
	}

	backend.PerformanceWeight = 1.5
	if backend.PerformanceWeight != 1.5 {
		t.Errorf("expected weight 1.5, got %f", backend.PerformanceWeight)
	}

	testWeights := []float64{0.1, 1.0, 2.5, 10.0}
	for _, weight := range testWeights {
		backend.PerformanceWeight = weight
		if backend.PerformanceWeight != weight {
			t.Errorf("expected weight %f, got %f", weight, backend.PerformanceWeight)
		}
	}
}
