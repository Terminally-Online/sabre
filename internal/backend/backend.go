package backend

import (
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Backend represents a single RPC backend with health monitoring, rate limiting,
// and performance tracking capabilities.
type Backend struct {
	Name                      string
	Chain                     string
	URL                       *url.URL
	WSURL                     *url.URL
	Client                    *http.Client
	PerformanceLatencyHistory []time.Duration
	PerformanceAvgLatency     time.Duration
	PerformanceWeight         float64
	PeformanceRequestCount    atomic.Int64
	PeformanceLastRequest     atomic.Value
	HealthProbeLatency        time.Duration
	HealthUp                  atomic.Bool
	HealthFailStreak          atomic.Int32
	HealthPassStreak          atomic.Int32
	HealthLastOK              atomic.Value
	HealthLastErr             atomic.Value

	limiter *rate.Limiter
}
