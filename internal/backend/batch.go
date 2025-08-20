package backend

import (
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
	Result  interface{}     `json:"result,omitempty"`
	Error   interface{}     `json:"error,omitempty"`
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
	BackendURL string
	Requests   []BatchRequest
	Responses  chan []BatchResponse
	Created    time.Time
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

func (bp *BatchProcessor) AddRequest(backendURL string, req BatchRequest) (<-chan []BatchResponse, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	batch, exists := bp.batches[backendURL]
	if !exists {
		batch = &Batch{
			BackendURL: backendURL,
			Requests:   make([]BatchRequest, 0, bp.maxBatchSize),
			Responses:  make(chan []BatchResponse, 1),
			Created:    time.Now(),
		}
		bp.batches[backendURL] = batch

		go bp.processBatch(batch)
	}

	batch.Requests = append(batch.Requests, req)
	if len(batch.Requests) >= bp.maxBatchSize {
		delete(bp.batches, backendURL)
	}

	return batch.Responses, nil
}

func (bp *BatchProcessor) processBatch(batch *Batch) {
	bp.workers <- struct{}{}
	defer func() { <-bp.workers }()

	timer := time.NewTimer(bp.maxWaitTime)
	defer timer.Stop()

	select {
	case <-timer.C:
	default:
	}

	responses, err := bp.sendBatch(batch.BackendURL, batch.Requests)
	if err != nil {
		errorResponses := make([]BatchResponse, len(batch.Requests))
		for i, req := range batch.Requests {
			errorResponses[i] = BatchResponse{
				JSONRPC: req.JSONRPC,
				ID:      req.ID,
				Error: map[string]any{
					"code":    -32603,
					"message": "Internal error: " + err.Error(),
				},
			}
		}
		batch.Responses <- errorResponses
	} else {
		batch.Responses <- responses
	}

	close(batch.Responses)
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
	defer bp.mu.Unlock()

	for _, batch := range bp.batches {
		go bp.processBatch(batch)
	}
	bp.batches = make(map[string]*Batch)
}
