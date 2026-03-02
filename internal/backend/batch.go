package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
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
type BatchResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
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
	workers chan struct{}
}

// Batch represents a collection of requests to be sent to a single backend.
type Batch struct {
	BackendURL    string
	Client        *http.Client
	Requests      []BatchRequest
	ResponseChans map[string]chan BatchResponse
	Created       time.Time
	Full          bool
	mu            sync.RWMutex
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
		workers:       make(chan struct{}, maxConcurrent),
	}
}

// AddRequest adds a request to a batch and returns a channel to receive the response.
func (bp *BatchProcessor) AddRequest(backendURL string, req BatchRequest, client *http.Client) (<-chan BatchResponse, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	batch, exists := bp.batches[backendURL]
	if !exists {
		batch = &Batch{
			BackendURL:    backendURL,
			Client:        client,
			Requests:      make([]BatchRequest, 0, bp.maxBatchSize),
			ResponseChans: make(map[string]chan BatchResponse),
			Created:       time.Now(),
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
	reqID := string(req.ID)

	batch.mu.Lock()
	batch.Requests = append(batch.Requests, reqCopy)
	batch.ResponseChans[reqID] = responseChan
	requestCount := len(batch.Requests)
	batch.mu.Unlock()

	if requestCount >= bp.maxBatchSize {
		batch.mu.Lock()
		batch.Full = true
		batch.mu.Unlock()
	}

	return responseChan, nil
}

func (bp *BatchProcessor) processBatch(batch *Batch) {
	bp.workers <- struct{}{}
	defer func() { <-bp.workers }()

	timer := time.NewTimer(bp.maxWaitTime)
	defer timer.Stop()

	batch.mu.RLock()
	full := batch.Full
	batch.mu.RUnlock()

	if !full {
		<-timer.C
	}

	batch.mu.RLock()
	requests := make([]BatchRequest, len(batch.Requests))
	copy(requests, batch.Requests)
	responseChans := make(map[string]chan BatchResponse, len(batch.ResponseChans))
	maps.Copy(responseChans, batch.ResponseChans)
	batch.mu.RUnlock()

	bp.mu.Lock()
	delete(bp.batches, batch.BackendURL)
	bp.mu.Unlock()

	// Aggregate eligible eth_call requests into Multicall3 calls.
	var mapping *MulticallMapping
	if bp.multicall.Enabled {
		requests, mapping = aggregateEthCalls(requests, bp.multicall)
	}

	responses, err := bp.sendBatch(batch, requests)

	// Expand multicall responses back into individual responses.
	if err == nil && mapping != nil {
		responses, err = expandMulticallResponses(responses, mapping)
	}

	if err != nil {
		// On error, fan out to all original request IDs. When multicall is
		// active, the request list contains synthetic IDs — use the mapping
		// to recover original IDs.
		errorTargets := requests
		if mapping != nil {
			errorTargets = make([]BatchRequest, 0, len(mapping.Passthrough)+countEntries(mapping))
			errorTargets = append(errorTargets, mapping.Passthrough...)
			for _, group := range mapping.Groups {
				for _, entry := range group.Entries {
					errorTargets = append(errorTargets, BatchRequest{
						JSONRPC: "2.0",
						ID:      entry.OriginalID,
					})
				}
			}
		}
		for _, req := range errorTargets {
			if ch, exists := responseChans[string(req.ID)]; exists {
				errorResp := BatchResponse{
					JSONRPC: req.JSONRPC,
					ID:      req.ID,
					Error: map[string]any{
						"code":    -32603,
						"message": "Internal error: " + err.Error(),
					},
				}
				select {
				case ch <- errorResp:
				default:
				}
			}
		}
	} else {
		for _, resp := range responses {
			if ch, exists := responseChans[string(resp.ID)]; exists {
				select {
				case ch <- resp:
				default:
				}
			}
		}
	}
}

func (bp *BatchProcessor) sendBatch(batch *Batch, requests []BatchRequest) ([]BatchResponse, error) {
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
			time.Sleep(base + jitter)
		}

		req, err := http.NewRequest("POST", batch.BackendURL, bytes.NewReader(batchBody))
		if err != nil {
			return nil, fmt.Errorf("failed to create batch request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("failed to send batch request: %w", err)
			if isTransientError(err) {
				continue
			}
			return nil, lastErr
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
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

// FlushAll processes all pending batches immediately.
func (bp *BatchProcessor) FlushAll() {
	bp.mu.Lock()
	batches := make([]*Batch, 0, len(bp.batches))
	for _, batch := range bp.batches {
		batches = append(batches, batch)
	}
	bp.batches = make(map[string]*Batch)
	bp.mu.Unlock()

	for _, batch := range batches {
		go bp.processBatch(batch)
	}
}

// GetBatchCount returns the number of active batches (for testing).
func (bp *BatchProcessor) GetBatchCount() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return len(bp.batches)
}
