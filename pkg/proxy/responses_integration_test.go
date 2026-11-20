package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type upstreamCapture struct {
	mu     sync.Mutex
	calls  int
	path   string
	header http.Header
	body   []byte
}

func (c *upstreamCapture) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.path = r.URL.Path
	c.header = r.Header.Clone()
	c.body = body
}

func (c *upstreamCapture) snapshot() (calls int, path string, header http.Header, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.path, c.header, c.body
}

func newFixtureUpstream(t *testing.T, status int, contentType, body string) (*httptest.Server, *upstreamCapture) {
	t.Helper()
	cap := &upstreamCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

const responsesJSONFixture = `{
	"id": "resp_json",
	"model": "global.openai.gpt-6-luna",
	"status": "completed",
	"output": [
		{"type": "reasoning", "id": "rs_1"},
		{"type": "message", "id": "msg_1", "role": "assistant", "content": [
			{"type": "output_text", "text": "ok"}
		]}
	],
	"usage": {
		"input_tokens": 30,
		"input_tokens_details": {"cached_tokens": 10, "cache_write_tokens": 0},
		"output_tokens": 7
	}
}`

func TestIntegrationNonStreaming(t *testing.T) {
	srv, cap := newFixtureUpstream(t, http.StatusOK, "application/json", responsesJSONFixture)
	ps, _ := newTestResponsesProxy(t, srv.URL)

	w := sendProxyMessagesRequest(ps, `{
		"model": "claude-sonnet-5",
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "hi"}]
	}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	calls, path, header, upstreamBody := cap.snapshot()
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if path != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses", path)
	}
	if got := header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got)
	}
	if got := header.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key leaked upstream: %q", got)
	}
	if got := header.Get("anthropic-version"); got != "" {
		t.Errorf("anthropic-version leaked upstream: %q", got)
	}
	if got := header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding = %q, want identity", got)
	}

	var sent map[string]interface{}
	if err := json.Unmarshal(upstreamBody, &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if sent["model"] != "gpt-6-luna" {
		t.Errorf("upstream model = %v", sent["model"])
	}
	if sent["store"] != false {
		t.Errorf("upstream store = %v, want false", sent["store"])
	}

	var msg map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("proxy response: %v\n%s", err, w.Body.String())
	}
	if msg["type"] != "message" || msg["model"] != "claude-sonnet-5" {
		t.Errorf("response message = %v", msg)
	}
	usage := msg["usage"].(map[string]interface{})
	if usage["input_tokens"].(float64) != 20 {
		t.Errorf("input_tokens = %v, want 30-10", usage["input_tokens"])
	}

	if got := ps.costTracker.TotalRequests(); got != 1 {
		t.Errorf("cost requests = %d, want 1", got)
	}
	bt := ps.costTracker.usage["gpt"]
	if bt == nil || bt.InputTokens != 20 || bt.OutputTokens != 7 {
		t.Errorf("cost usage = %+v, want in=20 out=7", bt)
	}
}

func TestIntegrationStreaming(t *testing.T) {
	srv, _ := newFixtureUpstream(t, http.StatusOK, "text/event-stream", streamTextFixture)
	ps, _ := newTestResponsesProxy(t, srv.URL)

	w := sendProxyMessagesRequest(ps, `{
		"model": "claude-sonnet-5",
		"max_tokens": 64,
		"stream": true,
		"messages": [{"role": "user", "content": "hi"}]
	}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}

	events := parseTestSSE(t, w.Body.String())
	if text := collectTextDeltas(events); text != "Hello!" {
		t.Errorf("text = %q", text)
	}
	if firstEventOfType(events, "message_delta")["delta"].(map[string]interface{})["stop_reason"] != "end_turn" {
		t.Error("stop_reason must be end_turn")
	}
	if got := ps.costTracker.TotalRequests(); got != 1 {
		t.Errorf("cost requests = %d", got)
	}
}

func TestIntegrationCountTokensIsLocal(t *testing.T) {
	srv, cap := newFixtureUpstream(t, http.StatusOK, "application/json", `{}`)
	ps, _ := newTestResponsesProxy(t, srv.URL)

	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"claude-sonnet-5","system":"You are helpful.","messages":[{"role":"user","content":"hello world"}]}`))
	req.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	ps.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("count_tokens response: %v", err)
	}
	if n, ok := resp["input_tokens"].(float64); !ok || n < 1 {
		t.Errorf("input_tokens = %v, want positive estimate", resp["input_tokens"])
	}
	if calls, _, _, _ := cap.snapshot(); calls != 0 {
		t.Errorf("count_tokens must not hit upstream, got %d calls", calls)
	}
}

func TestIntegrationUpstreamErrorMapping(t *testing.T) {
	srv, _ := newFixtureUpstream(t, http.StatusTooManyRequests, "application/json",
		`{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limited"}}`)
	ps, _ := newTestResponsesProxy(t, srv.URL)

	w := sendProxyMessagesRequest(ps, `{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body=%s)", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("error response: %v", err)
	}
	if resp["type"] != "error" {
		t.Errorf("type = %v", resp["type"])
	}
	errObj := resp["error"].(map[string]interface{})
	if errObj["type"] != "rate_limit_error" {
		t.Errorf("error.type = %v", errObj["type"])
	}
	if !strings.Contains(errObj["message"].(string), "slow down") {
		t.Errorf("error.message = %v", errObj["message"])
	}
}

func TestIntegrationBaseURLWithV1Suffix(t *testing.T) {
	srv, cap := newFixtureUpstream(t, http.StatusOK, "application/json", responsesJSONFixture)
	ps, _ := newTestResponsesProxy(t, srv.URL+"/v1")

	w := sendProxyMessagesRequest(ps, `{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if _, path, _, _ := cap.snapshot(); path != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses (base URL /v1 must not double up)", path)
	}
}

func TestIntegrationStaleBackendModelIsRemapped(t *testing.T) {
	srv, cap := newFixtureUpstream(t, http.StatusOK, "application/json", responsesJSONFixture)
	ps, _ := newTestResponsesProxy(t, srv.URL)

	// Claude Code may keep sending the previous backend's model name after a
	// switch; the reverse map must recover claude-sonnet-5.
	w := sendProxyMessagesRequest(ps, `{"model":"gpt-6-luna","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	_, _, _, upstreamBody := cap.snapshot()
	var sent map[string]interface{}
	json.Unmarshal(upstreamBody, &sent)
	if sent["model"] != "gpt-6-luna" {
		t.Errorf("upstream model = %v", sent["model"])
	}
	var msg map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &msg)
	if msg["model"] != "claude-sonnet-5" {
		t.Errorf("response model = %v, want canonical claude-sonnet-5", msg["model"])
	}
}

func TestIntegrationStreamRequestedButUpstreamJSON(t *testing.T) {
	srv, _ := newFixtureUpstream(t, http.StatusOK, "application/json", responsesJSONFixture)
	ps, _ := newTestResponsesProxy(t, srv.URL)

	w := sendProxyMessagesRequest(ps, `{"model":"claude-sonnet-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want SSE", ct)
	}
	events := parseTestSSE(t, w.Body.String())
	if text := collectTextDeltas(events); text != "ok" {
		t.Errorf("text = %q", text)
	}
	if firstEventOfType(events, "message_stop") == nil {
		t.Error("missing message_stop")
	}
}

func TestIntegrationNonStreamRequestedButUpstreamSSE(t *testing.T) {
	srv, _ := newFixtureUpstream(t, http.StatusOK, "text/event-stream", streamTextFixture)
	ps, _ := newTestResponsesProxy(t, srv.URL)

	w := sendProxyMessagesRequest(ps, `{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("expected JSON response: %v\n%s", err, w.Body.String())
	}
	content := msg["content"].([]interface{})
	block := content[0].(map[string]interface{})
	if block["text"] != "Hello!" {
		t.Errorf("content = %v", content)
	}
}
