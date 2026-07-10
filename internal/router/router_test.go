package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sabre/internal/backend"
)

func getTestCachePath() string {
	cwd, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("failed to get working directory: %v", err))
	}

	if filepath.Base(cwd) == "router" {
		cwd = filepath.Dir(filepath.Dir(cwd))
	}

	return filepath.Join(cwd, ".data", "sabre", "test")
}

func getUniqueTestCachePath(t *testing.T) string {
	basePath := getTestCachePath()
	return filepath.Join(basePath, t.Name())
}

func createTestConfig() backend.Config {
	return backend.Config{
		Sabre: backend.SabreConfig{
			Listen:      ":3000",
			MaxAttempts: 3,
		},
		Health: backend.HealthConfig{
			Enabled:      true,
			TTLCheck:     1000 * time.Millisecond,
			Timeout:      1500 * time.Millisecond,
			FailsToDown:  2,
			PassesToUp:   2,
			SampleMethod: "eth_blockNumber",
		},
		Performance: backend.PerformanceConfig{
			Timeout:              2000 * time.Millisecond,
			Samples:              100,
			Gamma:                0.9,
			MaxIdleConns:         8192,
			MaxIdleConnsPerHost:  2048,
			IdleConnTimeout:      90 * time.Second,
			DisableKeepAlives:    false,
			EnableHTTP2:          true,
			MaxConcurrentStreams: 250,
			EnableCompression:    true,
			CompressionLevel:     6,
		},
		Subscriptions: backend.SubscriptionsConfig{
			TTLBlock:                      13000 * time.Millisecond,
			MaxConnectionsPerBackend:      100,
			MaxSubscriptionsPerConnection: 50,
			PingInterval:                  30 * time.Second,
			PongWait:                      10 * time.Second,
			WriteWait:                     10 * time.Second,
			ReadWait:                      60 * time.Second,
			EnableCompression:             true,
			MaxMessageSize:                1048576,
		},
		Batch: backend.BatchConfig{
			Enabled:          false,
			MaxBatchSize:     10,
			MaxBatchWaitTime: 50 * time.Millisecond,
			MaxBatchWorkers:  4,
		},
		BatchProcessor: nil,
		Cache: backend.CacheConfig{
			Enabled:       true,
			Path:          getTestCachePath(),
			MemEntries:    1000,
			TTLLatest:     250 * time.Millisecond,
			TTLBlock:      24 * time.Hour,
			Clean:         true,
			MaxReorgDepth: 100,
		},
		Backends: func() []*backend.Backend {
			backend1 := backend.CreateMockBackend("test-backend-1", "ethereum", "https://api1.example.com")
			backend2 := backend.CreateMockBackend("test-backend-2", "ethereum", "https://api2.example.com")

			backend1.HealthUp.Store(true)
			backend2.HealthUp.Store(true)

			return []*backend.Backend{backend1, backend2}
		}(),
		BackendsCt: map[string]int{
			"ethereum": 2,
		},
		HasWebSocket: false,
	}
}

func createTestStore(t *testing.T) *backend.Store {
	cfg := backend.CacheConfig{
		Enabled:       true,
		Path:          getUniqueTestCachePath(t),
		MemEntries:    1000,
		TTLLatest:     250 * time.Millisecond,
		TTLBlock:      24 * time.Hour,
		Clean:         true,
		MaxReorgDepth: 100,
	}

	store, err := backend.Open(cfg)
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	return store
}

func cleanupTestStore(t *testing.T, store *backend.Store) {
	t.Helper()
	if store != nil {
		_ = store.Close()
	}
}

func TestNewRouter(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	if server == nil {
		t.Fatal("expected router to be created")
	}

	if server.Addr != cfg.Sabre.Listen {
		t.Errorf("expected server address %s, got %s", cfg.Sabre.Listen, server.Addr)
	}

	if server.Handler == nil {
		t.Error("expected server handler to be set")
	}
}

func TestRouter_HealthEndpoint(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := strings.TrimSpace(w.Body.String())
	if body != "OK" {
		t.Errorf("expected body 'OK', got '%s'", body)
	}
}

func TestRouter_HealthEndpointWithUnhealthyBackends(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)

	ethereumBackends := lb.GetBackends("ethereum")
	for _, b := range ethereumBackends {
		b.HealthUp.Store(false)
	}

	server := NewRouter(store, &cfg, lb)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}

	body := strings.TrimSpace(w.Body.String())
	if body != "No healthy backends" {
		t.Errorf("expected body 'No healthy backends', got '%s'", body)
	}
}

func TestRouter_NotFound(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	req := httptest.NewRequest("POST", "/", nil)
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestRouter_MethodNotAllowed(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	req := httptest.NewRequest("GET", "/ethereum", nil)
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", w.Code)
	}
}

func TestRouter_WebSocketNotEnabled(t *testing.T) {
	cfg := createTestConfig()
	cfg.HasWebSocket = false
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	req := httptest.NewRequest("GET", "/ethereum", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}

	body := strings.TrimSpace(w.Body.String())
	if body != "WebSocket not enabled" {
		t.Errorf("expected body 'WebSocket not enabled', got '%s'", body)
	}
}

func TestRouter_ValidJSONRPCRequest(t *testing.T) {
	cfg := createTestConfig()

	cfg.Cache.Enabled = false

	storeCfg := backend.CacheConfig{
		Enabled:       false,
		Path:          getUniqueTestCachePath(t),
		MemEntries:    1000,
		TTLLatest:     250 * time.Millisecond,
		TTLBlock:      24 * time.Hour,
		Clean:         true,
		MaxReorgDepth: 100,
	}
	store, err := backend.Open(storeCfg)
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer cleanupTestStore(t, store)

	mockResponse := `{"jsonrpc":"2.0","id":"1","result":"0x1234"}`
	backend1 := cfg.Backends[0]
	backend2 := cfg.Backends[1]

	backend1.HealthUp.Store(true)
	backend2.HealthUp.Store(true)

	mockClient1 := backend1.Client.Transport.(*backend.MockHTTPClient)
	mockClient2 := backend2.Client.Transport.(*backend.MockHTTPClient)

	mockClient1.ClearRequests()
	mockClient2.ClearRequests()

	mockClient1.SetResponse("https://api1.example.com", backend.MockResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(mockResponse),
	})
	mockClient2.SetResponse("https://api2.example.com", backend.MockResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(mockResponse),
	})

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	requestBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "eth_blockNumber",
		"params":  []any{},
	}

	bodyBytes, _ := json.Marshal(requestBody)
	req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	responseBody := w.Body.String()
	if !strings.Contains(responseBody, "0x1234") {
		t.Errorf("expected response to contain '0x1234', got %s", responseBody)
	}

	requests1 := mockClient1.GetRequests()
	requests2 := mockClient2.GetRequests()
	totalRequests := len(requests1) + len(requests2)

	if totalRequests == 0 {
		t.Errorf("expected request to be made to backend. Response code: %d, Response body: %s", w.Code, w.Body.String())
	}

	var requests []backend.MockRequest
	if len(requests1) > 0 {
		requests = requests1
	} else if len(requests2) > 0 {
		requests = requests2
	}

	if len(requests) > 0 {
		request := requests[0]
		if request.Method != "POST" {
			t.Errorf("expected POST request, got %s", request.Method)
		}
		if request.URL != "https://api1.example.com" && request.URL != "https://api2.example.com" {
			t.Errorf("expected request to api1.example.com or api2.example.com, got %s", request.URL)
		}
		if request.Headers.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", request.Headers.Get("Content-Type"))
		}
	}
}

func TestRouter_InvalidJSONRPCRequest(t *testing.T) {
	cfg := createTestConfig()

	cfg.Cache.Enabled = false

	storeCfg := backend.CacheConfig{
		Enabled:       false,
		Path:          getUniqueTestCachePath(t),
		MemEntries:    1000,
		TTLLatest:     250 * time.Millisecond,
		TTLBlock:      24 * time.Hour,
		Clean:         true,
		MaxReorgDepth: 100,
	}
	store, err := backend.Open(storeCfg)
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	req := httptest.NewRequest("POST", "/ethereum", strings.NewReader("invalid json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for invalid JSON, got %d", w.Code)
	}

	responseBody := w.Body.String()
	if !strings.Contains(responseBody, "error") {
		t.Errorf("expected error response, got %s", responseBody)
	}
}

func TestRouter_RequestCountIncrement(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	initialCount := TotalReq.Load()

	requestBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "eth_blockNumber",
		"params":  []any{},
	}

	bodyBytes, _ := json.Marshal(requestBody)
	req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	finalCount := TotalReq.Load()
	if finalCount <= initialCount {
		t.Errorf("expected request count to increase, got %d -> %d", initialCount, finalCount)
	}
}

func TestRouter_BufferPoolReuse(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	for i := range 5 {
		requestBody := map[string]any{
			"jsonrpc": "2.0",
			"id":      fmt.Sprintf("%d", i),
			"method":  "eth_blockNumber",
			"params":  []any{},
		}

		bodyBytes, _ := json.Marshal(requestBody)
		req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		server.Handler.ServeHTTP(w, req)

		if w.Code == 0 {
			t.Errorf("expected response to be written for request %d", i)
		}
	}
}

func TestRouter_ChainRouting(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	testChains := []string{"ethereum", "base", "polygon"}

	for _, chain := range testChains {
		requestBody := map[string]any{
			"jsonrpc": "2.0",
			"id":      "1",
			"method":  "eth_blockNumber",
			"params":  []any{},
		}

		bodyBytes, _ := json.Marshal(requestBody)
		req := httptest.NewRequest("POST", "/"+chain, bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		server.Handler.ServeHTTP(w, req)

		if w.Code == 0 {
			t.Errorf("expected response to be written for chain %s", chain)
		}
	}
}

func TestRouter_HeadersPreservation(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	requestBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "eth_blockNumber",
		"params":  []any{},
	}

	bodyBytes, _ := json.Marshal(requestBody)
	req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Custom-Header", "test-value")
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code == 0 {
		t.Error("expected response to be written")
	}
}

func TestRouter_ContextCancellation(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	requestBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "eth_blockNumber",
		"params":  []interface{}{},
	}

	bodyBytes, _ := json.Marshal(requestBody)
	req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(bodyBytes))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code == 0 {
		t.Error("expected response to be written")
	}
}

func TestRouter_LargeRequestBody(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	largeParams := make([]string, 1000)
	for i := range largeParams {
		largeParams[i] = fmt.Sprintf("param-%d", i)
	}

	requestBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "eth_call",
		"params":  largeParams,
	}

	bodyBytes, _ := json.Marshal(requestBody)
	req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code == 0 {
		t.Error("expected response to be written")
	}
}

func TestRouter_ConcurrentRequests(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	done := make(chan bool, 10)

	for i := range 5 {
		go func(id int) {
			requestBody := map[string]any{
				"jsonrpc": "2.0",
				"id":      fmt.Sprintf("%d", id),
				"method":  "eth_blockNumber",
				"params":  []any{},
			}

			bodyBytes, _ := json.Marshal(requestBody)
			req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			server.Handler.ServeHTTP(w, req)

			if w.Code == 0 {
				t.Errorf("expected response to be written for request %d", id)
			}

			done <- true
		}(i)
	}

	for range 5 {
		<-done
	}
}

func TestRouter_JSONRPCBatchRequest(t *testing.T) {
	cfg := createTestConfig()
	cfg.Cache.Enabled = false

	storeCfg := backend.CacheConfig{
		Enabled:       false,
		Path:          getUniqueTestCachePath(t),
		MemEntries:    1000,
		TTLLatest:     250 * time.Millisecond,
		TTLBlock:      24 * time.Hour,
		Clean:         true,
		MaxReorgDepth: 100,
	}
	store, err := backend.Open(storeCfg)
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer cleanupTestStore(t, store)

	mockBatchResponse := `[{"jsonrpc":"2.0","id":1,"result":"0x1234"},{"jsonrpc":"2.0","id":2,"result":"0x1"}]`
	backend1 := cfg.Backends[0]
	backend2 := cfg.Backends[1]

	backend1.HealthUp.Store(true)
	backend2.HealthUp.Store(true)

	mockClient1 := backend1.Client.Transport.(*backend.MockHTTPClient)
	mockClient2 := backend2.Client.Transport.(*backend.MockHTTPClient)

	mockClient1.ClearRequests()
	mockClient2.ClearRequests()

	mockClient1.SetResponse("https://api1.example.com", backend.MockResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(mockBatchResponse),
	})
	mockClient2.SetResponse("https://api2.example.com", backend.MockResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(mockBatchResponse),
	})

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	batchRequestBody := []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "eth_blockNumber", "params": []any{}},
		{"jsonrpc": "2.0", "id": 2, "method": "eth_chainId", "params": []any{}},
	}

	bodyBytes, _ := json.Marshal(batchRequestBody)
	req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d. Response body: %s", w.Code, w.Body.String())
	}

	responseBody := w.Body.String()
	if !strings.Contains(responseBody, "0x1234") {
		t.Errorf("expected response to contain '0x1234', got %s", responseBody)
	}
	if !strings.Contains(responseBody, "0x1") {
		t.Errorf("expected response to contain '0x1', got %s", responseBody)
	}

	if !strings.HasPrefix(strings.TrimSpace(responseBody), "[") {
		t.Errorf("expected batch response to be an array, got %s", responseBody)
	}

	requests1 := mockClient1.GetRequests()
	requests2 := mockClient2.GetRequests()
	totalRequests := len(requests1) + len(requests2)

	if totalRequests == 0 {
		t.Errorf("expected request to be made to backend. Response code: %d, Response body: %s", w.Code, w.Body.String())
	}

	var requests []backend.MockRequest
	if len(requests1) > 0 {
		requests = requests1
	} else if len(requests2) > 0 {
		requests = requests2
	}

	if len(requests) > 0 {
		request := requests[0]
		if request.Method != "POST" {
			t.Errorf("expected POST request, got %s", request.Method)
		}
		var requestBody []any
		if err := json.Unmarshal(request.Body, &requestBody); err != nil {
			t.Errorf("expected batch request body to be an array, got error: %v", err)
		}
		if len(requestBody) != 2 {
			t.Errorf("expected batch request to have 2 items, got %d", len(requestBody))
		}
	}
}

func TestRouter_JSONRPCBatchRequest_EmptyBatch(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	emptyBatch := []any{}
	bodyBytes, _ := json.Marshal(emptyBatch)
	req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for empty batch, got %d", w.Code)
	}

	responseBody := w.Body.String()
	if !strings.Contains(responseBody, "error") {
		t.Errorf("expected error response, got %s", responseBody)
	}
}

func TestRouter_JSONRPCBatchRequest_InvalidJSON(t *testing.T) {
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	req := httptest.NewRequest("POST", "/ethereum", strings.NewReader("[invalid json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for invalid JSON batch, got %d", w.Code)
	}

	responseBody := w.Body.String()
	if !strings.Contains(responseBody, "error") {
		t.Errorf("expected error response, got %s", responseBody)
	}
}

// TestRouter_XCacheHeader locks the cache-observability contract: sabre must
// label every response with X-Cache (miss on the upstream path, hit when
// served from cache) so gusher's gusher_sabre_requests_total{cache} metric —
// and the cache panel on the RPC dashboard — populates correctly.
func TestRouter_XCacheHeader(t *testing.T) {
	// The on-disk pebble cache persists between runs, so start from a clean
	// directory — this test asserts a miss-then-hit transition that a stale
	// entry would defeat.
	_ = os.RemoveAll(getUniqueTestCachePath(t))

	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	mockResponse := `{"jsonrpc":"2.0","id":"1","result":"0x1234"}`
	for _, b := range cfg.Backends {
		b.HealthUp.Store(true)
		mc := b.Client.Transport.(*backend.MockHTTPClient)
		mc.ClearRequests()
		mc.SetResponse(b.URL.String(), backend.MockResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(mockResponse),
		})
	}

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	// eth_getBlockByNumber pinned to a concrete block number → cacheable
	// with the long block TTL, so the second call must be served from cache.
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "eth_getBlockByNumber",
		"params":  []any{"0x1", false},
	})

	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)
		return w
	}

	upstreamCalls := func() int {
		n := 0
		for _, b := range cfg.Backends {
			n += len(b.Client.Transport.(*backend.MockHTTPClient).GetRequests())
		}
		return n
	}

	w1 := do()
	if w1.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", w1.Code)
	}
	if got := w1.Header().Get("X-Cache"); got != "miss" {
		t.Errorf("first call: expected X-Cache=miss, got %q", got)
	}
	afterFirst := upstreamCalls()
	if afterFirst == 0 {
		t.Fatalf("first call should have reached an upstream")
	}

	w2 := do()
	if w2.Code != http.StatusOK {
		t.Fatalf("second call: expected 200, got %d", w2.Code)
	}
	if got := w2.Header().Get("X-Cache"); got != "hit" {
		t.Errorf("second call: expected X-Cache=hit, got %q", got)
	}
	if afterSecond := upstreamCalls(); afterSecond != afterFirst {
		t.Errorf("cache hit should not reach upstream: before=%d after=%d", afterFirst, afterSecond)
	}
}

// TestRouter_ImmutableMulticallCachedAcrossBlocks proves the cross-reset win
// end-to-end through the HTTP router: an aggregate3 multicall of immutable
// reads (decimals) misses upstream once, then a content-identical multicall
// pinned to a DIFFERENT block is served entirely from cache — and the
// synthesized response reproduces the upstream result byte-for-byte.
func TestRouter_ImmutableMulticallCachedAcrossBlocks(t *testing.T) {
	_ = os.RemoveAll(getUniqueTestCachePath(t))
	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	// aggregate3([{ target=0x..aa, allowFailure=true, callData=decimals() }])
	reqData := "0x82ad56cb" +
		"0000000000000000000000000000000000000000000000000000000000000020" + // array offset
		"0000000000000000000000000000000000000000000000000000000000000001" + // n = 1
		"0000000000000000000000000000000000000000000000000000000000000020" + // tuple[0] offset
		"00000000000000000000000000000000000000000000000000000000000000aa" + // target
		"0000000000000000000000000000000000000000000000000000000000000001" + // allowFailure
		"0000000000000000000000000000000000000000000000000000000000000060" + // callData offset
		"0000000000000000000000000000000000000000000000000000000000000004" + // callData len
		"313ce56700000000000000000000000000000000000000000000000000000000" // decimals() selector

	// aggregate3 result: [{ success=true, returnData=uint256(18) }]
	resultHex := "0x" +
		"0000000000000000000000000000000000000000000000000000000000000020" + // array offset
		"0000000000000000000000000000000000000000000000000000000000000001" + // n = 1
		"0000000000000000000000000000000000000000000000000000000000000020" + // elem[0] offset
		"0000000000000000000000000000000000000000000000000000000000000001" + // success
		"0000000000000000000000000000000000000000000000000000000000000040" + // returnData offset
		"0000000000000000000000000000000000000000000000000000000000000020" + // returnData len
		"0000000000000000000000000000000000000000000000000000000000000012" // 18

	upstreamBody := `{"jsonrpc":"2.0","id":1,"result":"` + resultHex + `"}`
	for _, b := range cfg.Backends {
		b.HealthUp.Store(true)
		mc := b.Client.Transport.(*backend.MockHTTPClient)
		mc.ClearRequests()
		mc.SetResponse(b.URL.String(), backend.MockResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(upstreamBody),
		})
	}

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	call := func(blockTag string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": "1", "method": "eth_call",
			"params": []any{map[string]string{"to": "0x00000000000000000000000000000000000000bb", "data": reqData}, blockTag},
		})
		req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)
		return w
	}

	upstreamCalls := func() int {
		n := 0
		for _, b := range cfg.Backends {
			n += len(b.Client.Transport.(*backend.MockHTTPClient).GetRequests())
		}
		return n
	}

	// First call at "latest": miss → upstream → sub-calls cached.
	w1 := call("latest")
	if w1.Code != http.StatusOK || w1.Header().Get("X-Cache") != "miss" {
		t.Fatalf("first call: code=%d x-cache=%q", w1.Code, w1.Header().Get("X-Cache"))
	}
	if upstreamCalls() == 0 {
		t.Fatal("first call must reach upstream")
	}

	// Second call at a DIFFERENT block: served from the immutable sub-call cache.
	before := upstreamCalls()
	w2 := call("0x1312d00")
	if w2.Code != http.StatusOK || w2.Header().Get("X-Cache") != "hit" {
		t.Fatalf("second call must be a cache hit: code=%d x-cache=%q", w2.Code, w2.Header().Get("X-Cache"))
	}
	if upstreamCalls() != before {
		t.Errorf("cache hit must not reach upstream: before=%d after=%d", before, upstreamCalls())
	}

	// The synthesized result must reproduce the upstream result exactly.
	var r1, r2 struct {
		Result string `json:"result"`
	}
	_ = json.Unmarshal(w1.Body.Bytes(), &r1)
	_ = json.Unmarshal(w2.Body.Bytes(), &r2)
	if r2.Result != r1.Result || r2.Result != resultHex {
		t.Fatalf("synthesized result mismatch:\n upstream=%s\n cached  =%s", r1.Result, r2.Result)
	}
}

// TestWriteHopSafeHeaders_StripsBodyDescriptors guards the multicall-merge EOF bug:
// sabre rewrites the body on cached/merged/passthrough paths, so copying the
// upstream Content-Length/Content-Encoding strands a stale length on a transformed
// body and truncates the write. Both must be dropped so net/http recomputes them.
func TestWriteHopSafeHeaders_StripsBodyDescriptors(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Length", "37350") // upstream reduced-multicall length
	src.Set("Content-Encoding", "gzip")
	src.Set("Content-Type", "application/json")
	src.Set("X-Upstream-Thing", "keep-me")
	src.Set("Connection", "keep-alive") // hop-by-hop, also dropped

	rec := httptest.NewRecorder()
	writeHopSafeHeaders(rec, src)
	h := rec.Header()

	if got := h.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length must be stripped, got %q", got)
	}
	if got := h.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding must be stripped, got %q", got)
	}
	if got := h.Get("Connection"); got != "" {
		t.Errorf("Connection (hop-by-hop) must be stripped, got %q", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type must pass through, got %q", got)
	}
	if got := h.Get("X-Upstream-Thing"); got != "keep-me" {
		t.Errorf("end-to-end headers must pass through, got %q", got)
	}
}

// TestRouter_CacheHitServesCurrentRequestID guards against cache id leakage on
// the single-request path: the first client's response is cached with its id
// embedded, and the second client — same call, different id — must receive the
// cached result stamped with ITS OWN id, not the original requester's.
func TestRouter_CacheHitServesCurrentRequestID(t *testing.T) {
	_ = os.RemoveAll(getUniqueTestCachePath(t))

	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	mockResponse := `{"jsonrpc":"2.0","id":1,"result":"0x1234"}`
	for _, b := range cfg.Backends {
		b.HealthUp.Store(true)
		mc := b.Client.Transport.(*backend.MockHTTPClient)
		mc.ClearRequests()
		mc.SetResponse(b.URL.String(), backend.MockResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(mockResponse),
		})
	}

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	do := func(id any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  "eth_getBlockByNumber",
			"params":  []any{"0x1", false},
		})
		req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)
		return w
	}

	w1 := do(1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first client: status = %d, want 200", w1.Code)
	}
	if got := w1.Header().Get("X-Cache"); got != "miss" {
		t.Fatalf("first client: X-Cache = %q, want miss", got)
	}

	w2 := do("client-two")
	if w2.Code != http.StatusOK {
		t.Fatalf("second client: status = %d, want 200", w2.Code)
	}
	if got := w2.Header().Get("X-Cache"); got != "hit" {
		t.Fatalf("second client: X-Cache = %q, want hit", got)
	}

	var resp struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", w2.Body.String(), err)
	}
	if string(resp.ID) != `"client-two"` {
		t.Errorf("second client got id %s, want %q", resp.ID, `"client-two"`)
	}
	if string(resp.Result) != `"0x1234"` {
		t.Errorf("second client got result %s, want %q", resp.Result, `"0x1234"`)
	}
}

// TestRouter_BatchCacheHitServesCurrentRequestIDs guards the batch path: a
// batch with a mixed cache hit and miss must preserve the positional id
// mapping, with the hit stamped with the CURRENT batch item's id rather than
// the id of whichever client originally populated the cache entry.
func TestRouter_BatchCacheHitServesCurrentRequestIDs(t *testing.T) {
	_ = os.RemoveAll(getUniqueTestCachePath(t))

	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	// Another client populated the cache entry under its own id.
	store.Store("ethereum", "eth_getBlockByNumber", json.RawMessage(`["0x1",false]`),
		[]byte(`{"jsonrpc":"2.0","id":"original-client","result":"0xcafe"}`), &cfg.Subscriptions)

	// Upstream serves only the reduced (miss-only) batch: the eth_blockNumber item.
	upstreamBody := `[{"jsonrpc":"2.0","id":8,"result":"0x10"}]`
	for _, b := range cfg.Backends {
		b.HealthUp.Store(true)
		mc := b.Client.Transport.(*backend.MockHTTPClient)
		mc.ClearRequests()
		mc.SetResponse(b.URL.String(), backend.MockResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(upstreamBody),
		})
	}

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	body, _ := json.Marshal([]map[string]any{
		{"jsonrpc": "2.0", "id": 7, "method": "eth_getBlockByNumber", "params": []any{"0x1", false}},
		{"jsonrpc": "2.0", "id": 8, "method": "eth_blockNumber", "params": []any{}},
	})
	req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("batch: status = %d, want 200. body: %s", w.Code, w.Body.String())
	}

	var responses []struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &responses); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", w.Body.String(), err)
	}
	if len(responses) != 2 {
		t.Fatalf("batch returned %d responses, want 2. body: %s", len(responses), w.Body.String())
	}

	if string(responses[0].ID) != `7` {
		t.Errorf("cached item id = %s, want 7", responses[0].ID)
	}
	if string(responses[0].Result) != `"0xcafe"` {
		t.Errorf("cached item result = %s, want %q", responses[0].Result, `"0xcafe"`)
	}
	if string(responses[1].ID) != `8` {
		t.Errorf("uncached item id = %s, want 8", responses[1].ID)
	}
	if string(responses[1].Result) != `"0x10"` {
		t.Errorf("uncached item result = %s, want %q", responses[1].Result, `"0x10"`)
	}
}

// TestRouter_BatchAllCachedServesCurrentRequestIDs covers the fully-cached
// batch short-circuit, which serves cachedResponses without any upstream trip.
func TestRouter_BatchAllCachedServesCurrentRequestIDs(t *testing.T) {
	_ = os.RemoveAll(getUniqueTestCachePath(t))

	cfg := createTestConfig()
	store := createTestStore(t)
	defer cleanupTestStore(t, store)

	store.Store("ethereum", "eth_getBlockByNumber", json.RawMessage(`["0x1",false]`),
		[]byte(`{"jsonrpc":"2.0","id":"a","result":"0xcafe"}`), &cfg.Subscriptions)
	store.Store("ethereum", "eth_getBlockByNumber", json.RawMessage(`["0x2",false]`),
		[]byte(`{"jsonrpc":"2.0","id":"b","result":"0xbeef"}`), &cfg.Subscriptions)

	for _, b := range cfg.Backends {
		b.HealthUp.Store(true)
		b.Client.Transport.(*backend.MockHTTPClient).ClearRequests()
	}

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	body, _ := json.Marshal([]map[string]any{
		{"jsonrpc": "2.0", "id": 101, "method": "eth_getBlockByNumber", "params": []any{"0x1", false}},
		{"jsonrpc": "2.0", "id": 102, "method": "eth_getBlockByNumber", "params": []any{"0x2", false}},
	})
	req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("batch: status = %d, want 200. body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Cache"); got != "hit" {
		t.Fatalf("batch: X-Cache = %q, want hit", got)
	}

	var responses []struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &responses); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", w.Body.String(), err)
	}
	if len(responses) != 2 {
		t.Fatalf("batch returned %d responses, want 2. body: %s", len(responses), w.Body.String())
	}
	if string(responses[0].ID) != `101` || string(responses[0].Result) != `"0xcafe"` {
		t.Errorf("item 0 = (id %s, result %s), want (101, %q)", responses[0].ID, responses[0].Result, `"0xcafe"`)
	}
	if string(responses[1].ID) != `102` || string(responses[1].Result) != `"0xbeef"` {
		t.Errorf("item 1 = (id %s, result %s), want (102, %q)", responses[1].ID, responses[1].Result, `"0xbeef"`)
	}

	for _, b := range cfg.Backends {
		if n := len(b.Client.Transport.(*backend.MockHTTPClient).GetRequests()); n != 0 {
			t.Errorf("fully-cached batch reached upstream %s %d times, want 0", b.Name, n)
		}
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newEchoBackend returns a healthy mock backend whose upstream answers every
// request with a result identifying the request's method and params, and
// returns batch responses in a SHUFFLED order — spec-legal upstream behavior
// that exposes any positional or id-keyed response misrouting. The shuffle is
// deterministically seeded so runs are reproducible.
func newEchoBackend(t *testing.T) *backend.Backend {
	t.Helper()
	var shuffleMu sync.Mutex
	shuffleRng := mrand.New(mrand.NewPCG(0x5ab3e, 0xc0ffee))
	bk := backend.CreateMockBackend("echo", "ethereum", "https://echo.example.com")
	bk.HealthUp.Store(true)
	bk.Client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		type rpcMsg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		respond := func(m rpcMsg) json.RawMessage {
			result, _ := json.Marshal(fmt.Sprintf("m=%s;p=%s", m.Method, string(m.Params)))
			out, _ := json.Marshal(map[string]json.RawMessage{
				"jsonrpc": json.RawMessage(`"2.0"`),
				"id":      m.ID,
				"result":  result,
			})
			return out
		}
		var out []byte
		var batch []rpcMsg
		if json.Unmarshal(body, &batch) == nil {
			resps := make([]json.RawMessage, len(batch))
			for i, m := range batch {
				resps[i] = respond(m)
			}
			shuffleMu.Lock()
			shuffleRng.Shuffle(len(resps), func(i, j int) { resps[i], resps[j] = resps[j], resps[i] })
			shuffleMu.Unlock()
			out, _ = json.Marshal(resps)
		} else {
			var single rpcMsg
			if err := json.Unmarshal(body, &single); err != nil {
				return nil, err
			}
			out = respond(single)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(out)),
			Request:    r,
		}, nil
	})}
	return bk
}

// TestRouter_ConcurrentMixedMethodsNoCrossTalk is the router-level regression
// test for response cross-talk: many concurrent clients issuing mixed-method
// singles and batches that all reuse the same JSON-RPC ids, coalesced by the
// batch processor into shared upstream batches answered out of order. Every
// response payload must belong to its own request. Run with -race this doubles
// as the stress variant.
func TestRouter_ConcurrentMixedMethodsNoCrossTalk(t *testing.T) {
	cfg := createTestConfig()
	cfg.Cache.Enabled = true
	cfg.Cache.Path = getUniqueTestCachePath(t)
	cfg.Batch.Enabled = true
	cfg.Batch.MaxBatchSize = 10
	cfg.Batch.MaxBatchWaitTime = 5 * time.Millisecond
	cfg.BatchProcessor = backend.NewBatchProcessor(10, 5*time.Millisecond, 8, 1, backend.MulticallConfig{})
	cfg.Backends = []*backend.Backend{newEchoBackend(t)}
	cfg.BackendsCt = map[string]int{"ethereum": 1}

	store, err := backend.Open(cfg.Cache)
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	methods := []string{"eth_call", "eth_getLogs", "eth_getBlockByNumber", "eth_getStorageAt"}

	var wg sync.WaitGroup
	var crossTalk atomic.Int64
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				n := 1 + (g+i)%4
				type item struct{ method, params string }
				items := make([]item, n)
				reqs := make([]map[string]any, n)
				for j := 0; j < n; j++ {
					m := methods[(g+i+j)%len(methods)]
					p := fmt.Sprintf(`["0x%d%d%d"]`, g, i, j)
					if j%3 == 0 {
						p = `["0xshared"]`
					}
					items[j] = item{m, p}
					reqs[j] = map[string]any{
						"jsonrpc": "2.0", "id": j + 1, "method": m,
						"params": json.RawMessage(p),
					}
				}
				var body []byte
				if n == 1 {
					body, _ = json.Marshal(reqs[0])
				} else {
					body, _ = json.Marshal(reqs)
				}
				req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				server.Handler.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Errorf("goroutine %d iter %d: status %d body %s", g, i, w.Code, w.Body.String())
					continue
				}
				type rpcResp struct {
					ID     json.RawMessage `json:"id"`
					Result json.RawMessage `json:"result"`
					Error  json.RawMessage `json:"error"`
				}
				var responses []rpcResp
				if n == 1 {
					var one rpcResp
					if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil {
						t.Errorf("goroutine %d iter %d: unmarshal: %v", g, i, err)
						continue
					}
					responses = []rpcResp{one}
				} else if err := json.Unmarshal(w.Body.Bytes(), &responses); err != nil || len(responses) != n {
					t.Errorf("goroutine %d iter %d: bad batch response: %v %s", g, i, err, w.Body.String())
					continue
				}
				for j, r := range responses {
					if len(r.Error) > 0 && string(r.Error) != "null" {
						t.Errorf("goroutine %d iter %d item %d: rpc error %s", g, i, j, r.Error)
						continue
					}
					if string(r.ID) != fmt.Sprintf("%d", j+1) {
						t.Errorf("goroutine %d iter %d item %d: id %s, want %d", g, i, j, r.ID, j+1)
					}
					var got string
					if err := json.Unmarshal(r.Result, &got); err != nil {
						crossTalk.Add(1)
						t.Errorf("goroutine %d iter %d item %d (%s): malformed payload %s", g, i, j, items[j].method, r.Result)
						continue
					}
					want := fmt.Sprintf("m=%s;p=%s", items[j].method, items[j].params)
					if got != want {
						crossTalk.Add(1)
						t.Errorf("goroutine %d iter %d item %d: CROSS-TALK — sent %s %s, got payload %q",
							g, i, j, items[j].method, items[j].params, got)
					}
				}
			}
		}(g)
	}
	wg.Wait()

	if n := crossTalk.Load(); n > 0 {
		t.Fatalf("%d cross-talk events: responses delivered to the wrong requests", n)
	}
}

// TestRouter_BatchUpstreamOutOfOrderResponses covers the raw (batching
// disabled) forwarding path: a JSON-RPC batch answered out of order by the
// upstream must be re-associated with the right requests before positional
// merging and caching — and a repeat of the same batch must be served from
// cache with the correct payloads (no poisoning).
func TestRouter_BatchUpstreamOutOfOrderResponses(t *testing.T) {
	cfg := createTestConfig()
	cfg.Cache.Enabled = true
	cfg.Cache.Path = getUniqueTestCachePath(t)
	cfg.Backends = []*backend.Backend{newEchoBackend(t)}
	cfg.BackendsCt = map[string]int{"ethereum": 1}

	store, err := backend.Open(cfg.Cache)
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	items := []struct{ method, params string }{
		{"eth_call", `["0xaaa1"]`},
		{"eth_getLogs", `["0xaaa2"]`},
		{"eth_getStorageAt", `["0xaaa3"]`},
		{"eth_getBlockByNumber", `["0xaaa4"]`},
	}
	reqs := make([]map[string]any, len(items))
	for j, it := range items {
		reqs[j] = map[string]any{
			"jsonrpc": "2.0", "id": j + 1, "method": it.method,
			"params": json.RawMessage(it.params),
		}
	}
	body, _ := json.Marshal(reqs)

	passes := []struct{ pass, wantCache string }{{"upstream", "miss"}, {"cached", "hit"}}
	for _, p := range passes {
		pass, wantCache := p.pass, p.wantCache
		req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("%s pass: status %d body %s", pass, w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-Cache"); got != wantCache {
			t.Errorf("%s pass: X-Cache %q, want %q", pass, got, wantCache)
		}

		var responses []struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &responses); err != nil || len(responses) != len(items) {
			t.Fatalf("%s pass: bad batch response: %v %s", pass, err, w.Body.String())
		}
		for j, r := range responses {
			if string(r.ID) != fmt.Sprintf("%d", j+1) {
				t.Errorf("%s pass item %d: id %s, want %d", pass, j, r.ID, j+1)
			}
			var got string
			if err := json.Unmarshal(r.Result, &got); err != nil {
				t.Fatalf("%s pass item %d: malformed payload %s", pass, j, r.Result)
			}
			want := fmt.Sprintf("m=%s;p=%s", items[j].method, items[j].params)
			if got != want {
				t.Errorf("%s pass item %d: CROSS-TALK — sent %s %s, got payload %q",
					pass, j, items[j].method, items[j].params, got)
			}
		}
	}
}

// newRevertingBackend returns a healthy mock backend whose upstream answers
// eth_call with a legitimate JSON-RPC execution-revert error (code 3 with
// revert data) and every other method with a plain result.
func newRevertingBackend(t *testing.T) *backend.Backend {
	t.Helper()
	bk := backend.CreateMockBackend("reverter", "ethereum", "https://reverter.example.com")
	bk.HealthUp.Store(true)
	bk.Client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		type rpcMsg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		respond := func(m rpcMsg) json.RawMessage {
			if m.Method == "eth_call" {
				out, _ := json.Marshal(map[string]any{
					"jsonrpc": "2.0",
					"id":      m.ID,
					"error": map[string]any{
						"code":    3,
						"message": "execution reverted: probe failed",
						"data":    "0x08c379a00000000000000000000000000000000000000000000000000000000000000020",
					},
				})
				return out
			}
			out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": "0x1"})
			return out
		}
		var out []byte
		var batch []rpcMsg
		if json.Unmarshal(body, &batch) == nil {
			resps := make([]json.RawMessage, len(batch))
			for i, m := range batch {
				resps[i] = respond(m)
			}
			out, _ = json.Marshal(resps)
		} else {
			var single rpcMsg
			if err := json.Unmarshal(body, &single); err != nil {
				return nil, err
			}
			out = respond(single)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(out)),
			Request:    r,
		}, nil
	})}
	return bk
}

// TestRouter_BatchedEthCallRevertReturnsErrorEnvelope guards revert fidelity
// through the batch processor: a legitimate eth_call revert must reach the
// caller as an HTTP 200 JSON-RPC error envelope with code/message/data intact
// — never be converted into a transport failure and retried into a 502.
func TestRouter_BatchedEthCallRevertReturnsErrorEnvelope(t *testing.T) {
	cfg := createTestConfig()
	cfg.Cache.Enabled = false
	cfg.Batch.Enabled = true
	cfg.Batch.MaxBatchSize = 10
	cfg.Batch.MaxBatchWaitTime = 5 * time.Millisecond
	cfg.BatchProcessor = backend.NewBatchProcessor(10, 5*time.Millisecond, 4, 1, backend.MulticallConfig{})
	cfg.Backends = []*backend.Backend{newRevertingBackend(t)}
	cfg.BackendsCt = map[string]int{"ethereum": 1}

	storeCfg := backend.CacheConfig{Enabled: false, Path: getUniqueTestCachePath(t)}
	store, err := backend.Open(storeCfg)
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer cleanupTestStore(t, store)

	lb := backend.NewLoadBalancer(cfg)
	server := NewRouter(store, &cfg, lb)

	assertRevertEnvelope := func(t *testing.T, raw json.RawMessage, wantID string) {
		t.Helper()
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Data    string `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("response is not a JSON-RPC envelope: %v\nbody: %s", err, raw)
		}
		if string(resp.ID) != wantID {
			t.Errorf("id %s, want %s", resp.ID, wantID)
		}
		if resp.Error.Code != 3 {
			t.Errorf("error code %d, want 3", resp.Error.Code)
		}
		if resp.Error.Message != "execution reverted: probe failed" {
			t.Errorf("error message %q, want revert message", resp.Error.Message)
		}
		if resp.Error.Data != "0x08c379a00000000000000000000000000000000000000000000000000000000000000020" {
			t.Errorf("error data %q: revert data lost", resp.Error.Data)
		}
	}

	t.Run("single", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":7,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x01"},"latest"]}`)
		req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (revert must not become a transport error); body: %s", w.Code, w.Body.String())
		}
		assertRevertEnvelope(t, w.Body.Bytes(), "7")
	})

	t.Run("batch mixed", func(t *testing.T) {
		body := []byte(`[{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x01"},"latest"]},{"jsonrpc":"2.0","id":2,"method":"eth_blockNumber","params":[]}]`)
		req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var responses []json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &responses); err != nil || len(responses) != 2 {
			t.Fatalf("bad batch response: %v %s", err, w.Body.String())
		}
		assertRevertEnvelope(t, responses[0], "1")
		var ok struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(responses[1], &ok); err != nil || string(ok.Result) != `"0x1"` {
			t.Errorf("healthy neighbor disturbed: %s", responses[1])
		}
	})

	t.Run("batch all reverting", func(t *testing.T) {
		body := []byte(`[{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x01"},"latest"]},{"jsonrpc":"2.0","id":2,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000002","data":"0x02"},"latest"]}]`)
		req := httptest.NewRequest("POST", "/ethereum", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (all-revert batch must not become a transport error); body: %s", w.Code, w.Body.String())
		}
		var responses []json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &responses); err != nil || len(responses) != 2 {
			t.Fatalf("bad batch response: %v %s", err, w.Body.String())
		}
		assertRevertEnvelope(t, responses[0], "1")
		assertRevertEnvelope(t, responses[1], "2")
	})
}
