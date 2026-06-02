package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
		store.Close()
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
	os.RemoveAll(getUniqueTestCachePath(t))

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
	os.RemoveAll(getUniqueTestCachePath(t))
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
	var r1, r2 struct{ Result string `json:"result"` }
	_ = json.Unmarshal(w1.Body.Bytes(), &r1)
	_ = json.Unmarshal(w2.Body.Bytes(), &r2)
	if r2.Result != r1.Result || r2.Result != resultHex {
		t.Fatalf("synthesized result mismatch:\n upstream=%s\n cached  =%s", r1.Result, r2.Result)
	}
}
