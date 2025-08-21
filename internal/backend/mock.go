package backend

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"sync"
)

// MockHTTPClient is a mock HTTP client for testing
type MockHTTPClient struct {
	mu        sync.RWMutex
	requests  []MockRequest
	responses map[string]MockResponse
}

// MockRequest represents a captured HTTP request
type MockRequest struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
}

// MockResponse represents a mock HTTP response
type MockResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Error      error
}

// NewMockHTTPClient creates a new mock HTTP client
func NewMockHTTPClient() *MockHTTPClient {
	return &MockHTTPClient{
		responses: make(map[string]MockResponse),
	}
}

// SetResponse sets a mock response for a given URL
func (m *MockHTTPClient) SetResponse(url string, response MockResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses[url] = response
}

// GetRequests returns all captured requests
func (m *MockHTTPClient) GetRequests() []MockRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	requests := make([]MockRequest, len(m.requests))
	copy(requests, m.requests)
	return requests
}

// ClearRequests clears all captured requests
func (m *MockHTTPClient) ClearRequests() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = nil
}

// RoundTrip implements the http.RoundTripper interface
func (m *MockHTTPClient) RoundTrip(req *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Capture the request
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewBuffer(body)) // Restore body for potential reuse
	}

	mockReq := MockRequest{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: req.Header.Clone(),
		Body:    body,
	}
	m.requests = append(m.requests, mockReq)

	// Get the mock response
	response, exists := m.responses[req.URL.String()]
	if !exists {
		// Default response if none set
		response = MockResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"jsonrpc":"2.0","id":"1","result":"0x1234"}`),
		}
	}

	if response.Error != nil {
		return nil, response.Error
	}

	// Create the response
	resp := &http.Response{
		StatusCode: response.StatusCode,
		Header:     response.Headers,
		Body:       io.NopCloser(bytes.NewBuffer(response.Body)),
		Request:    req,
	}

	return resp, nil
}

// CreateMockBackend creates a backend with a mock HTTP client
func CreateMockBackend(name, chain, urlStr string) *Backend {
	parsedURL, _ := url.Parse(urlStr)
	mockClient := NewMockHTTPClient()
	return &Backend{
		Name:   name,
		Chain:  chain,
		URL:    parsedURL,
		Client: &http.Client{Transport: mockClient},
	}
}

// CreateMockBackendWithWS creates a backend with WebSocket support and a mock HTTP client
func CreateMockBackendWithWS(name, chain, urlStr, wsURLStr string) *Backend {
	parsedURL, _ := url.Parse(urlStr)
	parsedWSURL, _ := url.Parse(wsURLStr)
	mockClient := NewMockHTTPClient()
	return &Backend{
		Name:   name,
		Chain:  chain,
		URL:    parsedURL,
		WSURL:  parsedWSURL,
		Client: &http.Client{Transport: mockClient},
	}
}
