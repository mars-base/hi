package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mars-base/hi/pkg/config"
)

func mustParseAnthropicRequest(t *testing.T, body string) *anthropicRequest {
	t.Helper()
	req, err := parseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return req
}

func TestGeminiSystemMerge(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{
		"model": "claude-sonnet-5",
		"max_tokens": 64,
		"system": "You are Claude Code.",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "system", "content": [{"type": "text", "text": "reminder text"}]},
			{"role": "assistant", "content": "hello"},
			{"role": "user", "content": [{"type": "text", "text": "again"}]}
		]
	}`)

	out, err := convertRequestToGemini(req, "", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	if out.SystemInstruction == nil || len(out.SystemInstruction.Parts) != 1 {
		t.Fatalf("systemInstruction = %+v", out.SystemInstruction)
	}
	if got, want := out.SystemInstruction.Parts[0].Text, "You are Claude Code.\n\nreminder text"; got != want {
		t.Errorf("systemInstruction text = %q, want %q", got, want)
	}

	if len(out.Contents) != 3 {
		t.Fatalf("contents = %d, want 3: %+v", len(out.Contents), out.Contents)
	}
	if out.Contents[0].Role != "user" || out.Contents[1].Role != "model" || out.Contents[2].Role != "user" {
		t.Errorf("roles = %q/%q/%q", out.Contents[0].Role, out.Contents[1].Role, out.Contents[2].Role)
	}
}

func TestGeminiConsecutiveUserMerge(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{
		"messages": [
			{"role": "user", "content": "one"},
			{"role": "system", "content": "note"},
			{"role": "user", "content": "two"}
		]
	}`)

	out, err := convertRequestToGemini(req, "", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Contents) != 1 {
		t.Fatalf("contents = %+v, want single merged user turn", out.Contents)
	}
	if len(out.Contents[0].Parts) != 2 || out.Contents[0].Parts[0].Text != "one" || out.Contents[0].Parts[1].Text != "two" {
		t.Errorf("merged parts = %+v", out.Contents[0].Parts)
	}
}

func TestGeminiToolDegradationWithoutSignature(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{
		"messages": [
			{"role": "user", "content": "weather?"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "checking"},
				{"type": "tool_use", "id": "t1", "name": "get_weather", "input": {"city": "Paris"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": "sunny"}
			]}
		]
	}`)

	out, err := convertRequestToGemini(req, "", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	modelTurn := out.Contents[1]
	if len(modelTurn.Parts) != 2 || modelTurn.Parts[0].Text != "checking" {
		t.Fatalf("model parts = %+v", modelTurn.Parts)
	}
	if modelTurn.Parts[1].FunctionCall != nil {
		t.Fatalf("unsigned tool call must not be replayed as functionCall: %+v", modelTurn.Parts[1])
	}
	if !strings.Contains(modelTurn.Parts[1].Text, "tool call omitted: get_weather") {
		t.Errorf("degraded text = %q", modelTurn.Parts[1].Text)
	}

	userTurn := out.Contents[2]
	if len(userTurn.Parts) != 1 || userTurn.Parts[0].FunctionResponse != nil {
		t.Fatalf("tool result must degrade to text: %+v", userTurn.Parts)
	}
	if !strings.Contains(userTurn.Parts[0].Text, "Tool result (get_weather): sunny") {
		t.Errorf("degraded tool result = %q", userTurn.Parts[0].Text)
	}
}

func TestGeminiToolSignatureReplay(t *testing.T) {
	sigs := newThoughtSigStore()
	sigs.putAll([]string{"t1", "t2"}, "SIG-abc")

	req := mustParseAnthropicRequest(t, `{
		"messages": [
			{"role": "user", "content": "weather in Paris and Rome?"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "t1", "name": "get_weather", "input": {"city": "Paris"}},
				{"type": "tool_use", "id": "t2", "name": "get_weather", "input": {"city": "Rome"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": "sunny"},
				{"type": "tool_result", "tool_use_id": "t2", "content": [{"type": "text", "text": "rainy"}]}
			]}
		]
	}`)

	out, err := convertRequestToGemini(req, "", sigs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	modelTurn := out.Contents[1]
	if len(modelTurn.Parts) != 2 {
		t.Fatalf("model parts = %+v", modelTurn.Parts)
	}
	if modelTurn.Parts[0].ThoughtSignature != "SIG-abc" {
		t.Errorf("first functionCall signature = %q", modelTurn.Parts[0].ThoughtSignature)
	}
	if modelTurn.Parts[1].ThoughtSignature != "" {
		t.Errorf("second functionCall must not repeat the signature")
	}
	if modelTurn.Parts[0].FunctionCall.ID != "t1" || modelTurn.Parts[1].FunctionCall.ID != "t2" {
		t.Errorf("functionCall ids = %q/%q", modelTurn.Parts[0].FunctionCall.ID, modelTurn.Parts[1].FunctionCall.ID)
	}

	userTurn := out.Contents[2]
	if len(userTurn.Parts) != 2 {
		t.Fatalf("user parts = %+v", userTurn.Parts)
	}
	fr := userTurn.Parts[0].FunctionResponse
	if fr == nil || fr.Name != "get_weather" || fr.ID != "t1" || fr.Response["result"] != "sunny" {
		t.Errorf("functionResponse = %+v", fr)
	}
	if got := userTurn.Parts[1].FunctionResponse; got == nil || got.Response["result"] != "rainy" {
		t.Errorf("second functionResponse = %+v", got)
	}
}

func TestGeminiToolResultErrorAndImage(t *testing.T) {
	sigs := newThoughtSigStore()
	sigs.putAll([]string{"t1"}, "SIG")

	req := mustParseAnthropicRequest(t, `{
		"messages": [
			{"role": "user", "content": "read file"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "t1", "name": "read", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "is_error": true, "content": [
					{"type": "text", "text": "denied"},
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AA=="}}
				]}
			]}
		]
	}`)

	out, err := convertRequestToGemini(req, "", sigs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	fr := out.Contents[2].Parts[0].FunctionResponse
	if fr == nil {
		t.Fatalf("missing functionResponse: %+v", out.Contents[2].Parts)
	}
	if got := fr.Response["result"].(string); !strings.HasPrefix(got, "Error: denied") || !strings.Contains(got, "[image omitted]") {
		t.Errorf("functionResponse result = %q", got)
	}
}

func TestGeminiImageAndDocument(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "look"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "QUJD"}},
				{"type": "image", "source": {"type": "url", "url": "https://example.com/x.png"}},
				{"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": "UERG"}}
			]}
		]
	}`)

	out, err := convertRequestToGemini(req, "", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	parts := out.Contents[0].Parts
	if len(parts) != 4 {
		t.Fatalf("parts = %+v", parts)
	}
	if parts[1].InlineData == nil || parts[1].InlineData.MimeType != "image/png" || parts[1].InlineData.Data != "QUJD" {
		t.Errorf("inline image = %+v", parts[1])
	}
	if parts[2].FileData == nil || parts[2].FileData.FileURI != "https://example.com/x.png" {
		t.Errorf("file image = %+v", parts[2])
	}
	if parts[3].InlineData == nil || parts[3].InlineData.MimeType != "application/pdf" {
		t.Errorf("document = %+v", parts[3])
	}
}

func TestGeminiToolsAndToolChoice(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{
		"tools": [
			{"type": "custom", "name": "get_weather", "description": "weather",
			 "input_schema": {"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "properties": {"city": {"type": "string"}}}},
			{"type": "web_search_20250305", "name": "web_search"}
		],
		"tool_choice": {"type": "any"}
	}`)

	out, err := convertRequestToGemini(req, "", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Tools) != 1 || len(out.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("tools = %+v", out.Tools)
	}
	decl := out.Tools[0].FunctionDeclarations[0]
	if decl.Name != "get_weather" || len(decl.ParametersJsonSchema) == 0 {
		t.Errorf("declaration = %+v", decl)
	}
	if !strings.Contains(string(decl.ParametersJsonSchema), "$schema") {
		t.Errorf("parametersJsonSchema should keep the full schema: %s", decl.ParametersJsonSchema)
	}
	fcc := jsonMap(out.ToolConfig["functionCallingConfig"])
	if fcc["mode"] != "ANY" {
		t.Errorf("toolConfig mode = %v", fcc["mode"])
	}
}

func TestGeminiNamedToolChoice(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{
		"tools": [{"name": "get_weather", "input_schema": {"type": "object"}}],
		"tool_choice": {"type": "tool", "name": "get_weather"}
	}`)

	out, err := convertRequestToGemini(req, "", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	fcc := jsonMap(out.ToolConfig["functionCallingConfig"])
	if fcc["mode"] != "ANY" {
		t.Errorf("mode = %v", fcc["mode"])
	}
	allowed, _ := fcc["allowedFunctionNames"].([]string)
	if len(allowed) != 1 || allowed[0] != "get_weather" {
		t.Errorf("allowedFunctionNames = %v", fcc["allowedFunctionNames"])
	}
}

func TestGeminiGenerationConfig(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{
		"max_tokens": 2048,
		"temperature": 0.3,
		"top_p": 0.9,
		"stop_sequences": ["END"]
	}`)

	out, err := convertRequestToGemini(req, "high", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	cfg := out.GenerationConfig
	if cfg == nil {
		t.Fatal("missing generationConfig")
	}
	if cfg.MaxOutputTokens != 2048 || cfg.Temperature == nil || *cfg.Temperature != 0.3 || cfg.TopP == nil || *cfg.TopP != 0.9 {
		t.Errorf("generationConfig = %+v", cfg)
	}
	if len(cfg.StopSequences) != 1 || cfg.StopSequences[0] != "END" {
		t.Errorf("stopSequences = %v", cfg.StopSequences)
	}
	if cfg.ThinkingConfig == nil || cfg.ThinkingConfig.ThinkingLevel != "high" {
		t.Errorf("thinkingConfig = %+v", cfg.ThinkingConfig)
	}
}

func TestGeminiXHighThinkingLevel(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{"messages": [{"role": "user", "content": "hi"}]}`)
	out, err := convertRequestToGemini(req, "xhigh", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if out.GenerationConfig == nil || out.GenerationConfig.ThinkingConfig == nil {
		t.Fatalf("generationConfig = %+v, want thinkingConfig", out.GenerationConfig)
	}
	if got := out.GenerationConfig.ThinkingConfig.ThinkingLevel; got != "xhigh" {
		t.Errorf("thinkingLevel = %q, want xhigh", got)
	}
}

func TestGeminiGenerationConfigOmitsEmpty(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{"messages": [{"role": "user", "content": "hi"}]}`)
	out, err := convertRequestToGemini(req, "", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if out.GenerationConfig != nil {
		t.Errorf("generationConfig = %+v, want nil", out.GenerationConfig)
	}
}

func TestGeminiThinkingLevelMapping(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"none":    "",
		"minimal": "low",
		"low":     "low",
		"medium":  "medium",
		"high":    "high",
		"xhigh":   "xhigh",
		"max":     "high",
	}
	for in, want := range cases {
		if got := geminiThinkingLevel(in); got != want {
			t.Errorf("geminiThinkingLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGeminiEmptyContentsPlaceholder(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{"messages": [{"role": "system", "content": "only system"}]}`)
	out, err := convertRequestToGemini(req, "", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Contents) != 1 || out.Contents[0].Role != "user" || out.Contents[0].Parts[0].Text != "." {
		t.Errorf("contents = %+v", out.Contents)
	}
}

func TestGeminiTrailingSystemGetsUserContinuation(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"},
			{"role": "system", "content": "trailing reminder"}
		]
	}`)
	out, err := convertRequestToGemini(req, "", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	n := len(out.Contents)
	if n != 3 || out.Contents[n-1].Role != "user" {
		t.Fatalf("contents must end with a user turn: %+v", out.Contents)
	}
	if got := out.Contents[n-1].Parts[0].Text; got != "Continue." {
		t.Errorf("continuation text = %q", got)
	}
	if out.SystemInstruction == nil || !strings.Contains(out.SystemInstruction.Parts[0].Text, "trailing reminder") {
		t.Errorf("trailing system text must move into systemInstruction: %+v", out.SystemInstruction)
	}
}

func TestGeminiTrailingEmptyUserGetsUserContinuation(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"},
			{"role": "user", "content": [
				{"type": "thinking", "thinking": "internal"},
				{"type": "text", "text": ""}
			]}
		]
	}`)
	out, err := convertRequestToGemini(req, "", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	n := len(out.Contents)
	if n != 3 || out.Contents[n-1].Role != "user" {
		t.Fatalf("contents must end with a user turn: %+v", out.Contents)
	}
	if len(out.Contents[n-1].Parts) != 1 || out.Contents[n-1].Parts[0].Text != "Continue." {
		t.Errorf("continuation parts = %+v", out.Contents[n-1].Parts)
	}
}

func TestGeminiEndpointURL(t *testing.T) {
	b, err := NewBackend("gemini", config.BackendConfig{
		Type:    "gemini",
		BaseURL: "https://ai.lehihi.example/base/v1beta/",
		APIKey:  "k",
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	base := b.TargetURL()

	got := geminiEndpointURL(base, "models/gemini-4-hydrogen", false)
	if want := "https://ai.lehihi.example/base/v1beta/models/gemini-4-hydrogen:generateContent"; got.String() != want {
		t.Errorf("plain URL = %q, want %q", got.String(), want)
	}
	stream := geminiEndpointURL(base, "gemini-4-hydrogen", true)
	if want := "https://ai.lehihi.example/base/v1beta/models/gemini-4-hydrogen:streamGenerateContent?alt=sse"; stream.String() != want {
		t.Errorf("stream URL = %q, want %q", stream.String(), want)
	}
}

func TestGeminiRequestBodyOmitsModel(t *testing.T) {
	req := mustParseAnthropicRequest(t, `{"model": "claude-sonnet-5", "messages": [{"role": "user", "content": "hi"}]}`)
	out, err := convertRequestToGemini(req, "", newThoughtSigStore())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"model"`) {
		t.Errorf("gemini body must not carry a model field: %s", raw)
	}
}
