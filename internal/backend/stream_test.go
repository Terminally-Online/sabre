package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
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

	if stream.blockCache == nil {
		t.Error("expected block cache to be initialized")
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

	blockNum := uint64(0x1234)
	cacheKey := fmt.Sprintf("block:%s:%d", "ethereum", blockNum)
	stream.blockCacheMu.RLock()
	_, exists = stream.blockCache[cacheKey]
	stream.blockCacheMu.RUnlock()

	if !exists {
		t.Error("expected block to be cached")
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

func TestStream_CleanupBlockCache(t *testing.T) {
	cfg := createTestStreamConfig()
	lb := NewLoadBalancer(cfg)
	stream := NewStream(cfg, lb)
	defer stream.Shutdown(context.Background())

	oldTime := time.Now().Add(-2 * stream.cfg.Subscriptions.TTLBlock)
	stream.blockCacheMu.Lock()
	stream.blockCache["old-block"] = oldTime
	stream.blockCache["new-block"] = time.Now()
	stream.blockCacheMu.Unlock()

	stream.cleanupBlockCache()

	stream.blockCacheMu.RLock()
	_, oldExists := stream.blockCache["old-block"]
	_, newExists := stream.blockCache["new-block"]
	stream.blockCacheMu.RUnlock()

	if oldExists {
		t.Error("expected old block to be removed")
	}

	if !newExists {
		t.Error("expected new block to remain")
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
	staleTime := time.Now().Add(-2 * stream.cfg.Subscriptions.TTLBlock)
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

			stream.blockCacheMu.Lock()
			stream.blockCache[fmt.Sprintf("block-%d", id)] = time.Now()
			stream.blockCacheMu.Unlock()

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

	stream.blockCacheMu.RLock()
	blockCacheCount := len(stream.blockCache)
	stream.blockCacheMu.RUnlock()

	if blockCacheCount != numGoroutines {
		t.Errorf("expected %d block cache entries, got %d", numGoroutines, blockCacheCount)
	}

	stream.lastBlockMu.RLock()
	lastBlockCount := len(stream.lastBlockTime)
	stream.lastBlockMu.RUnlock()

	if lastBlockCount != numGoroutines {
		t.Errorf("expected %d last block time entries, got %d", numGoroutines, lastBlockCount)
	}
}
