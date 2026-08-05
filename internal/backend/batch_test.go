package backend

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func createTestBatchProcessor() *BatchProcessor {
	return NewBatchProcessor(5, 50*time.Millisecond, 2, 2, MulticallConfig{})
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

	responseChan, err := bp.AddRequest(backendURL, req, nil)
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

	if len(batch.entries) != 1 {
		t.Errorf("expected 1 request in batch, got %d", len(batch.entries))
	}

	if batch.entries[0].req.Method != "eth_blockNumber" {
		t.Errorf("expected method eth_blockNumber, got %s", batch.entries[0].req.Method)
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
		responseChan, err := bp.AddRequest(backendURL, req, nil)
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
		responseChan, err := bp.AddRequest(url, req, nil)
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
	bp := NewBatchProcessor(5, 50*time.Millisecond, 2, 2, MulticallConfig{})

	if cap(bp.workers) != 2 {
		t.Errorf("expected worker pool capacity 2, got %d", cap(bp.workers))
	}

	bp2 := NewBatchProcessor(5, 50*time.Millisecond, 1, 2, MulticallConfig{})

	if cap(bp2.workers) != 1 {
		t.Errorf("expected worker pool capacity 1, got %d", cap(bp2.workers))
	}

	bp3 := NewBatchProcessor(5, 10*time.Millisecond, 1, 2, MulticallConfig{})

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

	responseChan, err := bp.AddRequest(backendURL, req, nil)
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

	responseChan, err := bp.AddRequest("invalid-url", req, nil)
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
	bp := NewBatchProcessor(1, 10*time.Millisecond, 1, 2, MulticallConfig{})

	backendURL := "https://api.example.com"
	req := BatchRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"1"`),
		Method:  "eth_blockNumber",
		Params:  json.RawMessage(`[]`),
	}

	responseChan, err := bp.AddRequest(backendURL, req, nil)
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

	responseChan1, err := bp.AddRequest(backendURL, req1, nil)
	if err != nil {
		t.Errorf("expected no error adding request 1, got %v", err)
	}

	responseChan2, err := bp.AddRequest(backendURL, req2, nil)
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

	if len(batch.entries) != 2 {
		t.Errorf("expected 2 requests in batch, got %d", len(batch.entries))
	}

	if string(batch.entries[0].req.ID) != `"1"` {
		t.Errorf("expected first request ID \"1\", got %s", string(batch.entries[0].req.ID))
	}
	if string(batch.entries[1].req.ID) != `"2"` {
		t.Errorf("expected second request ID \"2\", got %s", string(batch.entries[1].req.ID))
	}

	if batch.entries[0].req.Method != "eth_blockNumber" {
		t.Errorf("expected first request method eth_blockNumber, got %s", batch.entries[0].req.Method)
	}
	if batch.entries[1].req.Method != "eth_getBalance" {
		t.Errorf("expected second request method eth_getBalance, got %s", batch.entries[1].req.Method)
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
	bp := NewBatchProcessor(1, 50*time.Millisecond, 1, 2, MulticallConfig{})

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

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonHTTPResponse(req *http.Request, body []byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}
}

func decodeUpstreamBatch(r *http.Request) ([]BatchRequest, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var reqs []BatchRequest
	if err := json.Unmarshal(body, &reqs); err != nil {
		var single BatchRequest
		if err := json.Unmarshal(body, &single); err != nil {
			return nil, err
		}
		reqs = []BatchRequest{single}
	}
	return reqs, nil
}

// methodEchoClient answers every request with a result identifying the
// request's method and params, in REVERSE order — a spec-legal upstream
// behavior that breaks any positional or id-collision-prone response routing.
func methodEchoClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		reqs, err := decodeUpstreamBatch(r)
		if err != nil {
			return nil, err
		}
		resps := make([]BatchResponse, 0, len(reqs))
		for i := len(reqs) - 1; i >= 0; i-- {
			resps = append(resps, BatchResponse{
				JSONRPC: "2.0",
				ID:      reqs[i].ID,
				Result:  rawJSON("m=" + reqs[i].Method + ";p=" + string(reqs[i].Params)),
			})
		}
		out, _ := json.Marshal(resps)
		return jsonHTTPResponse(r, out), nil
	})}
}

// TestBatchProcessor_DuplicateClientIDsNoCrossTalk is the regression test for
// cross-client response cross-talk: independent clients that share a JSON-RPC
// id (every client counting from 1) are coalesced into one upstream batch, and
// each must get back the payload for ITS request — never another client's.
func TestBatchProcessor_DuplicateClientIDsNoCrossTalk(t *testing.T) {
	bp := NewBatchProcessor(10, 20*time.Millisecond, 2, 0, MulticallConfig{})
	client := methodEchoClient(t)

	methods := []string{"eth_call", "eth_getLogs", "eth_getBlockByNumber", "eth_getStorageAt"}
	chans := make([]<-chan BatchResponse, len(methods))
	params := make([]string, len(methods))
	for i, m := range methods {
		params[i] = fmt.Sprintf(`["req-%d"]`, i)
		ch, err := bp.AddRequest("https://upstream.example", BatchRequest{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Method:  m,
			Params:  json.RawMessage(params[i]),
		}, client)
		if err != nil {
			t.Fatalf("AddRequest %d: %v", i, err)
		}
		chans[i] = ch
	}

	for i, ch := range chans {
		select {
		case resp := <-ch:
			if resp.HasError() {
				t.Fatalf("request %d (%s): unexpected error %s", i, methods[i], resp.Error)
			}
			if string(resp.ID) != `1` {
				t.Errorf("request %d: id rewritten to %s, want original 1", i, resp.ID)
			}
			var got string
			if err := json.Unmarshal(resp.Result, &got); err != nil {
				t.Fatalf("request %d: result not a string: %s", i, resp.Result)
			}
			want := fmt.Sprintf("m=%s;p=%s", methods[i], params[i])
			if got != want {
				t.Errorf("request %d (%s): CROSS-TALK — got payload %q, want %q", i, methods[i], got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("request %d (%s): no response delivered", i, methods[i])
		}
	}
}

// TestBatchProcessor_MissingUpstreamResponse verifies that an upstream
// dropping a response yields an explicit error for the affected caller
// instead of stranding it until the router timeout, and does not disturb
// the other callers' payloads.
func TestBatchProcessor_MissingUpstreamResponse(t *testing.T) {
	bp := NewBatchProcessor(10, 20*time.Millisecond, 2, 0, MulticallConfig{})
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		reqs, err := decodeUpstreamBatch(r)
		if err != nil {
			return nil, err
		}
		var resps []BatchResponse
		for i, req := range reqs {
			if i == 0 {
				continue
			}
			resps = append(resps, BatchResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  rawJSON("m=" + req.Method),
			})
		}
		out, _ := json.Marshal(resps)
		return jsonHTTPResponse(r, out), nil
	})}

	ch1, err := bp.AddRequest("https://upstream.example", BatchRequest{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "eth_call", Params: json.RawMessage(`["a"]`),
	}, client)
	if err != nil {
		t.Fatalf("AddRequest: %v", err)
	}
	ch2, err := bp.AddRequest("https://upstream.example", BatchRequest{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "eth_getLogs", Params: json.RawMessage(`["b"]`),
	}, client)
	if err != nil {
		t.Fatalf("AddRequest: %v", err)
	}

	select {
	case resp := <-ch1:
		if !resp.HasError() {
			t.Errorf("dropped request should receive an error, got result %s", resp.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dropped request: no response delivered")
	}

	select {
	case resp := <-ch2:
		if resp.HasError() {
			t.Fatalf("surviving request: unexpected error %s", resp.Error)
		}
		if string(resp.Result) != `"m=eth_getLogs"` {
			t.Errorf("surviving request: got payload %s, want \"m=eth_getLogs\"", resp.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("surviving request: no response delivered")
	}
}

// TestBatchProcessor_MulticallDuplicateClientIDs exercises the multicall
// fusion path with colliding client ids: fused eth_calls and a passthrough
// all using id 1 must each receive their own payload after expansion.
func TestBatchProcessor_MulticallDuplicateClientIDs(t *testing.T) {
	mcCfg := MulticallConfig{Enabled: true, Address: multicallAddress, MaxCalls: 150}
	bp := NewBatchProcessor(10, 20*time.Millisecond, 2, 0, mcCfg)

	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		reqs, err := decodeUpstreamBatch(r)
		if err != nil {
			return nil, err
		}
		resps := make([]BatchResponse, 0, len(reqs))
		for i := len(reqs) - 1; i >= 0; i-- {
			req := reqs[i]
			if req.Method == "eth_call" {
				parsed, ok := parseEthCallParams(req.Params)
				if ok && strings.EqualFold(parsed.Params.To, multicallAddress) {
					calls, ok := decodeAggregate3Calls(hexToBytes(parsed.Params.Data))
					if ok {
						results := make([]multicallResult, len(calls))
						for j, c := range calls {
							results[j] = multicallResult{Success: true, ReturnData: c.CallData}
						}
						resps = append(resps, BatchResponse{
							JSONRPC: "2.0",
							ID:      req.ID,
							Result:  rawJSON("0x" + hex.EncodeToString(encodeAggregate3Result(results))),
						})
						continue
					}
				}
			}
			resps = append(resps, BatchResponse{JSONRPC: "2.0", ID: req.ID, Result: rawJSON("0x1")})
		}
		out, _ := json.Marshal(resps)
		return jsonHTTPResponse(r, out), nil
	})}

	reqs := []BatchRequest{
		{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "eth_call",
			Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000001","data":"0xdeadbeef"},"latest"]`)},
		{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "eth_call",
			Params: json.RawMessage(`[{"to":"0x0000000000000000000000000000000000000002","data":"0xcafebabe"},"latest"]`)},
		{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "eth_blockNumber", Params: json.RawMessage(`[]`)},
	}
	chans := make([]<-chan BatchResponse, len(reqs))
	for i, req := range reqs {
		ch, err := bp.AddRequest("https://upstream.example", req, client)
		if err != nil {
			t.Fatalf("AddRequest %d: %v", i, err)
		}
		chans[i] = ch
	}

	want := []string{`"0xdeadbeef"`, `"0xcafebabe"`, `"0x1"`}
	for i, ch := range chans {
		select {
		case resp := <-ch:
			if resp.HasError() {
				t.Fatalf("request %d: unexpected error %s", i, resp.Error)
			}
			if string(resp.ID) != `1` {
				t.Errorf("request %d: id %s, want original 1", i, resp.ID)
			}
			if string(resp.Result) != want[i] {
				t.Errorf("request %d: CROSS-TALK — got payload %s, want %s", i, resp.Result, want[i])
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("request %d: no response delivered", i)
		}
	}
}

// TestAccumulationDoesNotHoldAWorkerSlot pins that a batch still filling its
// wait window does not occupy the concurrency semaphore.
//
// maxConcurrent exists to bound how many batches are in flight upstream. If it
// also gates the accumulation wait, every backend's wait window queues behind
// every other backend's and a caller's latency becomes the sum of the waits
// ahead of it rather than its own. Under a busy indexer that sum runs past the
// router's deadline and healthy requests come back as 502s.
func TestAccumulationDoesNotHoldAWorkerSlot(t *testing.T) {
	answer := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var reqs []BatchRequest
		_ = json.Unmarshal(body, &reqs)
		out := make([]BatchResponse, len(reqs))
		for i, q := range reqs {
			out[i] = BatchResponse{JSONRPC: "2.0", ID: q.ID, Result: json.RawMessage(`"0x1"`)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
	var servers [2]*httptest.Server
	for i := range servers {
		servers[i] = httptest.NewServer(http.HandlerFunc(answer))
		defer servers[i].Close()
	}

	const wait = 300 * time.Millisecond
	bp := NewBatchProcessor(10, wait, 1, 1, MulticallConfig{})
	req := BatchRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "eth_blockNumber", Params: json.RawMessage(`[]`)}

	chans := make([]<-chan BatchResponse, len(servers))
	start := time.Now()
	for i, s := range servers {
		ch, err := bp.AddRequest(s.URL, req, s.Client())
		if err != nil {
			t.Fatalf("backend %d: AddRequest: %v", i, err)
		}
		chans[i] = ch
	}
	for i, ch := range chans {
		select {
		case resp := <-ch:
			if resp.InternalErr != nil {
				t.Fatalf("backend %d: %v", i, resp.InternalErr)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("backend %d never answered", i)
		}
	}

	if elapsed, ceiling := time.Since(start), 2*wait; elapsed >= ceiling {
		t.Fatalf("two backends settled in %v, at least %v — the accumulation waits are serializing on the worker semaphore", elapsed, ceiling)
	}
}

// benchUpstream answers a JSON-RPC batch after a fixed delay, modelling a node
// whose per-call cost is dominated by execution rather than transport.
func benchUpstream(latency time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var reqs []BatchRequest
		_ = json.Unmarshal(body, &reqs)
		time.Sleep(latency)
		out := make([]BatchResponse, len(reqs))
		for i, q := range reqs {
			out[i] = BatchResponse{JSONRPC: "2.0", ID: q.ID, Result: json.RawMessage(`"0x1"`)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// BenchmarkBatchThroughput sweeps the batch knobs against a fixed-latency
// upstream and reports sustained requests per second plus per-request latency.
//
// The proxy exists to serve the most calls at the lowest latency, so the batch
// settings are an empirical question, not a design preference: coalescing trades
// added wait for fewer upstream round trips, and only measurement says where
// that trade turns negative for a given upstream cost and caller concurrency.
func BenchmarkBatchThroughput(b *testing.B) {
	const callers = 128

	req := BatchRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "eth_call", Params: json.RawMessage(`[]`)}

	for _, c := range []struct {
		upstream time.Duration
		workers  int
		size     int
		wait     time.Duration
	}{
		{2 * time.Millisecond, 4, 10, 50 * time.Millisecond},
		{2 * time.Millisecond, 4, 10, 5 * time.Millisecond},
		{2 * time.Millisecond, 64, 10, 5 * time.Millisecond},
		{2 * time.Millisecond, 256, 1, 0},

		{50 * time.Millisecond, 4, 10, 50 * time.Millisecond},
		{50 * time.Millisecond, 16, 10, 5 * time.Millisecond},
		{50 * time.Millisecond, 64, 10, 5 * time.Millisecond},
		{50 * time.Millisecond, 256, 10, 5 * time.Millisecond},
		{50 * time.Millisecond, 256, 1, 0},
	} {
		name := fmt.Sprintf("upstream=%s/workers=%d/size=%d/wait=%s", c.upstream, c.workers, c.size, c.wait)
		b.Run(name, func(b *testing.B) {
			srv := benchUpstream(c.upstream)
			defer srv.Close()
			bp := NewBatchProcessor(c.size, c.wait, c.workers, 1, MulticallConfig{})
			var wg sync.WaitGroup
			var latency atomic.Int64
			work := make(chan struct{}, b.N)
			for i := 0; i < b.N; i++ {
				work <- struct{}{}
			}
			close(work)

			b.ResetTimer()
			start := time.Now()
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range work {
						t0 := time.Now()
						ch, err := bp.AddRequest(srv.URL, req, srv.Client())
						if err != nil {
							b.Error(err)
							return
						}
						<-ch
						latency.Add(int64(time.Since(t0)))
					}
				}()
			}
			wg.Wait()
			elapsed := time.Since(start)
			b.StopTimer()

			b.ReportMetric(float64(b.N)/elapsed.Seconds(), "req/s")
			b.ReportMetric(float64(latency.Load())/float64(b.N)/1e6, "ms/req")
		})
	}
}
