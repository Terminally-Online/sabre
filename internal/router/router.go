package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sabre/internal/backend"

	"github.com/gorilla/websocket"
)

const (
	readTimeout       = 30 * time.Second
	readHeaderTimeout = 10 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
)

var (
	TotalReq atomic.Uint64

	bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}
)

var ErrServerClosed = http.ErrServerClosed

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func NewRouter(cstore *backend.Store, cfg *backend.Config, lb *backend.LoadBalancer) *http.Server {
	defer fmt.Fprintln(os.Stdout)

	mux := http.NewServeMux()

	lb.Monitor(context.Background(), *cfg, cstore)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			if cfg.Stream == nil {
				http.Error(w, "WebSocket not enabled", http.StatusServiceUnavailable)
				return
			}

			cfg.Stream.HandleWebSocket(w, r)
			return
		}

		TotalReq.Add(1)

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
		if err := json.Unmarshal(body, &req); err != nil {
			errorResponse := map[string]any{
				"jsonrpc": "2.0",
				"id":      nil,
				"error": map[string]any{
					"code":    -32700,
					"message": "Parse error",
				},
			}
			errorData, _ := json.Marshal(errorResponse)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(errorData)
			return
		}

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
		ttl := backend.TTL(req.Method, req.Params, cstore.Config(), &cfg.Subscriptions)
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

		for attempt := 1; attempt <= cfg.Sabre.MaxAttempts; attempt++ {
			bk, err = lb.Pick(r.Context(), chain, "http")
			if err != nil {
				if attempt == cfg.Sabre.MaxAttempts {
					http.Error(w, "no available backends: "+err.Error(), http.StatusServiceUnavailable)
					return
				}
				continue
			}

			start := time.Now()

			if cfg.Batch.Enabled {
				status, hdrs, data, err = processBatchRequest(cfg.BatchProcessor, bk, batchReq, req, cfg.Performance.Timeout, r, body)
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

			if attempt == cfg.Sabre.MaxAttempts && err != nil {
				http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
				return
			}
		}

		if ttl > 0 && status == http.StatusOK {
			cstore.Put(key, data, ttl, chain)
		}

		writeHopSafeHeaders(w, hdrs)
		if bk != nil {
			w.Header().Set("X-Upstream-Provider", bk.Name)
			w.Header().Set("X-Upstream-Chain", bk.Chain)
			w.Header().Set("X-Upstream-URL", bk.URL.String())
		}

		if status == 0 {
			status = http.StatusInternalServerError
		}

		w.WriteHeader(status)
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		healthy := true
		for chain := range cfg.BackendsCt {
			bes := lb.GetBackends(chain)
			hasHealthy := false
			for i := range bes {
				if bes[i].HealthUp.Load() {
					hasHealthy = true
					break
				}
			}
			if !hasHealthy {
				healthy = false
				break
			}
		}

		if healthy {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("No healthy backends"))
		}
	})

	srv := &http.Server{
		Addr:              cfg.Sabre.Listen,
		Handler:           mux,
		ReadTimeout:       readTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		BaseContext:       func(_ net.Listener) context.Context { return context.Background() },
	}

	/* stop := animate() */
	/* defer stop() */

	return srv
}

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

func processBatchRequest(batchProcessor *backend.BatchProcessor, bk *backend.Backend, batchReq backend.BatchRequest, req rpcReq, timeout time.Duration, r *http.Request, body []byte) (status int, hdrs http.Header, data []byte, err error) {
	responseChan, err := batchProcessor.AddRequest(bk.URL.String(), batchReq)
	if err != nil {
		return sendTo(r.Context(), bk, r.Header, body)
	}

	select {
	case resp := <-responseChan:
		data, err = json.Marshal(resp)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("failed to marshal batch response: %w", err)
		}
		hdrs = make(http.Header)
		hdrs.Set("Content-Type", "application/json")
		return http.StatusOK, hdrs, data, nil
	case <-time.After(timeout):
		return 0, nil, nil, fmt.Errorf("batch request timeout")
	}
}
