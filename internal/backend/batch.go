package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// BatchRequest represents a single JSON-RPC request within a batch.
type BatchRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// BatchResponse represents a single JSON-RPC response within a batch.
// Result and Error are kept as raw JSON so payloads round-trip through the
// batch pipeline byte-for-byte: no float64 coercion of numbers and no loss
// of an explicit "result":null.
type BatchResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`

	// InternalErr marks a response synthesized by the batch processor for a
	// transport or protocol failure (send failed, upstream dropped the
	// response). It is never set on an error the upstream itself returned —
	// a legitimate JSON-RPC error such as an eth_call revert must reach the
	// caller as an error envelope, not be retried as a failed request.
	InternalErr error `json:"-"`
}

// HasError reports whether the response carries a JSON-RPC error object.
func (r BatchResponse) HasError() bool {
	trimmed := bytes.TrimSpace(r.Error)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// BatchProcessor groups multiple RPC requests into batches for efficient processing.
type BatchProcessor struct {
	mu sync.Mutex

	maxBatchSize  int
	maxWaitTime   time.Duration
	maxConcurrent int
	maxRetries    int
	multicall     MulticallConfig

	batches map[string]*Batch
	workers map[string]chan struct{}
}

// batchEntry pairs a caller's original request with its private response
// channel. Responses are routed back by the entry's position in the batch —
// NEVER by the client-supplied JSON-RPC id, which is not unique across the
// independent clients coalesced into one upstream batch (two clients both
// using id 1 would otherwise receive each other's payloads).
type batchEntry struct {
	ctx context.Context
	req BatchRequest
	ch  chan BatchResponse
}

// Batch represents a collection of requests to be sent to a single backend.
type Batch struct {
	BackendURL string
	Client     *http.Client
	Created    time.Time
	dispatch   chan struct{}
	workers    chan struct{}
	mu         sync.Mutex
	entries    []batchEntry
}

// NewBatchProcessor creates a new batch processor with the specified configuration.
func NewBatchProcessor(maxBatchSize int, maxWaitTime time.Duration, maxConcurrent int, maxRetries int, multicall MulticallConfig) *BatchProcessor {
	return &BatchProcessor{
		maxBatchSize:  maxBatchSize,
		maxWaitTime:   maxWaitTime,
		maxConcurrent: maxConcurrent,
		maxRetries:    maxRetries,
		multicall:     multicall,
		batches:       make(map[string]*Batch),
		workers:       make(map[string]chan struct{}),
	}
}

// pool returns the worker semaphore bounding in-flight batches to backendURL.
func (bp *BatchProcessor) pool(backendURL string) chan struct{} {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.poolLocked(backendURL)
}

func (bp *BatchProcessor) poolLocked(backendURL string) chan struct{} {
	workers, ok := bp.workers[backendURL]
	if !ok {
		workers = make(chan struct{}, bp.maxConcurrent)
		bp.workers[backendURL] = workers
	}
	return workers
}

// AddRequest adds a request to a batch and returns a channel that receives
// exactly one response for this request. ctx is the caller's wait: once it is
// done the request is dropped rather than sent, and an upstream call is
// cancelled once every request it carries has been abandoned.
func (bp *BatchProcessor) AddRequest(ctx context.Context, backendURL string, req BatchRequest, client *http.Client) (<-chan BatchResponse, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	batch, exists := bp.batches[backendURL]
	if !exists {
		batch = &Batch{
			BackendURL: backendURL,
			Client:     client,
			Created:    time.Now(),
			dispatch:   make(chan struct{}),
			workers:    bp.poolLocked(backendURL),
			entries:    make([]batchEntry, 0, bp.maxBatchSize),
		}
		bp.batches[backendURL] = batch

		go bp.processBatch(batch)
	}

	reqCopy := BatchRequest{
		JSONRPC: req.JSONRPC,
		Method:  req.Method,
	}

	if len(req.ID) > 0 {
		reqCopy.ID = make(json.RawMessage, len(req.ID))
		copy(reqCopy.ID, req.ID)
	} else {
		reqCopy.ID = json.RawMessage("null")
	}

	if len(req.Params) > 0 {
		reqCopy.Params = make(json.RawMessage, len(req.Params))
		copy(reqCopy.Params, req.Params)
	} else {
		reqCopy.Params = json.RawMessage("[]")
	}

	responseChan := make(chan BatchResponse, 1)

	batch.mu.Lock()
	batch.entries = append(batch.entries, batchEntry{ctx: ctx, req: reqCopy, ch: responseChan})
	full := len(batch.entries) >= bp.maxBatchSize
	batch.mu.Unlock()

	if full {
		// Remove the batch from the map before signaling dispatch so no
		// further entries can join after the processor snapshots them, and
		// so the next request for this backend starts a fresh batch.
		delete(bp.batches, backendURL)
		close(batch.dispatch)
	}

	return responseChan, nil
}

func (bp *BatchProcessor) processBatch(batch *Batch) {
	timer := time.NewTimer(bp.maxWaitTime)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-batch.dispatch:
	}

	// Claim the batch under bp.mu BEFORE snapshotting entries: AddRequest
	// appends while holding bp.mu, so once the batch is unreachable from
	// the map no entry can be added after the snapshot (an added-but-unsent
	// request would hang its caller until timeout).
	bp.mu.Lock()
	if bp.batches[batch.BackendURL] == batch {
		delete(bp.batches, batch.BackendURL)
	}
	bp.mu.Unlock()

	batch.mu.Lock()
	entries := batch.entries
	batch.mu.Unlock()

	if len(entries) == 0 {
		return
	}

	ctx, release := callers(entries)
	defer release()

	select {
	case batch.workers <- struct{}{}:
	case <-ctx.Done():
		abandon(entries)
		return
	}

	live := make([]batchEntry, 0, len(entries))
	for _, e := range entries {
		if e.ctx.Err() == nil {
			live = append(live, e)
		} else {
			abandon([]batchEntry{e})
		}
	}
	entries = live
	if len(entries) == 0 {
		<-batch.workers
		return
	}

	// Requests go upstream under positional wire ids (the entry index).
	// Client-supplied ids are NOT unique across the coalesced clients, so
	// they can never be used to route responses. The original id is
	// restored on delivery.
	requests := make([]BatchRequest, len(entries))
	for i, e := range entries {
		requests[i] = e.req
		requests[i].ID = json.RawMessage(strconv.Itoa(i))
	}

	// Aggregate eligible eth_call requests into Multicall3 calls.
	var mapping *MulticallMapping
	if bp.multicall.Enabled {
		requests, mapping = aggregateEthCalls(requests, bp.multicall)
	}

	responses, err := func() ([]BatchResponse, error) {
		defer func() { <-batch.workers }()
		return bp.sendBatch(ctx, batch, requests)
	}()

	// Expand multicall responses back into individual responses.
	if err == nil && mapping != nil {
		responses, err = expandMulticallResponses(responses, mapping)
	}

	if err != nil {
		for _, e := range entries {
			e.ch <- BatchResponse{
				JSONRPC:     "2.0",
				ID:          e.req.ID,
				Error:       rawJSON(map[string]any{"code": -32603, "message": "Internal error: " + err.Error()}),
				InternalErr: err,
			}
		}
		return
	}

	delivered := make([]bool, len(entries))
	for _, resp := range responses {
		idx, ok := parseWireID(resp.ID)
		if !ok || idx < 0 || idx >= len(entries) || delivered[idx] {
			continue
		}
		delivered[idx] = true
		resp.ID = entries[idx].req.ID
		entries[idx].ch <- resp
	}

	// An upstream that drops or corrupts a response must not strand its
	// caller until the router timeout: deliver an explicit error.
	for i, e := range entries {
		if delivered[i] {
			continue
		}
		e.ch <- BatchResponse{
			JSONRPC:     "2.0",
			ID:          e.req.ID,
			Error:       rawJSON(map[string]any{"code": -32603, "message": "Internal error: upstream batch response missing"}),
			InternalErr: errMissingUpstreamResponse,
		}
	}
}

var errMissingUpstreamResponse = errors.New("upstream batch response missing")

// callers returns a context that ends once every entry's caller has stopped
// waiting, and a release that detaches it from the entries.
func callers(entries []batchEntry) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	var waiting atomic.Int64
	waiting.Store(int64(len(entries)))
	stops := make([]func() bool, len(entries))
	for i, e := range entries {
		stops[i] = context.AfterFunc(e.ctx, func() {
			if waiting.Add(-1) == 0 {
				cancel()
			}
		})
	}
	return ctx, func() {
		for _, stop := range stops {
			stop()
		}
		cancel()
	}
}

// abandon answers entries whose callers stopped waiting before they were sent.
func abandon(entries []batchEntry) {
	for _, e := range entries {
		err := context.Cause(e.ctx)
		e.ch <- BatchResponse{
			JSONRPC:     "2.0",
			ID:          e.req.ID,
			Error:       rawJSON(map[string]any{"code": -32603, "message": "Internal error: " + err.Error()}),
			InternalErr: err,
		}
	}
}

// parseWireID decodes a positional wire id assigned by processBatch. Upstreams
// must echo ids verbatim, but a quoted echo is tolerated.
func parseWireID(id json.RawMessage) (int, bool) {
	s := strings.Trim(string(bytes.TrimSpace(id)), `"`)
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// rawJSON marshals v, which must be JSON-encodable, into a raw message.
func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (bp *BatchProcessor) sendBatch(ctx context.Context, batch *Batch, requests []BatchRequest) ([]BatchResponse, error) {
	batchBody, err := json.Marshal(requests)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal batch request: %w", err)
	}

	client := batch.Client
	if client == nil {
		client = http.DefaultClient
	}

	var lastErr error
	for attempt := 0; attempt <= bp.maxRetries; attempt++ {
		if attempt > 0 {
			base := time.Duration(50<<uint(attempt-1)) * time.Millisecond
			jitter := time.Duration(rand.Int64N(int64(base / 2)))
			backoff := time.NewTimer(base + jitter)
			select {
			case <-backoff.C:
			case <-ctx.Done():
				backoff.Stop()
				return nil, fmt.Errorf("batch callers left: %w", context.Cause(ctx))
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", batch.BackendURL, bytes.NewReader(batchBody))
		if err != nil {
			return nil, fmt.Errorf("failed to create batch request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("failed to send batch request: %w", err)
			if ctx.Err() == nil && isTransientError(err) {
				continue
			}
			return nil, lastErr
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read batch response: %w", err)
			continue
		}

		if isRetryableHTTPStatus(resp.StatusCode) {
			lastErr = fmt.Errorf("batch request returned status %d", resp.StatusCode)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("batch request returned status %d: %s", resp.StatusCode, string(body))
		}

		var responses []BatchResponse
		if err := json.Unmarshal(body, &responses); err != nil {
			var singleResponse BatchResponse
			if err := json.Unmarshal(body, &singleResponse); err != nil {
				return nil, fmt.Errorf("failed to decode batch response: %w", err)
			}
			responses = []BatchResponse{singleResponse}
		}

		return responses, nil
	}

	return nil, lastErr
}

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "broken pipe")
}

func isRetryableHTTPStatus(code int) bool {
	return code == 429 || code == 408 || (code >= 500 && code != 501 && code != 505)
}

// FlushAll dispatches all pending batches immediately.
func (bp *BatchProcessor) FlushAll() {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	for url, batch := range bp.batches {
		delete(bp.batches, url)
		close(batch.dispatch)
	}
}

// GetBatchCount returns the number of active batches (for testing).
func (bp *BatchProcessor) GetBatchCount() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return len(bp.batches)
}
