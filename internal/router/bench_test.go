package router

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"sabre/internal/backend"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// benchUpstreamBody is what the stub node answers. It is deliberately trivial
// so the measurement isolates the proxy's own per-request cost rather than the
// node's execution time.
const benchUpstreamBody = `{"jsonrpc":"2.0","id":"1","result":"0x000000000000000000000000000000000000000000000000000000000000002a"}`

// benchRouter wires the router to a real HTTP stub upstream, with the batch
// settings the production binary runs, so a profile taken here reflects the
// work actually done per request.
func benchRouter(b *testing.B, batchEnabled bool) (*http.Server, func()) {
	b.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		w.Header().Set("Content-Type", "application/json")
		raw := bytes.TrimSpace(buf.Bytes())

		if bytes.HasPrefix(raw, []byte("[")) {
			var reqs []struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(raw, &reqs)
			out := make([]json.RawMessage, 0, len(reqs))
			for _, q := range reqs {
				out = append(out, json.RawMessage(
					`{"jsonrpc":"2.0","id":`+string(q.ID)+`,"result":"0x2a"}`))
			}
			enc, _ := json.Marshal(out)
			_, _ = w.Write(enc)
			return
		}
		_, _ = w.Write([]byte(benchUpstreamBody))
	}))

	cfg := createTestConfig()
	cfg.Cache.Enabled = false
	cfg.Batch = backend.BatchConfig{
		Enabled:          batchEnabled,
		MaxBatchSize:     10,
		MaxBatchWaitTime: 5 * time.Millisecond,
		MaxBatchWorkers:  32,
	}
	if batchEnabled {
		cfg.BatchProcessor = backend.NewBatchProcessor(
			cfg.Batch.MaxBatchSize, cfg.Batch.MaxBatchWaitTime,
			cfg.Batch.MaxBatchWorkers, 1, cfg.Multicall)
	}

	parsed, _ := url.Parse(upstream.URL)
	for _, bk := range cfg.Backends {
		bk.URL = parsed
		bk.Client = upstream.Client()
		bk.HealthUp.Store(true)
	}

	storeCfg := backend.CacheConfig{
		Enabled: false, Path: filepath.Join(getTestCachePath(), b.Name()),
		MemEntries: 1000, TTLLatest: 250 * time.Millisecond,
		TTLBlock: 24 * time.Hour, Clean: true, MaxReorgDepth: 100,
	}
	store, err := backend.Open(storeCfg)
	if err != nil {
		b.Fatalf("store: %v", err)
	}

	srv := NewRouter(store, &cfg, backend.NewLoadBalancer(cfg))
	return srv, func() {
		_ = store.Close()
		upstream.Close()
	}
}

// BenchmarkRouterRequest measures the proxy's own cost of serving one eth_call
// against an upstream that answers instantly.
//
// The proxy exists to add as little as possible between a caller and the node,
// so its per-request cost decides how much of the node's throughput survives.
//
// The handler emits a structured line per request through the global logger,
// which tests leave as a no-op — so the log variants are not decoration, they
// are the difference between measuring this binary and measuring the one that
// runs in production.
func BenchmarkRouterRequest(b *testing.B) {
	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48","data":"0x70a08231000000000000000000000000dac17f958d2ee523a2206206994597c13d831ec7"},"latest"]}`)

	logModes := []struct {
		name  string
		build func() *zap.Logger
	}{
		{"nolog", zap.NewNop},
		{"log", func() *zap.Logger {
			enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
			return zap.New(zapcore.NewCore(enc, zapcore.AddSync(io.Discard), zapcore.InfoLevel))
		}},
	}

	for _, batched := range []bool{false, true} {
		mode := "direct"
		if batched {
			mode = "batched"
		}
		for _, lm := range logModes {
			b.Run(mode+"/"+lm.name, func(b *testing.B) {
				restore := zap.ReplaceGlobals(lm.build())
				defer restore()

				srv, cleanup := benchRouter(b, batched)
				defer cleanup()

				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						req := httptest.NewRequest(http.MethodPost, "/ethereum", bytes.NewReader(payload))
						req.Header.Set("Content-Type", "application/json")
						rec := httptest.NewRecorder()
						srv.Handler.ServeHTTP(rec, req)
						if rec.Code != http.StatusOK {
							b.Fatalf("status %d: %s", rec.Code, rec.Body.String())
						}
					}
				})
			})
		}
	}
}
