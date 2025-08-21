package backend

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func createTestBatchProcessor() *BatchProcessor {
	return NewBatchProcessor(5, 50*time.Millisecond, 2)
}

func TestNewBatchProcessor(t *testing.T) {
	bp := createTestBatchProcessor()

	if bp == nil {
		t.Fatal("expected batch processor to be created")
	}

	if bp.maxBatchSize != 5 {
		t.Errorf("expected max batch size 5, got %d", bp.maxBatchSize)
	}

	if bp.maxWaitTime != 50*time.Millisecond {
		t.Errorf("expected max wait time 50ms, got %v", bp.maxWaitTime)
	}

	if bp.maxConcurrent != 2 {
		t.Errorf("expected max concurrent 2, got %d", bp.maxConcurrent)
	}

	if len(bp.batches) != 0 {
		t.Errorf("expected empty batches map, got %d entries", len(bp.batches))
	}

	if cap(bp.workers) != 2 {
		t.Errorf("expected workers channel capacity 2, got %d", cap(bp.workers))
	}
}

func TestBatchProcessor_AddRequest(t *testing.T) {
	bp := createTestBatchProcessor()

	backendURL := "https://api.example.com"
	req := BatchRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_blockNumber",
		Params:  json.RawMessage(`[]`),
	}

	responseChan, err := bp.AddRequest(backendURL, req)
	if err != nil {
		t.Errorf("expected no error adding request, got %v", err)
	}
	if responseChan == nil {
		t.Error("expected response channel to be returned")
	}

	if bp.GetBatchCount() != 1 {
		t.Errorf("expected 1 batch, got %d", bp.GetBatchCount())
	}

	time.Sleep(10 * time.Millisecond)

	bp.mu.Lock()
	batch, exists := bp.batches[backendURL]
	bp.mu.Unlock()
	if !exists {
		t.Error("expected batch to exist for backend URL")
	}

	if len(batch.Requests) != 1 {
		t.Errorf("expected 1 request in batch, got %d", len(batch.Requests))
	}

	if batch.Requests[0].Method != "eth_blockNumber" {
		t.Errorf("expected method eth_blockNumber, got %s", batch.Requests[0].Method)
	}
}

func TestBatchProcessor_MaxBatchSize(t *testing.T) {
	bp := createTestBatchProcessor()

	backendURL := "https://api.example.com"
	req := BatchRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_blockNumber",
		Params:  json.RawMessage(`[]`),
	}

	for i := range 5 {
		req.ID = json.RawMessage(fmt.Sprintf(`"%d"`, i))
		responseChan, err := bp.AddRequest(backendURL, req)
		if err != nil {
			t.Errorf("expected no error adding request %d, got %v", i, err)
		}
		if responseChan == nil {
			t.Errorf("expected response channel for request %d", i)
		}
	}

	time.Sleep(100 * time.Millisecond)

	if bp.GetBatchCount() != 0 {
		t.Errorf("expected 0 batches after reaching max size, got %d", bp.GetBatchCount())
	}
}

func TestBatchProcessor_MultipleBackends(t *testing.T) {
	bp := createTestBatchProcessor()

	backendURLs := []string{
		"https://api1.example.com",
		"https://api2.example.com",
		"https://api3.example.com",
	}

	req := BatchRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_blockNumber",
		Params:  json.RawMessage(`[]`),
	}

	for i, url := range backendURLs {
		req.ID = json.RawMessage(fmt.Sprintf(`"%d"`, i))
		responseChan, err := bp.AddRequest(url, req)
		if err != nil {
			t.Errorf("expected no error adding request to %s, got %v", url, err)
		}
		if responseChan == nil {
			t.Errorf("expected response channel for %s", url)
		}
	}

	if bp.GetBatchCount() != 3 {
		t.Errorf("expected 3 batches, got %d", bp.GetBatchCount())
	}

	for _, url := range backendURLs {
		bp.mu.Lock()
		_, exists := bp.batches[url]
		bp.mu.Unlock()
		if !exists {
			t.Errorf("expected batch to exist for %s", url)
		}
	}
}

func TestBatchProcessor_ConcurrentWorkers(t *testing.T) {
	bp := NewBatchProcessor(5, 50*time.Millisecond, 2)

	if cap(bp.workers) != 2 {
		t.Errorf("expected worker pool capacity 2, got %d", cap(bp.workers))
	}

	bp2 := NewBatchProcessor(5, 50*time.Millisecond, 1)

	if cap(bp2.workers) != 1 {
		t.Errorf("expected worker pool capacity 1, got %d", cap(bp2.workers))
	}

	bp3 := NewBatchProcessor(5, 10*time.Millisecond, 1)

	if len(bp3.workers) != 0 {
		t.Errorf("expected empty worker pool initially, got %d", len(bp3.workers))
	}

	select {
	case bp3.workers <- struct{}{}:
		if len(bp3.workers) != 1 {
			t.Errorf("expected 1 worker after acquisition, got %d", len(bp3.workers))
		}

		<-bp3.workers
		if len(bp3.workers) != 0 {
			t.Errorf("expected 0 workers after release, got %d", len(bp3.workers))
		}
	default:
		t.Error("expected to be able to acquire worker from pool")
	}
}

func TestBatchProcessor_FlushAll(t *testing.T) {
	bp := createTestBatchProcessor()

	backendURL := "https://api.example.com"
	req := BatchRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_blockNumber",
		Params:  json.RawMessage(`[]`),
	}

	responseChan, err := bp.AddRequest(backendURL, req)
	if err != nil {
		t.Errorf("expected no error adding request, got %v", err)
	}

	if bp.GetBatchCount() != 1 {
		t.Errorf("expected 1 batch before flush, got %d", bp.GetBatchCount())
	}

	bp.FlushAll()

	if bp.GetBatchCount() != 0 {
		t.Errorf("expected 0 batches after flush, got %d", bp.GetBatchCount())
	}

	select {
	case resp := <-responseChan:
		if resp.Error == nil {
			t.Error("expected error in response due to invalid URL")
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("timeout waiting for response after flush")
	}
}

func TestBatchProcessor_ErrorHandling(t *testing.T) {
	bp := createTestBatchProcessor()

	req := BatchRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_blockNumber",
		Params:  json.RawMessage(`[]`),
	}

	responseChan, err := bp.AddRequest("invalid-url", req)
	if err != nil {
		t.Errorf("expected no error adding request, got %v", err)
	}

	select {
	case resp := <-responseChan:
		if resp.Error == nil {
			t.Error("expected error in response")
		}
		if resp.JSONRPC != "2.0" {
			t.Errorf("expected JSONRPC 2.0, got %s", resp.JSONRPC)
		}
		if string(resp.ID) != `"1"` {
			t.Errorf("expected ID \"1\", got %s", string(resp.ID))
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("timeout waiting for error response")
	}
}

func TestBatchProcessor_TimeoutHandling(t *testing.T) {
	bp := NewBatchProcessor(1, 10*time.Millisecond, 1)

	backendURL := "https://api.example.com"
	req := BatchRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_blockNumber",
		Params:  json.RawMessage(`[]`),
	}

	responseChan, err := bp.AddRequest(backendURL, req)
	if err != nil {
		t.Errorf("expected no error adding request, got %v", err)
	}

	select {
	case resp := <-responseChan:
		if resp.Error == nil {
			t.Error("expected error in response due to timeout")
		}
		if resp.JSONRPC != "2.0" {
			t.Errorf("expected JSONRPC 2.0, got %s", resp.JSONRPC)
		}
		if string(resp.ID) != `"1"` {
			t.Errorf("expected ID \"1\", got %s", string(resp.ID))
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for response")
	}
}

func TestBatchProcessor_RequestMatching(t *testing.T) {
	bp := createTestBatchProcessor()

	backendURL := "https://api.example.com"
	req1 := BatchRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_blockNumber",
		Params:  json.RawMessage(`[]`),
	}

	req2 := BatchRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"2"`),
		Method:  "eth_getBalance",
		Params:  json.RawMessage(`["0x123", "latest"]`),
	}

	responseChan1, err := bp.AddRequest(backendURL, req1)
	if err != nil {
		t.Errorf("expected no error adding request 1, got %v", err)
	}

	responseChan2, err := bp.AddRequest(backendURL, req2)
	if err != nil {
		t.Errorf("expected no error adding request 2, got %v", err)
	}

	if bp.GetBatchCount() != 1 {
		t.Errorf("expected 1 batch for same backend, got %d", bp.GetBatchCount())
	}

	bp.mu.Lock()
	batch, exists := bp.batches[backendURL]
	bp.mu.Unlock()
	if !exists {
		t.Fatal("expected batch to exist for backend")
	}

	if len(batch.Requests) != 2 {
		t.Errorf("expected 2 requests in batch, got %d", len(batch.Requests))
	}

	if string(batch.Requests[0].ID) != `"1"` {
		t.Errorf("expected first request ID \"1\", got %s", string(batch.Requests[0].ID))
	}
	if string(batch.Requests[1].ID) != `"2"` {
		t.Errorf("expected second request ID \"2\", got %s", string(batch.Requests[1].ID))
	}

	if batch.Requests[0].Method != "eth_blockNumber" {
		t.Errorf("expected first request method eth_blockNumber, got %s", batch.Requests[0].Method)
	}
	if batch.Requests[1].Method != "eth_getBalance" {
		t.Errorf("expected second request method eth_getBalance, got %s", batch.Requests[1].Method)
	}

	if responseChan1 == responseChan2 {
		t.Error("expected different response channels for different requests")
	}
}

func TestBatchProcessor_EmptyBatch(t *testing.T) {
	bp := createTestBatchProcessor()

	bp.FlushAll()

	if bp.GetBatchCount() != 0 {
		t.Errorf("expected 0 batches, got %d", bp.GetBatchCount())
	}
}

func TestBatchProcessor_WorkerReuse(t *testing.T) {
	bp := NewBatchProcessor(1, 50*time.Millisecond, 1)

	if len(bp.workers) != 0 {
		t.Errorf("expected empty worker pool initially, got %d", len(bp.workers))
	}

	for i := range 3 {
		select {
		case bp.workers <- struct{}{}:
			if len(bp.workers) != 1 {
				t.Errorf("expected 1 worker after acquisition %d, got %d", i, len(bp.workers))
			}
		default:
			t.Errorf("expected to be able to acquire worker on iteration %d", i)
		}

		select {
		case <-bp.workers:
			if len(bp.workers) != 0 {
				t.Errorf("expected 0 workers after release %d, got %d", i, len(bp.workers))
			}
		default:
			t.Errorf("expected to be able to release worker on iteration %d", i)
		}
	}

	if cap(bp.workers) != 1 {
		t.Errorf("expected worker pool capacity 1, got %d", cap(bp.workers))
	}

	if len(bp.workers) != 0 {
		t.Errorf("expected empty worker pool at end, got %d", len(bp.workers))
	}
}
