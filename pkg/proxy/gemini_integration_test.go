package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func writeGeminiJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(body))
}

func writeGeminiSSE(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, frame := range frames {
		io.WriteString(w, "data: "+frame+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func TestGeminiIntegrationNonStreaming(t *testing.T) {
	srv, cap := newGeminiUpstream(t, func(w http.ResponseWriter, r *http.Request, index int) {
		writeGeminiJSON(w, `{
			"responseId": "abc",
			"modelVersion": "gemini-4-hydrogen",
			"candidates": [{"content": {"role": "model", "parts": [{"text": "OK"}]}, "finishReason": "STOP"}],
			"usageMetadata": {"promptTokenCount": 12, "candidatesTokenCount": 1}
		}`)
	})
	ps, _ := newTestGeminiProxy(t, srv.URL)

	w := sendGeminiMessagesRequest(ps, `{
		"model": "claude-sonnet-5",
		"max_tokens": 64,
		"system": "sys prompt",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "system", "content": "reminder"}
		]
	}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	calls := cap.snapshot()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.path != "/v1beta/models/gemini-4-hydrogen:generateContent" {
		t.Errorf("upstream path = %q", call.path)
	}
	if len(call.query) != 0 {
		t.Errorf("upstream query = %v, want none", call.query)
	}
	if got := call.header.Get("x-goog-api-key"); got != "gm-test" {
		t.Errorf("x-goog-api-key = %q", got)
	}
	if got := call.header.Get("Authorization"); got != "" {
		t.Errorf("Authorization leaked upstream: %q", got)
	}
	if got := call.header.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key leaked upstream: %q", got)
	}
	if got := call.header.Get("anthropic-version"); got != "" {
		t.Errorf("anthropic-version leaked upstream: %q", got)
	}
	if got := call.header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding = %q", got)
	}

	var sent map[string]interface{}
	if err := json.Unmarshal(call.body, &sent); err != nil {
		t.Fatalf("upstream body: %v\n%s", err, call.body)
	}
	if _, ok := sent["model"]; ok {
		t.Errorf("gemini body must not include model: %s", call.body)
	}
	sys := jsonMap(sent["systemInstruction"])
	parts := jsonList(sys["parts"])
	if len(parts) != 1 || jsonString(jsonMap(parts[0])["text"]) != "sys prompt\n\nreminder" {
		t.Errorf("systemInstruction = %v", sent["systemInstruction"])
	}
	contents := jsonList(sent["contents"])
	if len(contents) != 1 || jsonString(jsonMap(contents[0])["role"]) != "user" {
		t.Errorf("contents = %v", sent["contents"])
	}

	var msg map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("proxy response: %v\n%s", err, w.Body.String())
	}
	if msg["id"] != "msg_abc" || msg["model"] != "claude-sonnet-5" || msg["stop_reason"] != "end_turn" {
		t.Errorf("message = %v", msg)
	}
	blocks := jsonList(msg["content"])
	if len(blocks) != 1 || jsonString(jsonMap(blocks[0])["text"]) != "OK" {
		t.Errorf("content = %v", msg["content"])
	}
	usage := jsonMap(msg["usage"])
	if usage["input_tokens"].(float64) != 12 || usage["output_tokens"].(float64) != 1 {
		t.Errorf("usage = %v", usage)
	}
}

func TestGeminiIntegrationStreaming(t *testing.T) {
	srv, cap := newGeminiUpstream(t, func(w http.ResponseWriter, r *http.Request, index int) {
		writeGeminiSSE(w,
			`{"responseId":"s1","candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":2,"thoughtsTokenCount":5,"cachedContentTokenCount":10}}`,
		)
	})
	ps, _ := newTestGeminiProxy(t, srv.URL)

	w := sendGeminiMessagesRequest(ps, `{
		"model": "claude-sonnet-5",
		"max_tokens": 64,
		"stream": true,
		"messages": [{"role": "user", "content": "hi"}]
	}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	calls := cap.snapshot()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d", len(calls))
	}
	if calls[0].path != "/v1beta/models/gemini-4-hydrogen:streamGenerateContent" {
		t.Errorf("upstream path = %q", calls[0].path)
	}
	if calls[0].query.Get("alt") != "sse" {
		t.Errorf("upstream query = %v", calls[0].query)
	}
	if got := calls[0].header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q", got)
	}

	events := parseTestSSE(t, w.Body.String())
	want := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if got := eventTypes(events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("event sequence = %v, want %v", got, want)
	}
	if text := collectTextDeltas(events); text != "Hello" {
		t.Errorf("text deltas = %q", text)
	}
	delta := firstEventOfType(events, "message_delta")
	if d := jsonMap(delta["delta"]); d["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", d["stop_reason"])
	}
	usage := jsonMap(delta["usage"])
	if usage["input_tokens"].(float64) != 20 || usage["output_tokens"].(float64) != 7 || usage["cache_read_input_tokens"].(float64) != 10 {
		t.Errorf("usage = %v", usage)
	}
}

func TestGeminiIntegrationToolLoopWithSignature(t *testing.T) {
	srv, cap := newGeminiUpstream(t, func(w http.ResponseWriter, r *http.Request, index int) {
		if index == 0 {
			writeGeminiSSE(w,
				`{"responseId":"t1","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"},"id":"call_1"},"thoughtSignature":"SIG-77"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":3}}`,
			)
			return
		}
		writeGeminiSSE(w,
			`{"responseId":"t2","candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":1}}`,
		)
	})
	ps, gb := newTestGeminiProxy(t, srv.URL)

	toolRequest := `{
		"model": "claude-sonnet-5",
		"max_tokens": 128,
		"stream": true,
		"tools": [{"name": "get_weather", "description": "weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
		"messages": [{"role": "user", "content": "weather in Paris?"}]
	}`
	w := sendGeminiMessagesRequest(ps, toolRequest)
	if w.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", w.Code, w.Body.String())
	}
	events := parseTestSSE(t, w.Body.String())
	start := firstEventOfType(events, "content_block_start")
	block := jsonMap(start["content_block"])
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "get_weather" {
		t.Fatalf("tool block = %v", block)
	}
	if args := collectJSONDeltas(events); args != `{"city":"Paris"}` {
		t.Errorf("tool args deltas = %q", args)
	}
	if delta := firstEventOfType(events, "message_delta"); jsonMap(delta["delta"])["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", jsonMap(delta["delta"])["stop_reason"])
	}
	if sig, ok := gb.sigs.getAny([]string{"call_1"}); !ok || sig != "SIG-77" {
		t.Fatalf("cached signature = %q, %v", sig, ok)
	}

	// Second turn replays the tool history; the proxy must attach the cached
	// signature to the functionCall and send a functionResponse.
	followUp := `{
		"model": "claude-sonnet-5",
		"max_tokens": 128,
		"stream": true,
		"tools": [{"name": "get_weather", "description": "weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
		"messages": [
			{"role": "user", "content": "weather in Paris?"},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": {"city": "Paris"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "call_1", "content": "sunny"}]}
		]
	}`
	w = sendGeminiMessagesRequest(ps, followUp)
	if w.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", w.Code, w.Body.String())
	}

	calls := cap.snapshot()
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(calls))
	}
	var sent map[string]interface{}
	if err := json.Unmarshal(calls[1].body, &sent); err != nil {
		t.Fatalf("second upstream body: %v", err)
	}
	contents := jsonList(sent["contents"])
	if len(contents) != 3 {
		t.Fatalf("contents = %v", sent["contents"])
	}
	modelParts := jsonList(jsonMap(contents[1])["parts"])
	if len(modelParts) != 1 {
		t.Fatalf("model parts = %v", modelParts)
	}
	modelPart := jsonMap(modelParts[0])
	fc := jsonMap(modelPart["functionCall"])
	if fc["name"] != "get_weather" || fc["id"] != "call_1" {
		t.Errorf("functionCall = %v", fc)
	}
	if modelPart["thoughtSignature"] != "SIG-77" {
		t.Errorf("thoughtSignature = %v", modelPart["thoughtSignature"])
	}
	userParts := jsonList(jsonMap(contents[2])["parts"])
	fr := jsonMap(jsonMap(userParts[0])["functionResponse"])
	if fr["name"] != "get_weather" {
		t.Errorf("functionResponse = %v", fr)
	}
	if resp := jsonMap(fr["response"]); resp["result"] != "sunny" {
		t.Errorf("functionResponse.response = %v", resp)
	}
}

func TestGeminiIntegrationDegradesWithoutSignatureCache(t *testing.T) {
	srv, cap := newGeminiUpstream(t, func(w http.ResponseWriter, r *http.Request, index int) {
		writeGeminiSSE(w,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1}}`,
		)
	})
	// Fresh proxy: simulates a restart with an empty signature cache.
	ps, _ := newTestGeminiProxy(t, srv.URL)

	w := sendGeminiMessagesRequest(ps, `{
		"model": "claude-sonnet-5",
		"max_tokens": 128,
		"stream": true,
		"messages": [
			{"role": "user", "content": "weather in Paris?"},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": {"city": "Paris"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "call_1", "content": "sunny"}]}
		]
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	calls := cap.snapshot()
	var sent map[string]interface{}
	if err := json.Unmarshal(calls[0].body, &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	raw := string(calls[0].body)
	if strings.Contains(raw, "functionCall") || strings.Contains(raw, "thoughtSignature") {
		t.Errorf("unsigned tool history must not be replayed as functionCall: %s", raw)
	}
	if !strings.Contains(raw, "tool call omitted: get_weather") {
		t.Errorf("missing degraded tool-call text: %s", raw)
	}
	if !strings.Contains(raw, "Tool result (get_weather): sunny") {
		t.Errorf("missing degraded tool-result text: %s", raw)
	}
}

func TestGeminiIntegrationCountTokens(t *testing.T) {
	srv, cap := newGeminiUpstream(t, func(w http.ResponseWriter, r *http.Request, index int) {
		t.Errorf("count_tokens must not hit the upstream")
	})
	ps, _ := newTestGeminiProxy(t, srv.URL)

	req := httptest.NewRequest("POST", "/v1/messages/count_tokens",
		strings.NewReader(`{"model": "claude-sonnet-5", "messages": [{"role": "user", "content": "hello"}]}`))
	req.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	ps.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	if n, ok := resp["input_tokens"].(float64); !ok || n < 1 {
		t.Errorf("input_tokens = %v", resp["input_tokens"])
	}
	if calls := cap.snapshot(); len(calls) != 0 {
		t.Errorf("upstream calls = %d, want 0", len(calls))
	}
}

func TestGeminiIntegrationMaxTokensEmptyStream(t *testing.T) {
	srv, _ := newGeminiUpstream(t, func(w http.ResponseWriter, r *http.Request, index int) {
		writeGeminiSSE(w,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":16}}`,
		)
	})
	ps, _ := newTestGeminiProxy(t, srv.URL)

	w := sendGeminiMessagesRequest(ps, `{
		"model": "claude-sonnet-5",
		"max_tokens": 16,
		"stream": true,
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	events := parseTestSSE(t, w.Body.String())
	if text := collectTextDeltas(events); text != geminiEmptyMaxTokensText {
		t.Errorf("text deltas = %q", text)
	}
	if d := jsonMap(firstEventOfType(events, "message_delta")["delta"]); d["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %v", d["stop_reason"])
	}
}

func TestGeminiIntegrationUpstreamError(t *testing.T) {
	srv, _ := newGeminiUpstream(t, func(w http.ResponseWriter, r *http.Request, index int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": {"message": "API key not valid", "status": "INVALID_ARGUMENT", "code": 400}}`))
	})
	ps, _ := newTestGeminiProxy(t, srv.URL)

	w := sendGeminiMessagesRequest(ps, `{
		"model": "claude-sonnet-5",
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	errObj := jsonMap(resp["error"])
	if resp["type"] != "error" || !strings.Contains(jsonString(errObj["message"]), "API key not valid") {
		t.Errorf("error = %v", resp)
	}
	if errObj["type"] != "invalid_request_error" {
		t.Errorf("error type = %v", errObj["type"])
	}
}

func TestGeminiIntegrationPingWhileWaiting(t *testing.T) {
	old := geminiPingInterval
	geminiPingInterval = 15 * time.Millisecond
	t.Cleanup(func() { geminiPingInterval = old })

	srv, _ := newGeminiUpstream(t, func(w http.ResponseWriter, r *http.Request, index int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(80 * time.Millisecond)
		io.WriteString(w, "data: "+`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1}}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	ps, _ := newTestGeminiProxy(t, srv.URL)

	w := sendGeminiMessagesRequest(ps, `{
		"model": "claude-sonnet-5",
		"max_tokens": 64,
		"stream": true,
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	events := parseTestSSE(t, w.Body.String())
	if len(events) == 0 || events[0].Name != "message_start" {
		t.Fatalf("first event = %+v", events)
	}
	pings := 0
	for _, ev := range events {
		if ev.Name == "ping" {
			pings++
		}
	}
	if pings == 0 {
		t.Errorf("no ping events in: %v", eventTypes(events))
	}
}
