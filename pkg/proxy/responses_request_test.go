package proxy

import (
	"encoding/json"
	"testing"
)

func mustParseRequest(t *testing.T, body string) *anthropicRequest {
	t.Helper()
	req, err := parseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatalf("parseAnthropicRequest: %v", err)
	}
	return req
}

func mustConvertRequest(t *testing.T, body, mappedModel, effort string) map[string]interface{} {
	t.Helper()
	req := mustParseRequest(t, body)
	out, err := convertRequestToResponses(req, mappedModel, effort)
	if err != nil {
		t.Fatalf("convertRequestToResponses: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("converted body is not JSON: %v", err)
	}
	return m
}

func TestConvertRequestBasicText(t *testing.T) {
	out := mustConvertRequest(t, `{
		"model": "claude-sonnet-5",
		"max_tokens": 8,
		"stream": true,
		"system": [
			{"type": "text", "text": "Be terse.", "cache_control": {"type": "ephemeral"}},
			{"type": "text", "text": "Answer in English."}
		],
		"messages": [{"role": "user", "content": "hi"}]
	}`, "gpt-6-luna", "")

	if out["model"] != "gpt-6-luna" {
		t.Errorf("model = %v", out["model"])
	}
	if out["store"] != false {
		t.Errorf("store = %v, want false", out["store"])
	}
	if out["stream"] != true {
		t.Errorf("stream = %v, want true", out["stream"])
	}
	if got := out["instructions"]; got != "Be terse.\n\nAnswer in English." {
		t.Errorf("instructions = %v", got)
	}
	if got := out["max_output_tokens"].(float64); got != minOutputTokens {
		t.Errorf("max_output_tokens = %v, want %d", got, minOutputTokens)
	}
	if _, ok := out["thinking"]; ok {
		t.Error("thinking must not be forwarded")
	}
	reasoning := out["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "low" {
		t.Errorf("effort = %v, want low when thinking is off", reasoning["effort"])
	}
	input := out["input"].([]interface{})
	msg := input[0].(map[string]interface{})
	if msg["role"] != "user" || msg["content"] != "hi" {
		t.Errorf("input[0] = %v", msg)
	}
}

func TestConvertRequestKeepsMaxTokensAboveMinimum(t *testing.T) {
	out := mustConvertRequest(t, `{"model":"claude-sonnet-5","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`, "gpt-6-luna", "")
	if got := out["max_output_tokens"].(float64); got != 1000 {
		t.Errorf("max_output_tokens = %v, want 1000", got)
	}
}

func TestConvertRequestOmitsMaxTokensWhenUnset(t *testing.T) {
	out := mustConvertRequest(t, `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`, "gpt-6-luna", "")
	if _, ok := out["max_output_tokens"]; ok {
		t.Error("max_output_tokens should be omitted when max_tokens is absent")
	}
}

func TestConvertRequestToolLoop(t *testing.T) {
	out := mustConvertRequest(t, `{
		"model": "claude-sonnet-5",
		"system": "You are helpful.",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Weather in Beijing?"}]},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "secret", "signature": "sig"},
				{"type": "text", "text": "Let me check."},
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Beijing"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "sunny, 21C"},
				{"type": "text", "text": "Thanks!"}
			]}
		]
	}`, "gpt-6-luna", "")

	input := out["input"].([]interface{})
	if len(input) != 5 {
		t.Fatalf("input has %d items, want 5: %v", len(input), input)
	}

	if m := input[0].(map[string]interface{}); m["role"] != "user" || m["content"] != "Weather in Beijing?" {
		t.Errorf("input[0] = %v", m)
	}
	if m := input[1].(map[string]interface{}); m["role"] != "assistant" || m["content"] != "Let me check." {
		t.Errorf("input[1] = %v", m)
	}

	call := input[2].(map[string]interface{})
	if call["type"] != "function_call" || call["call_id"] != "toolu_1" || call["name"] != "get_weather" {
		t.Errorf("function_call = %v", call)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(call["arguments"].(string)), &args); err != nil || args["city"] != "Beijing" {
		t.Errorf("arguments = %v (err=%v)", call["arguments"], err)
	}

	result := input[3].(map[string]interface{})
	if result["type"] != "function_call_output" || result["call_id"] != "toolu_1" || result["output"] != "sunny, 21C" {
		t.Errorf("function_call_output = %v", result)
	}

	if m := input[4].(map[string]interface{}); m["role"] != "user" || m["content"] != "Thanks!" {
		t.Errorf("input[4] = %v", m)
	}
}

func TestConvertRequestToolResultErrorAndImages(t *testing.T) {
	out := mustConvertRequest(t, `{
		"model": "claude-sonnet-5",
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_9", "name": "read_file", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_9", "is_error": true, "content": [
					{"type": "text", "text": "file not readable"},
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
				]}
			]}
		]
	}`, "gpt-6-luna", "")

	input := out["input"].([]interface{})
	result := input[1].(map[string]interface{})
	parts, ok := result["output"].([]interface{})
	if !ok || len(parts) != 2 {
		t.Fatalf("output = %v, want 2 content parts", result["output"])
	}
	textPart := parts[0].(map[string]interface{})
	if textPart["type"] != "input_text" || textPart["text"] != "Error: file not readable" {
		t.Errorf("text part = %v", textPart)
	}
	imgPart := parts[1].(map[string]interface{})
	if imgPart["type"] != "input_image" || imgPart["image_url"] != "data:image/png;base64,AAAA" {
		t.Errorf("image part = %v", imgPart)
	}
}

func TestConvertRequestUserImage(t *testing.T) {
	out := mustConvertRequest(t, `{
		"model": "claude-sonnet-5",
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "What is this?"},
			{"type": "image", "source": {"type": "url", "url": "https://example.com/a.png"}}
		]}]
	}`, "gpt-6-luna", "")

	input := out["input"].([]interface{})
	msg := input[0].(map[string]interface{})
	parts := msg["content"].([]interface{})
	if len(parts) != 2 {
		t.Fatalf("parts = %v", parts)
	}
	if parts[0].(map[string]interface{})["text"] != "What is this?" {
		t.Errorf("text part = %v", parts[0])
	}
	if parts[1].(map[string]interface{})["image_url"] != "https://example.com/a.png" {
		t.Errorf("image part = %v", parts[1])
	}
}

func TestConvertRequestToolsAndChoice(t *testing.T) {
	out := mustConvertRequest(t, `{
		"model": "claude-sonnet-5",
		"max_tokens": 100,
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [
			{"name": "get_weather", "description": "Get weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}},
			{"type": "web_search_20250305", "name": "web_search", "max_uses": 5}
		],
		"tool_choice": {"type": "any", "disable_parallel_tool_use": true}
	}`, "gpt-6-luna", "")

	tools := out["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want only the custom function", tools)
	}
	tool := tools[0].(map[string]interface{})
	if tool["type"] != "function" || tool["name"] != "get_weather" || tool["description"] != "Get weather" {
		t.Errorf("tool = %v", tool)
	}
	if _, ok := tool["parameters"].(map[string]interface{}); !ok {
		t.Errorf("parameters = %v", tool["parameters"])
	}
	if out["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v, want required", out["tool_choice"])
	}
	if out["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v, want false", out["parallel_tool_calls"])
	}
}

func TestConvertRequestNamedToolChoice(t *testing.T) {
	out := mustConvertRequest(t, `{
		"model": "claude-sonnet-5",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"name": "get_weather", "input_schema": {"type": "object"}}],
		"tool_choice": {"type": "tool", "name": "get_weather"}
	}`, "gpt-6-luna", "")
	choice := out["tool_choice"].(map[string]interface{})
	if choice["type"] != "function" || choice["name"] != "get_weather" {
		t.Errorf("tool_choice = %v", choice)
	}
}

func TestChooseReasoningEffort(t *testing.T) {
	cases := []struct {
		name     string
		thinking map[string]interface{}
		override string
		want     string
	}{
		{"thinking off", nil, "", "low"},
		{"small budget", map[string]interface{}{"type": "enabled", "budget_tokens": 3000.0}, "", "low"},
		{"medium budget", map[string]interface{}{"type": "enabled", "budget_tokens": 8000.0}, "", "medium"},
		{"large budget", map[string]interface{}{"type": "enabled", "budget_tokens": 32000.0}, "", "high"},
		{"override wins", map[string]interface{}{"type": "enabled", "budget_tokens": 3000.0}, "max", "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseReasoningEffort(tc.thinking, tc.override); got != tc.want {
				t.Errorf("chooseReasoningEffort = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEstimateInputTokens(t *testing.T) {
	req := mustParseRequest(t, `{
		"model": "claude-sonnet-5",
		"system": "You are a helpful assistant.",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": [{"type": "text", "text": "hi there"}]}
		]
	}`)
	n := estimateInputTokens(req)
	if n < 10 || n > 200 {
		t.Errorf("estimateInputTokens = %d, want a plausible small count", n)
	}
}
