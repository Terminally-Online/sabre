package backend

import (
	"net/http"
	"testing"
)

func TestMockHTTPClient(t *testing.T) {
	mockClient := NewMockHTTPClient()

	expectedResponse := `{"jsonrpc":"2.0","id":"1","result":"0x1234"}`
	mockClient.SetResponse("https://api.example.com", MockResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(expectedResponse),
	})

	client := &http.Client{Transport: mockClient}

	req, err := http.NewRequest("POST", "https://api.example.com", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("failed to make request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("expected content-type application/json, got %s", resp.Header.Get("Content-Type"))
	}

	requests := mockClient.GetRequests()
	if len(requests) != 1 {
		t.Errorf("expected 1 request, got %d", len(requests))
	}

	if requests[0].Method != "POST" {
		t.Errorf("expected method POST, got %s", requests[0].Method)
	}

	if requests[0].URL != "https://api.example.com" {
		t.Errorf("expected URL https://api.example.com, got %s", requests[0].URL)
	}
}

func TestCreateMockBackend(t *testing.T) {
	backend := CreateMockBackend("test-backend", "ethereum", "https://api.example.com")

	if backend.Name != "test-backend" {
		t.Errorf("expected name test-backend, got %s", backend.Name)
	}

	if backend.Chain != "ethereum" {
		t.Errorf("expected chain ethereum, got %s", backend.Chain)
	}

	if backend.URL.String() != "https://api.example.com" {
		t.Errorf("expected URL https://api.example.com, got %s", backend.URL.String())
	}

	if backend.Client == nil {
		t.Error("expected client to be set")
	}
}

func TestCreateMockBackendWithWS(t *testing.T) {
	backend := CreateMockBackendWithWS("test-backend", "ethereum", "https://api.example.com", "wss://api.example.com")

	if backend.Name != "test-backend" {
		t.Errorf("expected name test-backend, got %s", backend.Name)
	}

	if backend.Chain != "ethereum" {
		t.Errorf("expected chain ethereum, got %s", backend.Chain)
	}

	if backend.URL.String() != "https://api.example.com" {
		t.Errorf("expected URL https://api.example.com, got %s", backend.URL.String())
	}

	if backend.WSURL.String() != "wss://api.example.com" {
		t.Errorf("expected WS URL wss://api.example.com, got %s", backend.WSURL.String())
	}

	if backend.Client == nil {
		t.Error("expected client to be set")
	}
}
