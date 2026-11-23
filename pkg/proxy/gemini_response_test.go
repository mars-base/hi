package proxy

import (
	"strings"
	"testing"
)

func TestConvertGeminiJSONText(t *testing.T) {
	body := `{
		"responseId": "abc123",
		"modelVersion": "gemini-4-hydrogen",
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"text": "internal reasoning", "thought": true},
				{"text": "Hello "},
				{"text": "world"}
			]},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 30,
			"candidatesTokenCount": 2,
			"thoughtsTokenCount": 7,
			"cachedContentTokenCount": 10,
			"totalTokenCount": 39
		}
	}`

	msg, usage, err := convertGeminiJSON([]byte(body), "claude-sonnet-5", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	if msg["id"] != "msg_abc123" || msg["model"] != "claude-sonnet-5" || msg["stop_reason"] != "end_turn" {
		t.Errorf("message = %v", msg)
	}
	blocks, _ := msg["content"].([]interface{})
	if len(blocks) != 1 {
		t.Fatalf("blocks = %+v", blocks)
	}
	block := jsonMap(blocks[0])
	if block["type"] != "text" || block["text"] != "Hello world" {
		t.Errorf("text block = %+v", block)
	}

	if usage.PromptTokenCount != 30 || usage.CachedContentTokenCount != 10 {
		t.Errorf("usage = %+v", usage)
	}
	u := msg["usage"].(map[string]interface{})
	if u["input_tokens"].(int64) != 20 || u["output_tokens"].(int64) != 9 || u["cache_read_input_tokens"].(int64) != 10 {
		t.Errorf("anthropic usage = %v", u)
	}
}

func TestConvertGeminiJSONToolCalls(t *testing.T) {
	sigs := newThoughtSigStore()
	body := `{
		"responseId": "r2",
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"text": "checking the weather"},
				{"functionCall": {"name": "get_weather", "args": {"city": "Paris"}, "id": "call_1"}, "thoughtSignature": "SIG-1"},
				{"functionCall": {"name": "get_weather", "args": {"city": "Rome"}, "id": "call_2"}}
			]},
			"finishReason": "STOP"
		}],
		"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 4}
	}`

	msg, _, err := convertGeminiJSON([]byte(body), "claude-sonnet-5", sigs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", msg["stop_reason"])
	}
	blocks, _ := msg["content"].([]interface{})
	if len(blocks) != 3 {
		t.Fatalf("blocks = %+v", blocks)
	}
	first := jsonMap(blocks[1])
	if first["type"] != "tool_use" || first["id"] != "call_1" || first["name"] != "get_weather" {
		t.Errorf("first tool block = %+v", first)
	}
	if input := jsonMap(first["input"]); input["city"] != "Paris" {
		t.Errorf("first tool input = %v", first["input"])
	}
	second := jsonMap(blocks[2])
	if second["id"] != "call_2" {
		t.Errorf("second tool block = %+v", second)
	}

	if sig, ok := sigs.getAny([]string{"call_1"}); !ok || sig != "SIG-1" {
		t.Errorf("signature for call_1 = %q, %v", sig, ok)
	}
	if sig, ok := sigs.getAny([]string{"call_2"}); !ok || sig != "SIG-1" {
		t.Errorf("signature for call_2 = %q, %v", sig, ok)
	}
}

func TestConvertGeminiJSONMaxTokensEmpty(t *testing.T) {
	body := `{
		"responseId": "r3",
		"candidates": [{
			"content": {"role": "model", "parts": [{"text": ""}]},
			"finishReason": "MAX_TOKENS"
		}],
		"usageMetadata": {"promptTokenCount": 5, "candidatesTokenCount": 16}
	}`

	msg, _, err := convertGeminiJSON([]byte(body), "claude-sonnet-5", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if msg["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %v", msg["stop_reason"])
	}
	blocks, _ := msg["content"].([]interface{})
	if len(blocks) != 1 || jsonMap(blocks[0])["text"] != geminiEmptyMaxTokensText {
		t.Errorf("blocks = %+v", blocks)
	}
}

func TestConvertGeminiJSONTruncatedDropsTools(t *testing.T) {
	sigs := newThoughtSigStore()
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"text": "partial answer"},
				{"functionCall": {"name": "get_weather", "args": {"city": "Par"}, "id": "call_9"}, "thoughtSignature": "SIG-9"}
			]},
			"finishReason": "MAX_TOKENS"
		}]
	}`

	msg, _, err := convertGeminiJSON([]byte(body), "claude-sonnet-5", sigs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if msg["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %v", msg["stop_reason"])
	}
	blocks, _ := msg["content"].([]interface{})
	if len(blocks) != 1 || jsonMap(blocks[0])["type"] != "text" {
		t.Errorf("blocks = %+v", blocks)
	}
	if _, ok := sigs.getAny([]string{"call_9"}); ok {
		t.Errorf("truncated tool signature must not be cached")
	}
}

func TestConvertGeminiJSONBlocked(t *testing.T) {
	body := `{"promptFeedback": {"blockReason": "SAFETY"}}`
	_, _, err := convertGeminiJSON([]byte(body), "claude-sonnet-5", newThoughtSigStore())
	if err == nil || !strings.Contains(err.Error(), "SAFETY") {
		t.Fatalf("err = %v, want block reason", err)
	}
}

func TestConvertGeminiJSONEmptyStop(t *testing.T) {
	body := `{"candidates": [{"content": {"role": "model", "parts": [{"text": "thought only", "thought": true}]}, "finishReason": "STOP"}]}`
	_, _, err := convertGeminiJSON([]byte(body), "claude-sonnet-5", newThoughtSigStore())
	if err == nil || !strings.Contains(err.Error(), "no visible content") {
		t.Fatalf("err = %v, want no-visible-content error", err)
	}
}

func TestConvertGeminiJSONErrorEnvelope(t *testing.T) {
	body := `{"error": {"message": "quota exceeded", "status": "RESOURCE_EXHAUSTED", "code": 429}}`
	_, _, err := convertGeminiJSON([]byte(body), "claude-sonnet-5", newThoughtSigStore())
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("err = %v, want upstream error", err)
	}
}

func TestThoughtSigStoreExpiryAndOverwrite(t *testing.T) {
	sigs := newThoughtSigStore()
	sigs.putAll([]string{"a", "b"}, "SIG")
	if sig, ok := sigs.getAny([]string{"b"}); !ok || sig != "SIG" {
		t.Fatalf("getAny = %q, %v", sig, ok)
	}
	sigs.putAll([]string{"a"}, "SIG2")
	if sig, _ := sigs.getAny([]string{"a"}); sig != "SIG2" {
		t.Errorf("overwrite = %q", sig)
	}
	if _, ok := sigs.getAny([]string{"missing"}); ok {
		t.Errorf("unexpected hit for unknown id")
	}
}
