package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/mars-base/hi/pkg/config"
)

// newTestGeminiProxy builds a ProxyState backed by a single gemini backend
// pointing at upstreamURL. The cost tracker is in-memory only so tests never
// touch ~/.hi/.cost.json.
func newTestGeminiProxy(t *testing.T, upstreamURL string) (*ProxyState, *geminiBackend) {
	t.Helper()
	b, err := NewBackend("gemini", config.BackendConfig{
		Type:    "gemini",
		BaseURL: upstreamURL,
		APIKey:  "gm-test",
		Models: config.ModelMapping{
			Opus:   "gemini-4-hydrogen",
			Sonnet: "gemini-4-hydrogen",
			Haiku:  "gemini-4-hydrogen",
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	gb, ok := b.(*geminiBackend)
	if !ok {
		t.Fatalf("backend type = %T, want *geminiBackend", b)
	}
	ps := &ProxyState{
		active:          "gemini",
		backends:        map[string]Backend{"gemini": b},
		reverseModelMap: map[string]string{"gemini-4-hydrogen": "claude-sonnet-5"},
		costTracker: &CostTracker{
			usage:   make(map[string]*backendTokens),
			pricing: map[string]config.PricingPerMillion{},
		},
	}
	return ps, gb
}

type geminiUpstreamCall struct {
	path   string
	query  url.Values
	header http.Header
	body   []byte
}

type geminiUpstreamCapture struct {
	mu    sync.Mutex
	calls []geminiUpstreamCall
}

func (c *geminiUpstreamCapture) record(r *http.Request) int {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, geminiUpstreamCall{
		path:   r.URL.Path,
		query:  r.URL.Query(),
		header: r.Header.Clone(),
		body:   body,
	})
	return len(c.calls) - 1
}

func (c *geminiUpstreamCapture) snapshot() []geminiUpstreamCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]geminiUpstreamCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// newGeminiUpstream starts a fixture upstream; handler receives the
// zero-based call index so tests can vary responses per turn.
func newGeminiUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, index int)) (*httptest.Server, *geminiUpstreamCapture) {
	t.Helper()
	cap := &geminiUpstreamCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := cap.record(r)
		handler(w, r, index)
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func sendGeminiMessagesRequest(ps *ProxyState, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", "client-key")
	req.Header.Set("authorization", "Bearer client-token")
	req.Header.Set("anthropic-version", "2023-06-01")
	w := httptest.NewRecorder()
	ps.ServeHTTP(w, req)
	return w
}
