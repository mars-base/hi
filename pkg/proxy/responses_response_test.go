package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func decodeAnthropicMessage(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var msg map[string]interface{}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("decode converted message: %v\n%s", err, body)
	}
	return msg
}

func TestConvertResponsesJSONText(t *testing.T) {
	body := []byte(`{
		"id": "resp_abc123",
		"model": "global.openai.gpt-6-luna",
		"status": "completed",
		"output": [
			{"type": "reasoning", "id": "rs_1", "encrypted_content": "opaque"},
			{"type": "message", "id": "msg_1", "role": "assistant", "content": [
				{"type": "output_text", "text": "Hello there."}
			]}
		],
		"usage": {
			"input_tokens": 100,
			"input_tokens_details": {"cached_tokens": 40, "cache_write_tokens": 10},
			"output_tokens": 20
		}
	}`)

	out, usage, err := convertResponsesJSON(body, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("convertResponsesJSON: %v", err)
	}
	msg := decodeAnthropicMessage(t, out)

	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("message = %v", msg)
	}
	if msg["id"] != "msg_abc123" {
		t.Errorf("id = %v", msg["id"])
	}
	if msg["model"] != "claude-sonnet-5" {
		t.Errorf("model = %v", msg["model"])
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", msg["stop_reason"])
	}

	content := msg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("content = %v", content)
	}
	block := content[0].(map[string]interface{})
	if block["type"] != "text" || block["text"] != "Hello there." {
		t.Errorf("text block = %v", block)
	}

	usageMap := msg["usage"].(map[string]interface{})
	if usageMap["input_tokens"].(float64) != 50 {
		t.Errorf("input_tokens = %v, want 100-40-10=50", usageMap["input_tokens"])
	}
	if usageMap["output_tokens"].(float64) != 20 {
		t.Errorf("output_tokens = %v", usageMap["output_tokens"])
	}
	if usageMap["cache_read_input_tokens"].(float64) != 40 {
		t.Errorf("cache_read_input_tokens = %v", usageMap["cache_read_input_tokens"])
	}
	if usageMap["cache_creation_input_tokens"].(float64) != 10 {
		t.Errorf("cache_creation_input_tokens = %v", usageMap["cache_creation_input_tokens"])
	}
	if usage.OutputTokens != 20 {
		t.Errorf("returned usage output = %d", usage.OutputTokens)
	}
}

func TestConvertResponsesJSONToolUse(t *testing.T) {
	body := []byte(`{
		"id": "resp_tool",
		"model": "global.openai.gpt-6-luna",
		"status": "completed",
		"output": [
			{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"Beijing\"}"}
		],
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`)

	out, _, err := convertResponsesJSON(body, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("convertResponsesJSON: %v", err)
	}
	msg := decodeAnthropicMessage(t, out)

	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", msg["stop_reason"])
	}
	block := msg["content"].([]interface{})[0].(map[string]interface{})
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "get_weather" {
		t.Errorf("tool_use block = %v", block)
	}
	input := block["input"].(map[string]interface{})
	if input["city"] != "Beijing" {
		t.Errorf("input = %v", input)
	}
}

func TestConvertResponsesJSONInvalidArguments(t *testing.T) {
	body := []byte(`{
		"id": "resp_bad_args",
		"status": "completed",
		"output": [
			{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "f", "arguments": "{not json"}
		]
	}`)
	out, _, err := convertResponsesJSON(body, "")
	if err != nil {
		t.Fatalf("convertResponsesJSON: %v", err)
	}
	msg := decodeAnthropicMessage(t, out)
	block := msg["content"].([]interface{})[0].(map[string]interface{})
	input := block["input"].(map[string]interface{})
	if input["_raw_arguments"] != "{not json" {
		t.Errorf("input = %v", input)
	}
}

func TestConvertResponsesJSONIncomplete(t *testing.T) {
	body := []byte(`{
		"id": "resp_truncated",
		"model": "global.openai.gpt-6-luna",
		"status": "incomplete",
		"incomplete_details": {"reason": "max_output_tokens"},
		"output": [
			{"type": "reasoning", "id": "rs_1"}
		],
		"usage": {"input_tokens": 16, "output_tokens": 16}
	}`)
	out, _, err := convertResponsesJSON(body, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("convertResponsesJSON: %v", err)
	}
	msg := decodeAnthropicMessage(t, out)

	if msg["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %v, want max_tokens", msg["stop_reason"])
	}
	content := msg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("content = %v, want one empty text block", content)
	}
	block := content[0].(map[string]interface{})
	if block["type"] != "text" || block["text"] != "" {
		t.Errorf("block = %v", block)
	}
}

func TestConvertResponsesJSONTruncatedToolCallIsNotToolUse(t *testing.T) {
	body := []byte(`{
		"id": "resp_trunc_tool",
		"status": "incomplete",
		"incomplete_details": {"reason": "max_output_tokens"},
		"output": [
			{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "f", "arguments": "{\"a\":"},
			{"type": "message", "id": "msg_1", "content": [{"type": "output_text", "text": "partial"}]}
		]
	}`)
	out, _, err := convertResponsesJSON(body, "")
	if err != nil {
		t.Fatalf("convertResponsesJSON: %v", err)
	}
	msg := decodeAnthropicMessage(t, out)
	if msg["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %v, want max_tokens (truncated tool args must not be tool_use)", msg["stop_reason"])
	}
}

func TestUpstreamErrorMessage(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`, "model not found (code: model_not_found)"},
		{`{"error":{"message":"bad key"}}`, "bad key"},
		{`plain text error`, "plain text error"},
		{``, "upstream returned an error"},
	}
	for _, tc := range cases {
		if got := upstreamErrorMessage([]byte(tc.body)); got != tc.want {
			t.Errorf("upstreamErrorMessage(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestAnthropicErrorType(t *testing.T) {
	cases := map[int]string{
		400: "invalid_request_error",
		401: "authentication_error",
		403: "permission_error",
		404: "not_found_error",
		429: "rate_limit_error",
		500: "api_error",
		503: "api_error",
	}
	for status, want := range cases {
		if got := anthropicErrorType(status); got != want {
			t.Errorf("anthropicErrorType(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestEmitAnthropicMessageSSE(t *testing.T) {
	msg := map[string]interface{}{
		"id":            "msg_1",
		"type":          "message",
		"role":          "assistant",
		"model":         "claude-sonnet-5",
		"stop_reason":   "tool_use",
		"stop_sequence": nil,
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "checking"},
			map[string]interface{}{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": map[string]interface{}{"city": "Beijing"}},
		},
		"usage": map[string]interface{}{"input_tokens": 10.0, "output_tokens": 5.0},
	}
	var sb strings.Builder
	w := &stringResponseWriter{header: make(http.Header), body: &sb}
	emitAnthropicMessageSSE(w, msg)

	events := parseTestSSE(t, sb.String())
	if got := strings.Join(eventTypes(events), ","); got != "message_start,content_block_start,content_block_delta,content_block_stop,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop" {
		t.Errorf("event sequence = %s", got)
	}
	if text := collectTextDeltas(events); text != "checking" {
		t.Errorf("text deltas = %q", text)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(collectJSONDeltas(events)), &args); err != nil || args["city"] != "Beijing" {
		t.Errorf("tool args = %q (err=%v)", collectJSONDeltas(events), err)
	}
	delta := firstEventOfType(events, "message_delta")
	if delta["delta"].(map[string]interface{})["stop_reason"] != "tool_use" {
		t.Errorf("message_delta = %v", delta)
	}
}

// stringResponseWriter is a minimal http.ResponseWriter for SSE tests.
type stringResponseWriter struct {
	header http.Header
	status int
	body   *strings.Builder
}

func (w *stringResponseWriter) Header() http.Header { return w.header }

func (w *stringResponseWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func (w *stringResponseWriter) WriteHeader(status int) { w.status = status }
