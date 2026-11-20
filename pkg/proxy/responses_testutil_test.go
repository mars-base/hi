package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mars-base/hi/pkg/config"
)

// newTestResponsesProxy builds a ProxyState backed by a single openai-response
// backend pointing at upstreamURL. The cost tracker is in-memory only so tests
// never touch ~/.hi/.cost.json.
func newTestResponsesProxy(t *testing.T, upstreamURL string) (*ProxyState, Backend) {
	t.Helper()
	b, err := NewBackend("gpt", config.BackendConfig{
		Type:    "openai-response",
		BaseURL: upstreamURL,
		APIKey:  "sk-test",
		Models: config.ModelMapping{
			Opus:   "gpt-6-luna",
			Sonnet: "gpt-6-luna",
			Haiku:  "gpt-6-luna",
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	ps := &ProxyState{
		active:          "gpt",
		backends:        map[string]Backend{"gpt": b},
		reverseModelMap: map[string]string{"gpt-6-luna": "claude-sonnet-5"},
		costTracker: &CostTracker{
			usage:   make(map[string]*backendTokens),
			pricing: map[string]config.PricingPerMillion{},
		},
	}
	return ps, b
}

func sendProxyMessagesRequest(ps *ProxyState, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	ps.ServeHTTP(w, req)
	return w
}

type testSSEEvent struct {
	Name string
	Data map[string]interface{}
}

func parseTestSSE(t *testing.T, body string) []testSSEEvent {
	t.Helper()
	var events []testSSEEvent
	err := scanSSEEvents(strings.NewReader(body), func(name string, data []byte) bool {
		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("bad SSE data %q: %v", string(data), err)
		}
		events = append(events, testSSEEvent{Name: name, Data: m})
		return false
	})
	if err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	return events
}

func eventTypes(events []testSSEEvent) []string {
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.Name)
	}
	return types
}

func firstEventOfType(events []testSSEEvent, eventType string) map[string]interface{} {
	for _, ev := range events {
		if ev.Name == eventType {
			return ev.Data
		}
	}
	return nil
}

func collectTextDeltas(events []testSSEEvent) string {
	var sb strings.Builder
	for _, ev := range events {
		if ev.Name != "content_block_delta" {
			continue
		}
		delta, _ := ev.Data["delta"].(map[string]interface{})
		if s, _ := delta["type"].(string); s == "text_delta" {
			sb.WriteString(jsonString(delta["text"]))
		}
	}
	return sb.String()
}

func collectJSONDeltas(events []testSSEEvent) string {
	var sb strings.Builder
	for _, ev := range events {
		if ev.Name != "content_block_delta" {
			continue
		}
		delta, _ := ev.Data["delta"].(map[string]interface{})
		if s, _ := delta["type"].(string); s == "input_json_delta" {
			sb.WriteString(jsonString(delta["partial_json"]))
		}
	}
	return sb.String()
}
