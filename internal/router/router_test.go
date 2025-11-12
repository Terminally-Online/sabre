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
