package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func answerBatch(w http.ResponseWriter, body []byte) {
	var reqs []BatchRequest
	_ = json.Unmarshal(body, &reqs)
	out := make([]BatchResponse, len(reqs))
	for i, q := range reqs {
		out[i] = BatchResponse{JSONRPC: "2.0", ID: q.ID, Result: json.RawMessage(`"0x1"`)}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func TestARequestWhoseCallerLeftIsNeverSent(t *testing.T) {
	release := make(chan struct{})
	var abandoned atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "eth_abandoned") {
			abandoned.Add(1)
		}
		<-release
		answerBatch(w, body)
	}))
	defer srv.Close()

	bp := NewBatchProcessor(1, time.Millisecond, 1, 0, MulticallConfig{})
	held, err := bp.AddRequest(context.Background(), srv.URL, BatchRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "eth_blockNumber"}, srv.Client())
	if err != nil {
		t.Fatalf("AddRequest: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := bp.AddRequest(ctx, srv.URL, BatchRequest{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "eth_abandoned"}, srv.Client()); err != nil {
		t.Fatalf("AddRequest: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(release)

	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the held request never answered")
	}

	live, err := bp.AddRequest(context.Background(), srv.URL, BatchRequest{JSONRPC: "2.0", ID: json.RawMessage(`3`), Method: "eth_chainId"}, srv.Client())
	if err != nil {
		t.Fatalf("AddRequest: %v", err)
	}
	select {
	case <-live:
	case <-time.After(5 * time.Second):
		t.Fatal("the request after the abandoned one never answered")
	}

	if n := abandoned.Load(); n != 0 {
		t.Fatalf("a request whose caller left was sent upstream %d time(s)", n)
	}
}

func TestAnUpstreamCallIsCancelledWhenEveryCallerLeaves(t *testing.T) {
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer srv.Close()

	bp := NewBatchProcessor(2, time.Millisecond, 1, 0, MulticallConfig{})
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	for i, ctx := range []context.Context{ctxA, ctxB} {
		if _, err := bp.AddRequest(ctx, srv.URL, BatchRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "eth_call"}, srv.Client()); err != nil {
			t.Fatalf("caller %d: AddRequest: %v", i, err)
		}
	}
	time.Sleep(50 * time.Millisecond)

	cancelA()
	select {
	case <-cancelled:
		t.Fatal("the upstream call was cancelled while a caller still waited on it")
	case <-time.After(100 * time.Millisecond):
	}

	cancelB()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream call kept running after every caller left")
	}
}

func TestOneBackendsLoadDoesNotStarveAnother(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		answerBatch(w, body)
	}))
	defer busy.Close()
	idle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		answerBatch(w, body)
	}))
	defer idle.Close()

	bp := NewBatchProcessor(1, time.Millisecond, 1, 0, MulticallConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 4; i++ {
		if _, err := bp.AddRequest(ctx, busy.URL, BatchRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "eth_getLogs"}, busy.Client()); err != nil {
			t.Fatalf("AddRequest: %v", err)
		}
	}
	time.Sleep(50 * time.Millisecond)

	ch, err := bp.AddRequest(context.Background(), idle.URL, BatchRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "eth_blockNumber"}, idle.Client())
	if err != nil {
		t.Fatalf("AddRequest: %v", err)
	}
	select {
	case resp := <-ch:
		if resp.InternalErr != nil {
			t.Fatalf("idle backend: %v", resp.InternalErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a backend with no load waited on another backend's workers")
	}
}
