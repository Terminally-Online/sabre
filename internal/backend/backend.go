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
	Head                      atomic.Uint64

	limiter *rate.Limiter
}

// ObserveHead records that the backend has imported block n. The known head
// only ever rises here; a stale observation never lowers it.
func (b *Backend) ObserveHead(n uint64) {
	for {
		cur := b.Head.Load()
		if n <= cur || b.Head.CompareAndSwap(cur, n) {
			return
		}
	}
}

// HeadBelow records that the backend has not yet imported block n, lowering
// its known head to n-1 when the current knowledge (unknown, or at least n)
// contradicts that evidence.
func (b *Backend) HeadBelow(n uint64) {
	if n == 0 {
		return
	}
	for {
		cur := b.Head.Load()
		if cur != 0 && cur < n {
			return
		}
		if b.Head.CompareAndSwap(cur, n-1) {
			return
		}
	}
}
