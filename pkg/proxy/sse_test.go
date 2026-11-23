package proxy

import (
	"encoding/json"
	"testing"

	"github.com/mars-base/hi/pkg/config"
)

func newTestDeepSeekBackend(t *testing.T) Backend {
	t.Helper()
	backend, err := newDeepSeekBackend("deepseek", config.BackendConfig{
		Type: "deepseek",
		Models: config.ModelMapping{
			Opus:   "deepseek-flash",
			Sonnet: "deepseek-flash",
			Haiku:  "deepseek-flash",
		},
		ReasoningEffort: "max",
	})
	if err != nil {
		t.Fatalf("newDeepSeekBackend: %v", err)
	}
	return backend
}

func transform(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	ps := &ProxyState{reverseModelMap: map[string]string{}}
	out, originalModel := ps.transformRequestBody([]byte(body), true, "deepseek", newTestDeepSeekBackend(t))
	if originalModel != "claude-sonnet-5" {
		t.Fatalf("originalModel = %q, want claude-sonnet-5", originalModel)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal transformed body: %v", err)
	}
	return parsed
}

func assistantBlocks(t *testing.T, parsed map[string]interface{}, idx int) []interface{} {
	t.Helper()
	messages := parsed["messages"].([]interface{})
	msg := messages[idx].(map[string]interface{})
	blocks, ok := msg["content"].([]interface{})
	if !ok {
		t.Fatalf("messages[%d].content is not an array", idx)
	}
	return blocks
}

func assertThinkingEnabled(t *testing.T, parsed map[string]interface{}) {
	t.Helper()
	think, ok := parsed["thinking"].(map[string]interface{})
	if !ok {
		t.Fatalf("thinking missing from transformed body")
	}
	if think["type"] != "enabled" {
		t.Fatalf("thinking.type = %v, want enabled", think["type"])
	}
}

// Switching from gemini leaves assistant turns without thinking blocks; a
// continued tool-use turn triggers a 400 from deepseek unless a thinking
// block is passed back.
func TestTransformGeminiHistoryToDeepSeek(t *testing.T) {
	body := `{
		"model": "claude-sonnet-5",
		"messages": [
			{"role": "user", "content": "switch the backend"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "switching now"},
				{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": {"command": "true"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "done"}
			]}
		]
	}`

	parsed := transform(t, body)

	if parsed["model"] != "deepseek-flash" {
		t.Fatalf("model = %v, want deepseek-flash", parsed["model"])
	}
	assertThinkingEnabled(t, parsed)

	blocks := assistantBlocks(t, parsed, 1)
	if len(blocks) != 3 {
		t.Fatalf("assistant blocks = %d, want 3", len(blocks))
	}
	first := blocks[0].(map[string]interface{})
	if first["type"] != "thinking" {
		t.Fatalf("assistant content[0].type = %v, want thinking", first["type"])
	}

	out, ok := parsed["output_config"].(map[string]interface{})
	if !ok || out["effort"] != "max" {
		t.Fatalf("output_config = %v, want effort=max", parsed["output_config"])
	}

	user := parsed["messages"].([]interface{})[2].(map[string]interface{})
	userBlocks := user["content"].([]interface{})
	if userBlocks[0].(map[string]interface{})["type"] != "tool_result" {
		t.Fatalf("user message was modified")
	}
}

func TestTransformMixedThinkingState(t *testing.T) {
	body := `{
		"model": "claude-sonnet-5",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "thought"},
				{"type": "text", "text": "hello"}
			]},
			{"role": "user", "content": "again"},
			{"role": "assistant", "content": [{"type": "text", "text": "hi again"}]}
		]
	}`

	parsed := transform(t, body)
	assertThinkingEnabled(t, parsed)

	for _, idx := range []int{1, 3} {
		blocks := assistantBlocks(t, parsed, idx)
		if blocks[0].(map[string]interface{})["type"] != "thinking" {
			t.Fatalf("messages[%d] does not start with thinking", idx)
		}
	}
	blocks := assistantBlocks(t, parsed, 1)
	if len(blocks) != 2 {
		t.Fatalf("existing thinking block was duplicated: %d blocks", len(blocks))
	}
}

func TestTransformFreshSession(t *testing.T) {
	body := `{
		"model": "claude-sonnet-5",
		"messages": [{"role": "user", "content": "hello"}]
	}`

	parsed := transform(t, body)
	assertThinkingEnabled(t, parsed)

	msg := parsed["messages"].([]interface{})[0].(map[string]interface{})
	if msg["content"] != "hello" {
		t.Fatalf("user content = %v, want hello", msg["content"])
	}
}
