package backend

import (
	"maps"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type BatchRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type BatchResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

type BatchProcessor struct {
	mu sync.Mutex

	maxBatchSize  int
	maxWaitTime   time.Duration
	maxConcurrent int

	batches map[string]*Batch
	workers chan struct{}
}

type Batch struct {
	BackendURL    string
	Requests      []BatchRequest
	ResponseChans map[string]chan BatchResponse
	Created       time.Time
	Full          bool
	mu            sync.RWMutex
}

func NewBatchProcessor(maxBatchSize int, maxWaitTime time.Duration, maxConcurrent int) *BatchProcessor {
	return &BatchProcessor{
		maxBatchSize:  maxBatchSize,
		maxWaitTime:   maxWaitTime,
		maxConcurrent: maxConcurrent,
		batches:       make(map[string]*Batch),
		workers:       make(chan struct{}, maxConcurrent),
	}
}

func (bp *BatchProcessor) AddRequest(backendURL string, req BatchRequest) (<-chan BatchResponse, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	batch, exists := bp.batches[backendURL]
	if !exists {
		batch = &Batch{
			BackendURL:    backendURL,
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

	responses, err := bp.sendBatch(batch.BackendURL, requests)
	if err != nil {
		for _, req := range requests {
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

func (bp *BatchProcessor) sendBatch(backendURL string, requests []BatchRequest) ([]BatchResponse, error) {
	batchBody, err := json.Marshal(requests)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal batch request: %w", err)
	}

	req, err := http.NewRequest("POST", backendURL, bytes.NewReader(batchBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create batch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send batch request: %w", err)
	}
	defer resp.Body.Close()

	var responses []BatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&responses); err != nil {
		return nil, fmt.Errorf("failed to decode batch response: %w", err)
	}

	return responses, nil
}

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
