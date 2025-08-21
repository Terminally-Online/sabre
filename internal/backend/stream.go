package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type Stream struct {
	cfg           Config
	loadBalancer  *LoadBalancer
	connections   map[string][]*WebsocketConnection
	subscriptions map[string]*Subscription
	clients       map[string]*WebsocketClient
	blockCache    map[string]time.Time
	blockCacheMu  sync.RWMutex
	lastBlockTime map[string]time.Time // key: "chain:backendURL"
	lastBlockMu   sync.RWMutex
	mu            sync.RWMutex
	upgrader      websocket.Upgrader
}

type WebsocketConnection struct {
	Backend       *Backend
	Conn          *websocket.Conn
	Subscriptions map[string]*Subscription
	mu            sync.RWMutex
	writeMu       sync.Mutex
	closed        atomic.Bool
	lastPing      atomic.Value
	lastPong      atomic.Value
	messageCount  atomic.Int64
}

type WebsocketClient struct {
	ID            string
	Chain         string
	Conn          *websocket.Conn
	Subscriptions map[string]*Subscription
	mu            sync.RWMutex
	writeMu       sync.Mutex
	closed        atomic.Bool
	lastActivity  atomic.Value
}

type Subscription struct {
	ID       string          `json:"subscription"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
	ClientID string          `json:"client_id"`
	Backend  *Backend        `json:"-"`
	Created  time.Time       `json:"created"`
}

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

func NewStream(cfg Config, loadBalancer *LoadBalancer) *Stream {
	stream := &Stream{
		cfg:           cfg,
		loadBalancer:  loadBalancer,
		connections:   make(map[string][]*WebsocketConnection),
		subscriptions: make(map[string]*Subscription),
		clients:       make(map[string]*WebsocketClient),
		blockCache:    make(map[string]time.Time),
		lastBlockTime: make(map[string]time.Time),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
			EnableCompression: cfg.Subscriptions.EnableCompression,
		},
	}

	go stream.sanityCheckLoop()
	return stream
}

func (wm *Stream) Shutdown(ctx context.Context) {
	wm.mu.Lock()
	for _, client := range wm.clients {
		client.closed.Store(true)
		if client.Conn != nil {
			client.Conn.Close()
		}
	}
	wm.mu.Unlock()

	wm.mu.Lock()
	for _, connections := range wm.connections {
		for _, conn := range connections {
			conn.closed.Store(true)
			if conn.Conn != nil {
				conn.Conn.Close()
			}
		}
	}
	wm.mu.Unlock()
}

func (wm *Stream) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	chain := strings.Trim(strings.TrimSpace(r.URL.Path), "/")
	if chain == "" {
		http.Error(w, "chain path required (e.g., /ethereum, /base)", http.StatusBadRequest)
		return
	}

	conn, err := wm.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	clientID := generateClientID()
	client := &WebsocketClient{
		ID:            clientID,
		Chain:         chain,
		Conn:          conn,
		Subscriptions: make(map[string]*Subscription),
	}
	client.lastActivity.Store(time.Now())

	wm.mu.Lock()
	wm.clients[clientID] = client
	wm.mu.Unlock()

	defer func() {
		wm.removeClient(clientID)
		conn.Close()
	}()

	conn.SetReadLimit(wm.cfg.Subscriptions.MaxMessageSize)
	conn.SetReadDeadline(time.Now().Add(wm.cfg.Subscriptions.ReadWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wm.cfg.Subscriptions.ReadWait))
		return nil
	})

	for {
		var msg Message
		err := conn.ReadJSON(&msg)
		if err != nil {
			_ = websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure)
			break
		}

		client.lastActivity.Store(time.Now())
		wm.handleClientMessage(client, chain, &msg)
	}
}

func (wm *Stream) handleClientMessage(client *WebsocketClient, chain string, msg *Message) {
	switch msg.Method {
	case "eth_subscribe":
		wm.handleSubscribe(client, chain, msg)
	case "eth_unsubscribe":
		wm.handleUnsubscribe(client, msg)
	default:
		wm.handleRPCCall(client, chain, msg)
	}
}

func (wm *Stream) handleSubscribe(client *WebsocketClient, chain string, msg *Message) {
	backend, err := wm.pickBackend(chain)
	if err != nil {
		wm.sendError(client, msg.ID, -32603, "No available backends")
		return
	}

	subID := generateSubscriptionID()
	subscription := &Subscription{
		ID:       subID,
		Method:   msg.Method,
		Params:   msg.Params,
		ClientID: client.ID,
		Backend:  backend,
		Created:  time.Now(),
	}

	client.mu.Lock()
	client.Subscriptions[subID] = subscription
	client.mu.Unlock()

	wm.mu.Lock()
	wm.subscriptions[subID] = subscription
	wm.mu.Unlock()

	wsConn, err := wm.getOrCreateWSConnection(backend)
	if err != nil {
		wm.sendError(client, msg.ID, -32603, "Failed to connect to backend")
		return
	}

	wsConn.mu.Lock()
	wsConn.Subscriptions[subID] = subscription
	wsConn.mu.Unlock()

	err = wsConn.sendMessage(msg)
	if err != nil {
		wm.sendError(client, msg.ID, -32603, "Failed to subscribe")
		return
	}

	response := Message{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Result:  subID,
	}
	wm.sendToClient(client, &response)
}

func (wm *Stream) handleUnsubscribe(client *WebsocketClient, msg *Message) {
	var subID string
	if err := json.Unmarshal(msg.Params, &subID); err != nil {
		wm.sendError(client, msg.ID, -32602, "Invalid parameters")
		return
	}

	client.mu.Lock()
	subscription, exists := client.Subscriptions[subID]
	delete(client.Subscriptions, subID)
	client.mu.Unlock()

	if !exists {
		wm.sendError(client, msg.ID, -32602, "Subscription not found")
		return
	}

	wm.mu.Lock()
	delete(wm.subscriptions, subID)
	wm.mu.Unlock()

	if subscription.Backend != nil {
		wsConn := wm.getWSConnection(subscription.Backend)
		if wsConn != nil {
			wsConn.mu.Lock()
			delete(wsConn.Subscriptions, subID)
			wsConn.mu.Unlock()

			unsubMsg := Message{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Method:  "eth_unsubscribe",
				Params:  msg.Params,
			}
			wsConn.sendMessage(&unsubMsg)
		}
	}

	response := Message{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Result:  true,
	}
	wm.sendToClient(client, &response)
}

func (wm *Stream) handleRPCCall(client *WebsocketClient, chain string, msg *Message) {
	backend, err := wm.pickBackend(chain)
	if err != nil {
		wm.sendError(client, msg.ID, -32603, "No available backends")
		return
	}

	wsConn, err := wm.getOrCreateWSConnection(backend)
	if err != nil {
		wm.sendError(client, msg.ID, -32603, "Failed to connect to backend")
		return
	}

	err = wsConn.sendMessage(msg)
	if err != nil {
		wm.sendError(client, msg.ID, -32603, "Failed to send request")
		return
	}
}

func (wm *Stream) getOrCreateWSConnection(backend *Backend) (*WebsocketConnection, error) {
	backendURL := backend.WSURL.String()

	wm.mu.RLock()
	connections := wm.connections[backendURL]
	wm.mu.RUnlock()

	for _, conn := range connections {
		if !conn.closed.Load() && len(conn.Subscriptions) < wm.cfg.Subscriptions.MaxSubscriptionsPerConnection {
			return conn, nil
		}
	}

	if len(connections) < wm.cfg.Subscriptions.MaxConnectionsPerBackend {
		return wm.createWSConnection(backend)
	}

	var leastLoaded *WebsocketConnection
	minSubs := int(^uint(0) >> 1)
	for _, conn := range connections {
		if !conn.closed.Load() {
			conn.mu.RLock()
			subCount := len(conn.Subscriptions)
			conn.mu.RUnlock()
			if subCount < minSubs {
				minSubs = subCount
				leastLoaded = conn
			}
		}
	}

	if leastLoaded != nil {
		return leastLoaded, nil
	}

	return nil, fmt.Errorf("no available connections to backend")
}

func (wm *Stream) createWSConnection(backend *Backend) (*WebsocketConnection, error) {
	conn, _, err := websocket.DefaultDialer.Dial(backend.WSURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to backend: %w", err)
	}

	wsConn := &WebsocketConnection{
		Backend:       backend,
		Conn:          conn,
		Subscriptions: make(map[string]*Subscription),
	}
	wsConn.lastPing.Store(time.Now())
	wsConn.lastPong.Store(time.Now())

	conn.SetReadLimit(wm.cfg.Subscriptions.MaxMessageSize)
	conn.SetReadDeadline(time.Now().Add(wm.cfg.Subscriptions.ReadWait))
	conn.SetPongHandler(func(string) error {
		wsConn.lastPong.Store(time.Now())
		conn.SetReadDeadline(time.Now().Add(wm.cfg.Subscriptions.ReadWait))
		return nil
	})

	wm.mu.Lock()
	wm.connections[backend.WSURL.String()] = append(wm.connections[backend.WSURL.String()], wsConn)
	wm.mu.Unlock()

	go wm.pingPongLoop(wsConn)
	go wm.handleBackendMessages(wsConn)

	return wsConn, nil
}

func (wm *Stream) pingPongLoop(wsConn *WebsocketConnection) {
	ticker := time.NewTicker(wm.cfg.Subscriptions.PingInterval)
	defer ticker.Stop()

	for range ticker.C {
		if wsConn.closed.Load() {
			return
		}

		lastPing := wsConn.lastPing.Load().(time.Time)
		if time.Since(lastPing) >= wm.cfg.Subscriptions.PingInterval {
			wsConn.writeMu.Lock()
			err := wsConn.Conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(wm.cfg.Subscriptions.WriteWait))
			wsConn.writeMu.Unlock()
			if err != nil {
				wm.closeWSConnection(wsConn)
				return
			}
			wsConn.lastPing.Store(time.Now())
		}

		lastPong := wsConn.lastPong.Load().(time.Time)
		if time.Since(lastPong) > wm.cfg.Subscriptions.PongWait {
			wm.closeWSConnection(wsConn)
			return
		}
	}
}

func (wm *Stream) handleBackendMessages(wsConn *WebsocketConnection) {
	defer wm.closeWSConnection(wsConn)

	for {
		var msg Message
		err := wsConn.Conn.ReadJSON(&msg)
		if err != nil {
			_ = websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure)
			break
		}

		wsConn.messageCount.Add(1)
		wm.forwardBackendMessage(wsConn, &msg)
	}
}

func (wm *Stream) forwardBackendMessage(wsConn *WebsocketConnection, msg *Message) {
	if msg.Method == "eth_subscription" {
		var params []any
		if err := json.Unmarshal(msg.Params, &params); err != nil || len(params) < 1 {
			return
		}

		subID, ok := params[0].(string)
		if !ok {
			return
		}

		wsConn.mu.RLock()
		subscription, exists := wsConn.Subscriptions[subID]
		wsConn.mu.RUnlock()

		if !exists {
			return
		}

		if len(params) > 1 {
			if subscription.Method == "eth_subscribe" && subscription.Params != nil {
				var subParams []any
				if err := json.Unmarshal(subscription.Params, &subParams); err == nil {
					if len(subParams) > 0 && subParams[0] == "newHeads" {
						blockData, _ := json.Marshal(params[1])
						wm.handleBlockNotification(wsConn.Backend.Chain, wsConn.Backend, blockData)
						return
					}
				}
			}
		}

		wm.mu.RLock()
		client, exists := wm.clients[subscription.ClientID]
		wm.mu.RUnlock()

		if exists && !client.closed.Load() {
			wm.sendToClient(client, msg)
		}
	} else {
		wm.mu.RLock()
		for _, client := range wm.clients {
			if !client.closed.Load() {
				wm.sendToClient(client, msg)
			}
		}
		wm.mu.RUnlock()
	}
}

func (wm *Stream) closeWSConnection(wsConn *WebsocketConnection) {
	if wsConn.closed.CompareAndSwap(false, true) {
		if wsConn.Conn != nil {
			wsConn.Conn.Close()
		}

		wm.mu.Lock()
		backendURL := wsConn.Backend.WSURL.String()
		connections := wm.connections[backendURL]
		for i, conn := range connections {
			if conn == wsConn {
				connections = append(connections[:i], connections[i+1:]...)
				break
			}
		}
		wm.connections[backendURL] = connections
		wm.mu.Unlock()

		wsConn.mu.Lock()
		for subID := range wsConn.Subscriptions {
			delete(wm.subscriptions, subID)
		}
		wsConn.mu.Unlock()
	}
}

func (wm *Stream) removeClient(clientID string) {
	wm.mu.Lock()
	client, exists := wm.clients[clientID]
	delete(wm.clients, clientID)
	wm.mu.Unlock()

	if !exists {
		return
	}

	client.closed.Store(true)
	if client.Conn != nil {
		client.Conn.Close()
	}

	client.mu.Lock()
	for subID := range client.Subscriptions {
		delete(wm.subscriptions, subID)
	}
	client.mu.Unlock()
}

func (wm *Stream) pickBackend(chain string) (*Backend, error) {
	return wm.loadBalancer.Pick(context.Background(), chain, "ws")
}

func (wm *Stream) getWSConnection(backend *Backend) *WebsocketConnection {
	wm.mu.RLock()
	defer wm.mu.RUnlock()

	connections := wm.connections[backend.WSURL.String()]
	for _, conn := range connections {
		if conn.Backend == backend && !conn.closed.Load() {
			return conn
		}
	}
	return nil
}

func (wsConn *WebsocketConnection) sendMessage(msg *Message) error {
	wsConn.writeMu.Lock()
	defer wsConn.writeMu.Unlock()

	if wsConn.Conn == nil {
		return fmt.Errorf("websocket connection is nil")
	}

	wsConn.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return wsConn.Conn.WriteJSON(msg)
}

func (wm *Stream) sendToClient(client *WebsocketClient, msg *Message) {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()

	if client.Conn == nil {
		return
	}

	client.Conn.SetWriteDeadline(time.Now().Add(wm.cfg.Subscriptions.WriteWait))
	_ = client.Conn.WriteJSON(msg)
}

func (wm *Stream) sendError(client *WebsocketClient, id json.RawMessage, code int, message string) {
	response := Message{
		JSONRPC: "2.0",
		ID:      id,
		Error: map[string]interface{}{
			"code":    code,
			"message": message,
		},
	}
	wm.sendToClient(client, &response)
}

func generateClientID() string {
	return fmt.Sprintf("client_%d", time.Now().UnixNano())
}

func generateSubscriptionID() string {
	return fmt.Sprintf("0x%x", time.Now().UnixNano())
}

func (wm *Stream) handleBlockNotification(chain string, backend *Backend, blockData []byte) {
	blockNum, blockHash := ExtractBlockInfo(blockData)
	if blockNum == 0 || blockHash == "" {
		return
	}

	backendKey := fmt.Sprintf("%s:%s", chain, backend.WSURL.String())
	wm.lastBlockMu.Lock()
	wm.lastBlockTime[backendKey] = time.Now()
	wm.lastBlockMu.Unlock()

	cacheKey := fmt.Sprintf("block:%s:%d", chain, blockNum)

	wm.blockCacheMu.Lock()
	if lastNotified, exists := wm.blockCache[cacheKey]; exists {
		if time.Since(lastNotified) < wm.cfg.Subscriptions.TTLBlock {
			wm.blockCacheMu.Unlock()
			return
		}
	}

	wm.blockCache[cacheKey] = time.Now()
	wm.blockCacheMu.Unlock()

	go wm.cleanupBlockCache()

	wm.notifyBlockSubscribers(chain, blockData)
}

func (wm *Stream) cleanupBlockCache() {
	wm.blockCacheMu.Lock()
	defer wm.blockCacheMu.Unlock()

	cutoff := time.Now().Add(-wm.cfg.Subscriptions.TTLBlock)
	for key, timestamp := range wm.blockCache {
		if timestamp.Before(cutoff) {
			delete(wm.blockCache, key)
		}
	}
}

func (wm *Stream) hasActiveBlockSubscriptions(backend *Backend) bool {
	wm.mu.RLock()
	defer wm.mu.RUnlock()

	for _, sub := range wm.subscriptions {
		if sub.Backend != nil && sub.Backend.WSURL.String() == backend.WSURL.String() {
			if sub.Method == "eth_subscribe" {
				var params []any
				if err := json.Unmarshal(sub.Params, &params); err == nil && len(params) > 0 {
					if subType, ok := params[0].(string); ok && (subType == "newHeads" || subType == "logs") {
						return true
					}
				}
			}
		}
	}
	return false
}

func (wm *Stream) checkBlockSanity() {
	wm.lastBlockMu.RLock()
	staleBackends := make([]*Backend, 0)

	for backendKey, lastBlock := range wm.lastBlockTime {
		if time.Since(lastBlock) > wm.cfg.Subscriptions.TTLBlock {
			// Extract chain and WSURL from backendKey "chain:wsurl"
			parts := strings.SplitN(backendKey, ":", 2)
			if len(parts) != 2 {
				continue
			}
			chain, wsURL := parts[0], parts[1]

			// Find the backend struct
			for _, backend := range wm.cfg.Backends {
				if backend.Chain == chain && backend.WSURL.String() == wsURL {
					if wm.hasActiveBlockSubscriptions(backend) {
						staleBackends = append(staleBackends, backend)
					}
					break
				}
			}
		}
	}
	wm.lastBlockMu.RUnlock()

	for _, backend := range staleBackends {
		wm.resetBackendConnections(backend)
	}
}

func (wm *Stream) resetBackendConnections(staleBackend *Backend) {
	wm.mu.RLock()
	affectedSubscriptions := make([]*Subscription, 0)

	// Only find subscriptions for this specific backend
	for _, sub := range wm.subscriptions {
		if sub.Backend != nil && sub.Backend.WSURL.String() == staleBackend.WSURL.String() {
			affectedSubscriptions = append(affectedSubscriptions, sub)
		}
	}
	wm.mu.RUnlock()

	if len(affectedSubscriptions) == 0 {
		return
	}

	// Pick a NEW backend for this chain (avoiding the stale one)
	newBackend, err := wm.loadBalancer.Pick(context.Background(), staleBackend.Chain, "ws")
	if err != nil {
		return
	}

	// Don't migrate to the same backend
	if newBackend.WSURL.String() == staleBackend.WSURL.String() {
		return
	}

	newConn, err := wm.getOrCreateWSConnection(newBackend)
	if err != nil {
		return
	}

	wm.mu.Lock()
	for _, sub := range affectedSubscriptions {
		// Move subscription to new backend
		sub.Backend = newBackend

		// Remove from old backend connection
		if oldConn := wm.getWSConnection(staleBackend); oldConn != nil {
			oldConn.mu.Lock()
			delete(oldConn.Subscriptions, sub.ID)
			oldConn.mu.Unlock()
		}

		// Add to new backend connection
		newConn.mu.Lock()
		newConn.Subscriptions[sub.ID] = sub
		newConn.mu.Unlock()

		// Re-subscribe on new backend
		subMsg := Message{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`"` + sub.ID + `"`),
			Method:  sub.Method,
			Params:  sub.Params,
		}
		newConn.sendMessage(&subMsg)
	}
	wm.mu.Unlock()

	// Close the specific stale backend connection
	wm.closeBackendConnection(staleBackend)

	// Remove the stale backend from block time tracking
	backendKey := fmt.Sprintf("%s:%s", staleBackend.Chain, staleBackend.WSURL.String())
	wm.lastBlockMu.Lock()
	delete(wm.lastBlockTime, backendKey)
	wm.lastBlockMu.Unlock()
}

func (wm *Stream) closeBackendConnection(backend *Backend) {
	connectionsToClose := make([]*WebsocketConnection, 0)

	for _, connections := range wm.connections {
		for _, conn := range connections {
			if conn.Backend.WSURL.String() == backend.WSURL.String() {
				conn.mu.RLock()
				hasSubscriptions := len(conn.Subscriptions) > 0
				conn.mu.RUnlock()

				if !hasSubscriptions {
					connectionsToClose = append(connectionsToClose, conn)
				}
			}
		}
	}

	for _, conn := range connectionsToClose {
		wm.closeWSConnection(conn)
	}
}

func (wm *Stream) sanityCheckLoop() {
	ticker := time.NewTicker(wm.cfg.Subscriptions.TTLBlock)
	defer ticker.Stop()

	for range ticker.C {
		wm.checkBlockSanity()
	}
}

func (wm *Stream) notifyBlockSubscribers(chain string, blockData []byte) {
	wm.mu.RLock()
	defer wm.mu.RUnlock()

	for _, client := range wm.clients {
		if client.Chain != chain {
			continue
		}

		hasBlockSubs := false
		for _, sub := range client.Subscriptions {
			if sub.Method == "eth_subscribe" && sub.Params != nil {
				var params []any
				if err := json.Unmarshal(sub.Params, &params); err == nil {
					if len(params) > 0 && params[0] == "newHeads" {
						hasBlockSubs = true
						break
					}
				}
			}
		}

		if hasBlockSubs {
			params := []any{
				"newHeads",
				json.RawMessage(blockData),
			}
			paramsBytes, _ := json.Marshal(params)

			notification := Message{
				JSONRPC: "2.0",
				Method:  "eth_subscription",
				Params:  paramsBytes,
			}

			wm.sendToClient(client, &notification)
		}
	}
}
