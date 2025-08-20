package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"rapier/internal/backend"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
)

const (
	readTimeout       = 30 * time.Second
	readHeaderTimeout = 10 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
	barMax            = 60
	glyphRPS          = 10.0
	emaAlpha          = 0.35
	tickEvery         = time.Second
)

var (
	totalReq atomic.Uint64

	bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}
)

var ErrServerClosed = http.ErrServerClosed

func writeHopSafeHeaders(dst http.ResponseWriter, src http.Header) {
	for k, vv := range src {
		kl := strings.ToLower(k)
		switch kl {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"te", "trailers", "transfer-encoding", "upgrade":
			continue
		}
		for _, v := range vv {
			dst.Header().Add(k, v)
		}
	}
}

func isRetryableStatus(code int) bool {
	if code == 429 {
		return true
	}
	return code >= 500 && code != 501 && code != 505
}

func sendTo(ctx context.Context, bk *backend.Backend, inHdr http.Header, body []byte) (status int, hdr http.Header, data []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bk.URL.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	ct := inHdr.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	req.Header.Set("Content-Type", ct)
	if auth := inHdr.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := bk.Client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, _ = io.ReadAll(resp.Body)

	hdr = make(http.Header, len(resp.Header))
	for k, vv := range resp.Header {
		kl := strings.ToLower(k)
		switch kl {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"te", "trailers", "transfer-encoding", "upgrade":
			continue
		default:
			for _, v := range vv {
				hdr.Add(k, v)
			}
		}
	}
	return resp.StatusCode, hdr, data, nil
}

func animate() func() {
	tick := time.NewTicker(tickEvery)
	done := make(chan struct{})

	go func() {
		defer tick.Stop()
		var last uint64
		var ema float64
		for {
			select {
			case <-tick.C:
				cur := totalReq.Load()
				delta := float64(cur - last)
				last = cur

				rps := delta / tickEvery.Seconds()
				if ema == 0 {
					ema = rps
				} else {
					ema = emaAlpha*rps + (1-emaAlpha)*ema
				}
				n := min(max(int(ema/glyphRPS+0.5), 4), barMax)
				bar := "<----]=╦" + strings.Repeat("═", n) + "▷"
				fmt.Fprintf(os.Stdout, "\r%-80s %s rps %s total", bar, humanize.SI(rps, ""), humanize.SI(float64(cur), ""))
			case <-done:
				return
			}
		}
	}()

	return func() { close(done); fmt.Fprintln(os.Stdout) }
}

func NewRouter(cstore *backend.Store, cfg *backend.Config, lb *backend.LoadBalancer) *http.Server {
	defer fmt.Fprintln(os.Stdout)

	type rpcReq struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}

	mux := http.NewServeMux()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lb.Monitor(ctx, *cfg, cstore)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		totalReq.Add(1)

		chain := strings.Trim(strings.TrimSpace(r.URL.Path), "/")
		if chain == "" {
			http.NotFound(w, r)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		buf := bufPool.Get().(*bytes.Buffer)
		buf.Reset()
		defer bufPool.Put(buf)
		_, _ = io.Copy(buf, r.Body)
		_ = r.Body.Close()
		body := buf.Bytes()

		var req rpcReq
		_ = json.Unmarshal(body, &req)

		var batchReq backend.BatchRequest
		if cfg.Batch.Enabled {
			batchReq = backend.BatchRequest{
				JSONRPC: req.JSONRPC,
				ID:      req.ID,
				Method:  req.Method,
				Params:  req.Params,
			}
		}

		key, _ := backend.CanonicalKey(chain, req.Method, req.Params)
		ttl := backend.TTL(req.Method, req.Params, cstore.Config())
		if ttl > 0 {
			if cached, ok := cstore.Get(key); ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(cached)
				return
			}
		}

		var (
			bk     *backend.Backend
			status int
			hdrs   http.Header
			data   []byte
			err    error
		)

		for attempt := 1; attempt <= cfg.Rapier.MaxAttempts; attempt++ {
			bk, err = lb.Pick(r.Context(), chain)
			if err != nil {
				break
			}

			start := time.Now()

			if cfg.Batch.Enabled {
				responseChan, err := cfg.BatchProcessor.AddRequest(bk.URL.String(), batchReq)
				if err != nil {
					status, hdrs, data, err = sendTo(r.Context(), bk, r.Header, body)
				} else {
					select {
					case responses := <-responseChan:
						if len(responses) > 0 {
							for _, resp := range responses {
								if string(resp.ID) == string(req.ID) {
									data, _ = json.Marshal(resp)
									status = http.StatusOK
									hdrs = make(http.Header)
									hdrs.Set("Content-Type", "application/json")
									break
								}
							}
						}
						if status == 0 {
							err = fmt.Errorf("response not found in batch")
						}
					case <-time.After(30 * time.Second):
						err = fmt.Errorf("batch request timeout")
					}
				}
			} else {
				status, hdrs, data, err = sendTo(r.Context(), bk, r.Header, body)
			}
			if err == nil && !isRetryableStatus(status) {
				lb.UpdateLatency(bk, time.Since(start), cfg.Performance)
				if status == http.StatusOK {
					if blockNum, blockHash := backend.ExtractBlockInfo(data); blockNum > 0 {
						cstore.UpdateLatestBlock(chain, blockNum, blockHash, data)
					}
				}
				break
			}

			if attempt == cfg.Rapier.MaxAttempts && err != nil {
				http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
				return
			}
		}

		if ttl > 0 && status == http.StatusOK {
			cstore.Put(key, data, ttl, chain)
		}

		writeHopSafeHeaders(w, hdrs)
		w.Header().Set("X-Upstream-Provider", bk.Name)
		w.Header().Set("X-Upstream-Chain", bk.Chain)
		w.Header().Set("X-Upstream-URL", bk.URL.String())
		w.WriteHeader(status)
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		type item struct {
			Name    string        `json:"name"`
			Chain   string        `json:"chain"`
			URL     string        `json:"url"`
			Up      bool          `json:"up"`
			Latency time.Duration `json:"latency_ms"`
			Fails   int32         `json:"fail_streak"`
			Passes  int32         `json:"pass_streak"`
		}

		ready := true
		for chain := range cfg.BackendsCt {
			bes := lb.GetBackends(chain)
			hasHealthy := false
			for i := range bes {
				b := bes[i]
				if b.HealthUp.Load() {
					hasHealthy = true
					break
				}
			}
			if !hasHealthy {
				ready = false
				log.Printf("health check: no healthy backends for chain %s", chain)
				break
			}
		}

		out := make([]item, 0, len(cfg.Backends))
		for i := range cfg.Backends {
			b := &cfg.Backends[i]
			out = append(out, item{
				Name:    b.Name,
				Chain:   b.Chain,
				URL:     b.URL.String(),
				Up:      b.HealthUp.Load(),
				Latency: b.HealthProbeLatency,
				Fails:   b.HealthFailStreak.Load(),
				Passes:  b.HealthPassStreak.Load(),
			})
		}

		w.Header().Set("Content-Type", "application/json")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(out)
	})

	srv := &http.Server{
		Addr:              cfg.Rapier.Listen,
		Handler:           mux,
		ReadTimeout:       readTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		BaseContext:       func(_ net.Listener) context.Context { return context.Background() },
	}

	stop := animate()
	defer stop()

	return srv
}
