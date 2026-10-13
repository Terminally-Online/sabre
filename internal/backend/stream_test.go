package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func createTestStreamConfig() Config {
	backend := &Backend{
		Name:   "test-backend",
		Chain:  "ethereum",
		URL:    &url.URL{Scheme: "https", Host: "api.example.com"},
		WSURL:  &url.URL{Scheme: "wss", Host: "api.example.com"},
		Client: &http.Client{},
	}
	backend.HealthUp.Store(true)

	return Config{
		Subscriptions: SubscriptionsConfig{
			MaxConnectionsPerBackend:      100,
			MaxSubscriptionsPerConnection: 50,
			PingInterval:                  30 * time.Second,
			PongWait:                      10 * time.Second,
			WriteWait:                     10 * time.Second,
			ReadWait:                      60 * time.Second,
			EnableCompression:             true,
			MaxMessageSize:                1048576,
		},
		Cache: CacheConfig{
			MaxReorgDepth: 100,
			Chains:        map[string]ChainCacheConfig{"ethereum": {BlockTime: 12 * time.Second}},
		},
		Backends: []*Backend{backend},
	}
}

func TestNewStream(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)

	stream := NewStream(cfg, lb)

	if stream == nil {
		t.Fatal("expected non-nil stream")
	}

	if stream.cfg.Sabre.Listen != cfg.Sabre.Listen {
		t.Error("expected config to be set")
	}

	if stream.loadBalancer != lb {
		t.Error("expected load balancer to be set")
	}

	if stream.connections == nil {
		t.Error("expected connections map to be initialized")
	}

	if stream.subscriptions == nil {
		t.Error("expected subscriptions map to be initialized")
	}

	if stream.clients == nil {
		t.Error("expected clients map to be initialized")
	}

	if stream.delivered == nil {
		t.Error("expected delivered headers map to be initialized")
	}

	if stream.lastBlockTime == nil {
		t.Error("expected last block time map to be initialized")
	}

	if !stream.upgrader.EnableCompression {
		t.Error("expected compression to be enabled")
	}

	if !stream.upgrader.CheckOrigin(&http.Request{}) {
		t.Error("expected origin check to return true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	stream.Shutdown(ctx)
}

func TestStream_Shutdown(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)

	client := &WebsocketClient{
		ID:    "test-client",
		Chain: "ethereum",
		Conn:  nil,
	}
	stream.clients["test-client"] = client

	wsConn := &WebsocketConnection{
		Backend: cfg.Backends[0],
		Conn:    nil,
	}
	stream.connections[cfg.Backends[0].WSURL.String()] = []*WebsocketConnection{wsConn}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	stream.Shutdown(ctx)

	// Verify clients and connections are marked as closed
	if !client.closed.Load() {
		t.Error("expected client to be marked as closed")
	}

	if !wsConn.closed.Load() {
		t.Error("expected connection to be marked as closed")
	}
}

func TestStream_HandleWebSocket_InvalidPath(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	stream.HandleWebSocket(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}

	if !strings.Contains(w.Body.String(), "chain path required") {
		t.Error("expected error message about chain path")
	}
}

func TestStream_HandleWebSocket_UpgradeFailure(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	req := httptest.NewRequest("GET", "/ethereum", nil)
	w := httptest.NewRecorder()

	stream.HandleWebSocket(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

func TestStream_HandleClientMessage_Subscribe(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	client := &WebsocketClient{
		ID:            "test-client",
		Chain:         "ethereum",
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}

	msg := &Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_subscribe",
		Params:  json.RawMessage(`["newHeads"]`),
	}

	stream.handleClientMessage(client, "ethereum", msg)

	stream.mu.RLock()
	subscriptionCount := len(stream.subscriptions)
	stream.mu.RUnlock()

	if subscriptionCount != 1 {
		t.Errorf("expected 1 subscription, got %d", subscriptionCount)
	}
}

func TestStream_HandleClientMessage_Unsubscribe(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	client := &WebsocketClient{
		ID:    "test-client",
		Chain: "ethereum",
		Conn:  nil,
	}
	client.Subscriptions = make(map[string]*Subscription)

	subID := "test-sub"
	subscription := &Subscription{
		ID:       subID,
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["newHeads"]`),
		ClientID: client.ID,
		Backend:  cfg.Backends[0],
		Created:  time.Now(),
	}

	client.Subscriptions[subID] = subscription
	stream.subscriptions[subID] = subscription

	msg := &Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_unsubscribe",
		Params:  json.RawMessage(`"` + subID + `"`),
	}

	stream.handleClientMessage(client, "ethereum", msg)

	if len(client.Subscriptions) != 0 {
		t.Error("expected subscription to be removed from client")
	}

	stream.mu.RLock()
	subscriptionCount := len(stream.subscriptions)
	stream.mu.RUnlock()

	if subscriptionCount != 0 {
		t.Error("expected subscription to be removed from stream")
	}
}

func TestStream_HandleClientMessage_UnsubscribeInvalidParams(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	client := &WebsocketClient{
		ID:    "test-client",
		Chain: "ethereum",
		Conn:  nil,
	}

	msg := &Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_unsubscribe",
		Params:  json.RawMessage(`[1, 2, 3]`),
	}

	stream.handleClientMessage(client, "ethereum", msg)
}

func TestStream_HandleClientMessage_UnsubscribeNotFound(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	client := &WebsocketClient{
		ID:    "test-client",
		Chain: "ethereum",
		Conn:  nil,
	}
	client.Subscriptions = make(map[string]*Subscription)

	msg := &Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_unsubscribe",
		Params:  json.RawMessage(`"non-existent-sub"`),
	}

	stream.handleClientMessage(client, "ethereum", msg)
}

func TestStream_HandleClientMessage_RPCCall(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	client := &WebsocketClient{
		ID:    "test-client",
		Chain: "ethereum",
		Conn:  nil,
	}

	msg := &Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_blockNumber",
		Params:  json.RawMessage(`[]`),
	}

	stream.handleClientMessage(client, "ethereum", msg)
}

func TestStream_PickBackend(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend, err := stream.pickBackend("ethereum")

	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	if backend == nil {
		t.Error("expected non-nil backend")
	} else {
		if backend.Chain != "ethereum" {
			t.Errorf("expected ethereum backend, got %s", backend.Chain)
		}
	}
}

func TestStream_PickBackend_NoBackends(t *testing.T) {
	cfg := createTestStreamConfig()
	cfg.Backends = []*Backend{}
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend, err := stream.pickBackend("ethereum")

	if err == nil {
		t.Error("expected error when no backends available")
	}

	if backend != nil {
		t.Error("expected nil backend when no backends available")
	}
}

func TestStream_GetOrCreateWSConnection_NewConnection(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]

	conn, err := stream.getOrCreateWSConnection(backend)

	if err == nil {
		t.Error("expected error when backend is not reachable")
	}

	if conn != nil {
		t.Error("expected nil connection when backend is not reachable")
	}
}

func TestStream_GetOrCreateWSConnection_ExistingConnection(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]
	backendURL := backend.WSURL.String()

	wsConn := &WebsocketConnection{
		Backend:       backend,
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}
	wsConn.lastPing.Store(time.Now())
	wsConn.lastPong.Store(time.Now())

	stream.connections[backendURL] = []*WebsocketConnection{wsConn}

	conn, err := stream.getOrCreateWSConnection(backend)

	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	if conn != wsConn {
		t.Error("expected existing connection to be returned")
	}
}

func TestStream_GetOrCreateWSConnection_ConnectionLimit(t *testing.T) {
	cfg := createTestStreamConfig()
	cfg.Subscriptions.MaxConnectionsPerBackend = 1
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]
	backendURL := backend.WSURL.String()

	wsConn := &WebsocketConnection{
		Backend:       backend,
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}
	wsConn.lastPing.Store(time.Now())
	wsConn.lastPong.Store(time.Now())

	for i := 0; i < cfg.Subscriptions.MaxSubscriptionsPerConnection; i++ {
		wsConn.Subscriptions[fmt.Sprintf("sub-%d", i)] = &Subscription{}
	}

	stream.connections[backendURL] = []*WebsocketConnection{wsConn}

	conn, err := stream.getOrCreateWSConnection(backend)

	if err != nil {
		t.Errorf("expected no error when connection limit reached, got %v", err)
	}

	if conn == nil {
		t.Error("expected existing connection to be returned when connection limit reached")
	}

	if conn != wsConn {
		t.Error("expected the existing connection to be returned")
	}
}

func TestStream_GetWSConnection(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]
	backendURL := backend.WSURL.String()

	wsConn := &WebsocketConnection{
		Backend:       backend,
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}

	stream.connections[backendURL] = []*WebsocketConnection{wsConn}

	found := stream.getWSConnection(backend)

	if found != wsConn {
		t.Error("expected to find the connection")
	}
}

func TestStream_GetWSConnection_NotFound(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]

	found := stream.getWSConnection(backend)

	if found != nil {
		t.Error("expected nil when connection not found")
	}
}

func TestStream_GetWSConnection_ClosedConnection(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]
	backendURL := backend.WSURL.String()

	wsConn := &WebsocketConnection{
		Backend:       backend,
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}
	wsConn.closed.Store(true)

	stream.connections[backendURL] = []*WebsocketConnection{wsConn}

	found := stream.getWSConnection(backend)

	if found != nil {
		t.Error("expected nil when connection is closed")
	}
}

func TestStream_CloseWSConnection(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]
	backendURL := backend.WSURL.String()

	wsConn := &WebsocketConnection{
		Backend:       backend,
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}

	subID := "test-sub"
	subscription := &Subscription{
		ID:       subID,
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["newHeads"]`),
		ClientID: "test-client",
		Backend:  backend,
		Created:  time.Now(),
	}
	wsConn.Subscriptions[subID] = subscription
	stream.subscriptions[subID] = subscription
	stream.connections[backendURL] = []*WebsocketConnection{wsConn}

	stream.closeWSConnection(wsConn)

	if !wsConn.closed.Load() {
		t.Error("expected connection to be marked as closed")
	}

	stream.mu.RLock()
	connections := stream.connections[backendURL]
	stream.mu.RUnlock()

	if len(connections) != 0 {
		t.Error("expected connection to be removed from connections map")
	}

	stream.mu.RLock()
	_, exists := stream.subscriptions[subID]
	stream.mu.RUnlock()

	if exists {
		t.Error("expected subscription to be removed from subscriptions map")
	}
}

func TestStream_RemoveClient(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	client := &WebsocketClient{
		ID:            "test-client",
		Chain:         "ethereum",
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}

	subID := "test-sub"
	subscription := &Subscription{
		ID:       subID,
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["newHeads"]`),
		ClientID: client.ID,
		Backend:  cfg.Backends[0],
		Created:  time.Now(),
	}
	client.Subscriptions[subID] = subscription
	stream.subscriptions[subID] = subscription
	stream.clients[client.ID] = client

	stream.removeClient(client.ID)

	if !client.closed.Load() {
		t.Error("expected client to be marked as closed")
	}

	stream.mu.RLock()
	_, exists := stream.clients[client.ID]
	stream.mu.RUnlock()

	if exists {
		t.Error("expected client to be removed from clients map")
	}

	stream.mu.RLock()
	_, exists = stream.subscriptions[subID]
	stream.mu.RUnlock()

	if exists {
		t.Error("expected subscription to be removed from subscriptions map")
	}
}

func TestStream_RemoveClient_NotFound(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	stream.removeClient("non-existent-client")
}

func TestStream_SendToClient(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	client := &WebsocketClient{
		ID:    "test-client",
		Chain: "ethereum",
		Conn:  nil,
	}

	msg := &Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Result:  "test-result",
	}

	stream.sendToClient(client, msg)
}

func TestStream_SendError(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	client := &WebsocketClient{
		ID:    "test-client",
		Chain: "ethereum",
		Conn:  nil,
	}

	stream.sendError(client, json.RawMessage(`"1"`), -32603, "Test error")
}

func TestStream_ForwardBackendMessage_Subscription(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]

	wsConn := &WebsocketConnection{
		Backend:       backend,
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}

	subID := "test-sub"
	subscription := &Subscription{
		ID:       subID,
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["newHeads"]`),
		ClientID: "test-client",
		Backend:  backend,
		Created:  time.Now(),
	}
	wsConn.Subscriptions[subID] = subscription
	stream.subscriptions[subID] = subscription

	client := &WebsocketClient{
		ID:            "test-client",
		Chain:         "ethereum",
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}
	stream.clients[client.ID] = client

	params := []any{subID, map[string]any{"number": "0x1234"}}
	paramsBytes, _ := json.Marshal(params)

	msg := &Message{
		JSONRPC: "2.0",
		Method:  "eth_subscription",
		Params:  paramsBytes,
	}

	stream.forwardBackendMessage(wsConn, msg)
}

func TestStream_ForwardBackendMessage_BlockNotification(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]

	wsConn := &WebsocketConnection{
		Backend:       backend,
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}

	subID := "test-sub"
	subscription := &Subscription{
		ID:       subID,
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["newHeads"]`),
		ClientID: "test-client",
		Backend:  backend,
		Created:  time.Now(),
	}
	wsConn.Subscriptions[subID] = subscription
	stream.subscriptions[subID] = subscription

	blockData := map[string]any{
		"number": "0x1234",
		"hash":   "0xabcd",
	}
	params := []any{subID, blockData}
	paramsBytes, _ := json.Marshal(params)

	msg := &Message{
		JSONRPC: "2.0",
		Method:  "eth_subscription",
		Params:  paramsBytes,
	}

	stream.forwardBackendMessage(wsConn, msg)
}

func TestStream_ForwardBackendMessage_InvalidFormat(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]

	wsConn := &WebsocketConnection{
		Backend:       backend,
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}

	msg := &Message{
		JSONRPC: "2.0",
		Method:  "eth_subscription",
		Params:  json.RawMessage(`"invalid"`), // Should be an array
	}

	stream.forwardBackendMessage(wsConn, msg)
}

func TestStream_HandleBlockNotification(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]

	blockData := map[string]any{
		"result": map[string]any{
			"number": "0x1234",
			"hash":   "0xabcd",
		},
	}
	blockDataBytes, _ := json.Marshal(blockData)

	stream.handleBlockNotification("ethereum", backend, blockDataBytes)

	backendKey := fmt.Sprintf("%s:%s", "ethereum", backend.WSURL.String())
	stream.lastBlockMu.RLock()
	_, exists := stream.lastBlockTime[backendKey]
	stream.lastBlockMu.RUnlock()

	if !exists {
		t.Error("expected block time to be recorded")
	}

	stream.deliveredMu.Lock()
	hash := stream.delivered["ethereum"][0x1234]
	stream.deliveredMu.Unlock()

	if hash != "0xabcd" {
		t.Errorf("delivered hash at 0x1234 = %q, want 0xabcd", hash)
	}
}

func TestStream_HandleBlockNotification_InvalidData(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]

	blockData := map[string]any{
		"hash": "0xabcd",
	}
	blockDataBytes, _ := json.Marshal(blockData)

	stream.handleBlockNotification("ethereum", backend, blockDataBytes)

	backendKey := fmt.Sprintf("%s:%s", "ethereum", backend.WSURL.String())
	stream.lastBlockMu.RLock()
	_, exists := stream.lastBlockTime[backendKey]
	stream.lastBlockMu.RUnlock()

	if exists {
		t.Error("expected no block time to be recorded for invalid data")
	}
}

func TestStream_DeliveredHeightsPastReorgDepthAreForgotten(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	for n := uint64(1); n <= 250; n++ {
		if !stream.markDelivered("ethereum", n, fmt.Sprintf("0x%x", n)) {
			t.Fatalf("block %d was not news", n)
		}
	}
	if stream.markDelivered("ethereum", 250, "0xfa") {
		t.Error("a header already delivered was news")
	}

	stream.deliveredMu.Lock()
	defer stream.deliveredMu.Unlock()
	for n := range stream.delivered["ethereum"] {
		if n < 150 {
			t.Errorf("height %d is more than the reorg depth below 250 and was kept", n)
		}
	}
	if _, ok := stream.delivered["ethereum"][150]; !ok {
		t.Error("height 150, within the reorg depth of 250, was forgotten")
	}
}

func TestStream_HasActiveBlockSubscriptions(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]

	hasSubs := stream.hasActiveBlockSubscriptions(backend)
	if hasSubs {
		t.Error("expected no active block subscriptions")
	}

	subscription := &Subscription{
		ID:       "test-sub",
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["newHeads"]`),
		ClientID: "test-client",
		Backend:  backend,
		Created:  time.Now(),
	}
	stream.subscriptions["test-sub"] = subscription

	hasSubs = stream.hasActiveBlockSubscriptions(backend)
	if !hasSubs {
		t.Error("expected active block subscription")
	}

	stream.subscriptions = make(map[string]*Subscription)
	logsSubscription := &Subscription{
		ID:       "logs-sub",
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["logs"]`),
		ClientID: "test-client",
		Backend:  backend,
		Created:  time.Now(),
	}
	stream.subscriptions["logs-sub"] = logsSubscription

	hasSubs = stream.hasActiveBlockSubscriptions(backend)
	if !hasSubs {
		t.Error("expected active logs subscription")
	}

	stream.subscriptions = make(map[string]*Subscription)
	nonBlockSubscription := &Subscription{
		ID:       "non-block-sub",
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["syncing"]`),
		ClientID: "test-client",
		Backend:  backend,
		Created:  time.Now(),
	}
	stream.subscriptions["non-block-sub"] = nonBlockSubscription

	hasSubs = stream.hasActiveBlockSubscriptions(backend)
	if hasSubs {
		t.Error("expected no active block subscriptions for syncing")
	}
}

func TestStream_CheckBlockSanity(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]

	backendKey := fmt.Sprintf("%s:%s", backend.Chain, backend.WSURL.String())
	staleTime := time.Now().Add(-3 * stream.cfg.Cache.BlockTime(backend.Chain))
	stream.lastBlockMu.Lock()
	stream.lastBlockTime[backendKey] = staleTime
	stream.lastBlockMu.Unlock()

	subscription := &Subscription{
		ID:       "test-sub",
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["newHeads"]`),
		ClientID: "test-client",
		Backend:  backend,
		Created:  time.Now(),
	}
	stream.subscriptions["test-sub"] = subscription

	stream.checkBlockSanity()
}

func TestStream_ResetBackendConnections(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	backend := cfg.Backends[0]

	subscription := &Subscription{
		ID:       "test-sub",
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["newHeads"]`),
		ClientID: "test-client",
		Backend:  backend,
		Created:  time.Now(),
	}
	stream.subscriptions["test-sub"] = subscription

	stream.resetBackendConnections(backend)
}

func TestStream_NotifyBlockSubscribers(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	client := &WebsocketClient{
		ID:            "test-client",
		Chain:         "ethereum",
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}

	subscription := &Subscription{
		ID:       "test-sub",
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["newHeads"]`),
		ClientID: client.ID,
		Backend:  cfg.Backends[0],
		Created:  time.Now(),
	}
	client.Subscriptions["test-sub"] = subscription
	stream.clients[client.ID] = client

	blockData := map[string]any{
		"number": "0x1234",
		"hash":   "0xabcd",
	}
	blockDataBytes, _ := json.Marshal(blockData)

	stream.notifyBlockSubscribers("ethereum", blockDataBytes)
}

func TestStream_NotifyBlockSubscribers_WrongChain(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	client := &WebsocketClient{
		ID:            "test-client",
		Chain:         "polygon",
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}

	subscription := &Subscription{
		ID:       "test-sub",
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["newHeads"]`),
		ClientID: client.ID,
		Backend:  cfg.Backends[0],
		Created:  time.Now(),
	}
	client.Subscriptions["test-sub"] = subscription
	stream.clients[client.ID] = client

	blockData := map[string]any{
		"number": "0x1234",
		"hash":   "0xabcd",
	}
	blockDataBytes, _ := json.Marshal(blockData)

	stream.notifyBlockSubscribers("ethereum", blockDataBytes)
}

func TestStream_NotifyBlockSubscribers_NoBlockSubs(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	client := &WebsocketClient{
		ID:            "test-client",
		Chain:         "ethereum",
		Conn:          nil,
		Subscriptions: make(map[string]*Subscription),
	}

	subscription := &Subscription{
		ID:       "test-sub",
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["syncing"]`),
		ClientID: client.ID,
		Backend:  cfg.Backends[0],
		Created:  time.Now(),
	}
	client.Subscriptions["test-sub"] = subscription
	stream.clients[client.ID] = client

	blockData := map[string]any{
		"number": "0x1234",
		"hash":   "0xabcd",
	}
	blockDataBytes, _ := json.Marshal(blockData)

	stream.notifyBlockSubscribers("ethereum", blockDataBytes)
}

func TestWebsocketConnection_SendMessage(t *testing.T) {
	wsConn := &WebsocketConnection{
		Backend: &Backend{},
		Conn:    nil,
	}

	msg := &Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_blockNumber",
		Params:  json.RawMessage(`[]`),
	}

	err := wsConn.sendMessage(msg)

	if err == nil {
		t.Error("expected error with nil connection")
	}
}

func TestGenerateClientID(t *testing.T) {
	id1 := generateClientID()
	time.Sleep(1 * time.Millisecond)
	id2 := generateClientID()

	if id1 == id2 {
		t.Error("expected different client IDs")
	}

	if !strings.HasPrefix(id1, "client_") {
		t.Error("expected client ID to start with 'client_'")
	}

	if !strings.HasPrefix(id2, "client_") {
		t.Error("expected client ID to start with 'client_'")
	}
}

func TestGenerateSubscriptionID(t *testing.T) {
	id1 := generateSubscriptionID()
	time.Sleep(1 * time.Millisecond)
	id2 := generateSubscriptionID()

	if id1 == id2 {
		t.Error("expected different subscription IDs")
	}

	if !strings.HasPrefix(id1, "0x") {
		t.Error("expected subscription ID to start with '0x'")
	}

	if !strings.HasPrefix(id2, "0x") {
		t.Error("expected subscription ID to start with '0x'")
	}
}

func TestExtractBlockInfo(t *testing.T) {
	blockData := map[string]any{
		"result": map[string]any{
			"number": "0x1234",
			"hash":   "0xabcd",
		},
	}
	blockDataBytes, _ := json.Marshal(blockData)
	blockNum, blockHash := ExtractBlockInfo(blockDataBytes)

	if blockNum != 0x1234 {
		t.Errorf("expected block number 0x1234, got %d", blockNum)
	}

	if blockHash != "0xabcd" {
		t.Errorf("expected block hash 0xabcd, got %s", blockHash)
	}
}

func TestExtractBlockInfo_InvalidData(t *testing.T) {
	blockData := map[string]any{
		"result": map[string]any{
			"hash": "0xabcd",
		},
	}
	blockDataBytes, _ := json.Marshal(blockData)
	blockNum, blockHash := ExtractBlockInfo(blockDataBytes)

	if blockNum != 0 {
		t.Errorf("expected block number 0, got %d", blockNum)
	}

	if blockHash != "0xabcd" {
		t.Errorf("expected block hash 0xabcd, got %s", blockHash)
	}
}

func TestExtractBlockInfo_InvalidJSON(t *testing.T) {
	blockDataBytes := []byte("invalid json")
	blockNum, blockHash := ExtractBlockInfo(blockDataBytes)

	if blockNum != 0 {
		t.Errorf("expected block number 0, got %d", blockNum)
	}

	if blockHash != "" {
		t.Errorf("expected empty block hash, got %s", blockHash)
	}
}

func TestStream_ConcurrentAccess(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	var wg sync.WaitGroup
	numGoroutines := 10

	for i := range numGoroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			subscription := &Subscription{
				ID:       fmt.Sprintf("sub-%d", id),
				Method:   "eth_subscribe",
				Params:   json.RawMessage(`["newHeads"]`),
				ClientID: fmt.Sprintf("client-%d", id),
				Backend:  cfg.Backends[0],
				Created:  time.Now(),
			}

			stream.mu.Lock()
			stream.subscriptions[subscription.ID] = subscription
			stream.mu.Unlock()

			stream.markDelivered("ethereum", uint64(id+1), fmt.Sprintf("0x%x", id+1))

			stream.lastBlockMu.Lock()
			stream.lastBlockTime[fmt.Sprintf("chain-%d", id)] = time.Now()
			stream.lastBlockMu.Unlock()
		}(i)
	}

	wg.Wait()

	stream.mu.RLock()
	subscriptionCount := len(stream.subscriptions)
	stream.mu.RUnlock()

	if subscriptionCount != numGoroutines {
		t.Errorf("expected %d subscriptions, got %d", numGoroutines, subscriptionCount)
	}

	stream.deliveredMu.Lock()
	deliveredCount := len(stream.delivered["ethereum"])
	stream.deliveredMu.Unlock()

	if deliveredCount != numGoroutines {
		t.Errorf("expected %d delivered heights, got %d", numGoroutines, deliveredCount)
	}

	stream.lastBlockMu.RLock()
	lastBlockCount := len(stream.lastBlockTime)
	stream.lastBlockMu.RUnlock()

	if lastBlockCount != numGoroutines {
		t.Errorf("expected %d last block time entries, got %d", numGoroutines, lastBlockCount)
	}
}

// wsEndpoint is a websocket server that accepts every connection, reads until
// the peer goes away, and hands each server-side connection to conns.
func wsEndpoint(t *testing.T, conns chan<- *websocket.Conn) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if conns != nil {
			conns <- c
			return
		}
		go func() {
			defer func() { _ = c.Close() }()
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func parseStreamConfig(t *testing.T, body string) Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := fmt.Sprintf("[cache]\nenabled = false\npath = %q\n\n%s", filepath.Join(dir, "cache"), body)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return ParseConfig(path)
}

func newHeadsClient(t *testing.T, chain string) (*WebsocketClient, *websocket.Conn) {
	t.Helper()
	conns := make(chan *websocket.Conn, 1)
	srv := wsEndpoint(t, conns)
	peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	serverSide := <-conns
	t.Cleanup(func() { _ = serverSide.Close() })

	client := &WebsocketClient{
		ID:            "client",
		Chain:         chain,
		Conn:          serverSide,
		Subscriptions: make(map[string]*Subscription),
	}
	client.Subscriptions["sub"] = &Subscription{
		ID:       "sub",
		Method:   "eth_subscribe",
		Params:   json.RawMessage(`["newHeads"]`),
		ClientID: client.ID,
		Created:  time.Now(),
	}
	return client, peer
}

func blockAt(number uint64, hash string) []byte {
	return []byte(fmt.Sprintf(`{"result":{"number":"0x%x","hash":%q}}`, number, hash))
}

func TestStream_ReorgAtTheSameHeightIsDelivered(t *testing.T) {
	cfg := parseStreamConfig(t, `
[cache.chains.8453]
block_time_ms = 2000

[node.8453]
url = "http://127.0.0.1:1"
ws_url = "ws://127.0.0.1:1"
`)
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	client, peer := newHeadsClient(t, "8453")
	stream.mu.Lock()
	stream.clients[client.ID] = client
	stream.mu.Unlock()

	b := cfg.Backends[0]
	stream.handleBlockNotification("8453", b, blockAt(0x10, "0xaaaa"))
	stream.handleBlockNotification("8453", b, blockAt(0x10, "0xbbbb"))
	stream.handleBlockNotification("8453", b, blockAt(0x10, "0xbbbb"))

	var got []string
	for {
		_ = peer.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		var msg struct {
			Params []json.RawMessage `json:"params"`
		}
		if err := peer.ReadJSON(&msg); err != nil {
			break
		}
		if len(msg.Params) < 2 {
			t.Fatalf("notification without a header: %v", msg.Params)
		}
		_, hash := ExtractBlockInfo(msg.Params[1])
		got = append(got, hash)
	}

	want := []string{"0xaaaa", "0xbbbb"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("delivered headers %v, want %v: the replacement for block 16 must reach the client once, the repeat not at all", got, want)
	}
}

func TestStream_StalenessFollowsTheChainBlockTime(t *testing.T) {
	srv := wsEndpoint(t, nil)
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")
	cfg := parseStreamConfig(t, fmt.Sprintf(`
[cache.chains.8453]
block_time_ms = 2000

[stale.8453]
url = "http://127.0.0.1:1"
ws_url = "%s/stale"

[live.8453]
url = "http://127.0.0.1:1"
ws_url = "%s/live"
`, wsBase, wsBase))
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	var stale, live *Backend
	for _, b := range cfg.Backends {
		switch b.Name {
		case "stale":
			stale = b
		case "live":
			live = b
		}
	}
	stale.HealthUp.Store(false)

	sub := &Subscription{
		ID:      "sub",
		Method:  "eth_subscribe",
		Params:  json.RawMessage(`["newHeads"]`),
		Backend: stale,
		Created: time.Now(),
	}
	stream.mu.Lock()
	stream.subscriptions[sub.ID] = sub
	stream.mu.Unlock()

	stream.lastBlockMu.Lock()
	stream.lastBlockTime[fmt.Sprintf("%s:%s", stale.Chain, stale.WSURL.String())] = time.Now().Add(-5 * time.Second)
	stream.lastBlockMu.Unlock()

	stream.checkBlockSanity()

	stream.mu.RLock()
	moved := sub.Backend == live
	stream.mu.RUnlock()
	if !moved {
		t.Errorf("a backend silent for 5s on a 2s chain kept its subscription")
	}
}

func TestParseConfig_WebSocketChainWithoutBlockTime(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a chain with a websocket backend and no block_time_ms parsed")
		}
		if !strings.Contains(fmt.Sprint(r), "8453") {
			t.Errorf("panic %q does not name the chain", r)
		}
	}()
	parseStreamConfig(t, `
[node.8453]
url = "http://127.0.0.1:1"
ws_url = "ws://127.0.0.1:1"
`)
}
