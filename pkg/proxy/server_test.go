package proxy

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mars-base/hi/pkg/config"
)

// TestHandleConnectBlocksAnthropicAPI verifies that CONNECT requests to
// api.anthropic.com are blocked with 403 Forbidden.
func TestHandleConnectBlocksAnthropicAPI(t *testing.T) {
	proxy, err := createTestProxy()
	if err != nil {
		t.Fatalf("Failed to create test proxy: %v", err)
	}
	defer proxy.Close()

	// Create a CONNECT request to api.anthropic.com
	req := httptest.NewRequest("CONNECT", "api.anthropic.com:443", nil)
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	// Should be blocked with 403
	if w.Code != http.StatusForbidden {
		t.Errorf("Expected 403 Forbidden, got %d", w.Code)
	}

	// Response body should contain the block message
	if w.Body.String() == "" {
		t.Error("Expected non-empty response body")
	}
}

// TestHandleConnectAllowsOtherDomains verifies that CONNECT requests to
// non-anthropic domains are allowed through.
func TestHandleConnectAllowsOtherDomains(t *testing.T) {
	proxy, err := createTestProxy()
	if err != nil {
		t.Fatalf("Failed to create test proxy: %v", err)
	}
	defer proxy.Close()

	// Create a CONNECT request to example.com (should be allowed)
	req := httptest.NewRequest("CONNECT", "example.com:443", nil)
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	// Should NOT be 403 (it will likely be 502 Bad Gateway because we can't
	// actually establish a tunnel in the test, but that's fine - we just
	// want to verify it's not blocked)
	if w.Code == http.StatusForbidden {
		t.Errorf("Expected non-403 status for allowed domain, got 403")
	}
}

// TestHandleConnectLogsRequests verifies that CONNECT requests are logged.
func TestHandleConnectLogsRequests(t *testing.T) {
	proxy, err := createTestProxy()
	if err != nil {
		t.Fatalf("Failed to create test proxy: %v", err)
	}
	defer proxy.Close()

	initialReqCount := proxy.reqCount.Load()

	// Make a CONNECT request
	req := httptest.NewRequest("CONNECT", "api.anthropic.com:443", nil)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	// Verify request counter increased
	if proxy.reqCount.Load() <= initialReqCount {
		t.Error("Expected request counter to increase")
	}
}

// TestHandleConnectWithRealHTTPServer tests CONNECT handling with a real
// HTTP server to verify the proxy correctly hijacks connections.
func TestHandleConnectWithRealHTTPServer(t *testing.T) {
	proxy, err := createTestProxy()
	if err != nil {
		t.Fatalf("Failed to create test proxy: %v", err)
	}

	// Start a real HTTP server for the proxy
	server := httptest.NewServer(proxy)
	defer server.Close()

	// Test 1: CONNECT to api.anthropic.com should be blocked
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer conn.Close()

	// Send CONNECT request
	connectReq := "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n"
	if _, err := conn.Write([]byte(connectReq)); err != nil {
		t.Fatalf("Failed to write CONNECT request: %v", err)
	}

	// Read response
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected 403 for api.anthropic.com, got %d", resp.StatusCode)
	}
}

// createTestProxy creates a minimal ProxyState for testing
func createTestProxy() (*ProxyState, error) {
	cfg := &config.Config{
		ActiveBackend: "test",
		ProxyPort:     18799,
		Backends: map[string]config.BackendConfig{
			"test": {
				Type:    "anthropic",
				BaseURL: "http://127.0.0.1:18800",
				APIKey:  "test-key",
				Models: config.ModelMapping{
					Opus:   "test-opus",
					Sonnet: "test-sonnet",
					Haiku:  "test-haiku",
				},
			},
		},
	}
	return NewProxyState(cfg)
}
