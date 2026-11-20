package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mars-base/hi/pkg/logx"
)

// minOutputTokens is the smallest max_output_tokens accepted by the
// upstream Responses API; Anthropic clients may send much smaller values.
const minOutputTokens = 16

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicRequest struct {
	Model       string                 `json:"model"`
	System      json.RawMessage        `json:"system"`
	Messages    []anthropicMessage     `json:"messages"`
	Tools       []anthropicTool        `json:"tools"`
	ToolChoice  map[string]interface{} `json:"tool_choice"`
	MaxTokens   int64                  `json:"max_tokens"`
	Stream      bool                   `json:"stream"`
	Temperature *float64               `json:"temperature"`
	TopP        *float64               `json:"top_p"`
	Thinking    map[string]interface{} `json:"thinking"`
}

func parseAnthropicRequest(body []byte) (*anthropicRequest, error) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid Anthropic request: %w", err)
	}
	return &req, nil
}

// convertRequestToResponses translates an Anthropic Messages request into an
// OpenAI Responses request body.
func convertRequestToResponses(req *anthropicRequest, mappedModel, effortOverride string) ([]byte, error) {
	out := map[string]interface{}{
		"model":  mappedModel,
		"store":  false,
		"stream": req.Stream,
	}

	if instr := systemText(req.System); instr != "" {
		out["instructions"] = instr
	}

	items, err := convertMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		items = append(items, map[string]interface{}{"role": "user", "content": "."})
	}
	out["input"] = items

	if req.MaxTokens > 0 {
		n := req.MaxTokens
		if n < minOutputTokens {
			n = minOutputTokens
		}
		out["max_output_tokens"] = n
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}

	if tools := convertTools(req.Tools); len(tools) > 0 {
		out["tools"] = tools
	}
	applyToolChoice(out, req.ToolChoice)

	out["reasoning"] = map[string]interface{}{
		"effort": chooseReasoningEffort(req.Thinking, effortOverride),
	}

	return json.Marshal(out)
}

// systemText flattens an Anthropic system field (string or text blocks).
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func convertMessages(messages []anthropicMessage) ([]interface{}, error) {
	items := make([]interface{}, 0, len(messages))

	for _, msg := range messages {
		role := msg.Role
		if role != "user" && role != "assistant" {
			logx.Debug("responses convert: skipping message with role %q", role)
			continue
		}

		var plain string
		if err := json.Unmarshal(msg.Content, &plain); err == nil {
			items = append(items, map[string]interface{}{"role": role, "content": plain})
			continue
		}

		var blocks []map[string]interface{}
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			return nil, fmt.Errorf("unsupported content for %s message: %w", role, err)
		}

		var texts []string
		var images []interface{}
		flush := func() {
			if len(texts) == 0 && len(images) == 0 {
				return
			}
			if len(images) == 0 {
				items = append(items, map[string]interface{}{
					"role":    role,
					"content": strings.Join(texts, "\n"),
				})
			} else {
				parts := make([]interface{}, 0, len(texts)+len(images))
				for _, t := range texts {
					parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
				}
				parts = append(parts, images...)
				items = append(items, map[string]interface{}{"role": role, "content": parts})
			}
			texts = nil
			images = nil
		}

		for _, block := range blocks {
			blockType, _ := block["type"].(string)
			switch blockType {
			case "text":
				if t, _ := block["text"].(string); t != "" {
					texts = append(texts, t)
				}

			case "image":
				if role != "user" {
					logx.Debug("responses convert: dropping image block in assistant message")
					continue
				}
				if part := anthropicImagePart(block); part != nil {
					images = append(images, part)
				}

			case "tool_use":
				flush()
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				if id == "" || name == "" {
					logx.Warn("responses convert: dropping tool_use block without id/name")
					continue
				}
				args := []byte("{}")
				if input, ok := block["input"]; ok && input != nil {
					if b, err := json.Marshal(input); err == nil {
						args = b
					}
				}
				items = append(items, map[string]interface{}{
					"type":      "function_call",
					"call_id":   id,
					"name":      name,
					"arguments": string(args),
				})

			case "tool_result":
				if role != "user" {
					logx.Warn("responses convert: dropping tool_result in assistant message")
					continue
				}
				flush()
				id, _ := block["tool_use_id"].(string)
				if id == "" {
					logx.Warn("responses convert: dropping tool_result without tool_use_id")
					continue
				}
				items = append(items, map[string]interface{}{
					"type":    "function_call_output",
					"call_id": id,
					"output":  toolResultOutput(block),
				})

			case "thinking", "redacted_thinking":
				// No reasoning summary is available from the upstream gateway.

			case "document":
				logx.Warn("responses convert: dropping document block (unsupported upstream)")

			case "server_tool_use", "web_search_tool_result":
				logx.Warn("responses convert: dropping server tool block %q", blockType)

			default:
				logx.Debug("responses convert: dropping unknown block type %q", blockType)
			}
		}
		flush()
	}

	return items, nil
}

func anthropicImagePart(block map[string]interface{}) map[string]interface{} {
	source, _ := block["source"].(map[string]interface{})
	if source == nil {
		return nil
	}
	switch source["type"] {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if mediaType == "" || data == "" {
			return nil
		}
		return map[string]interface{}{
			"type":      "input_image",
			"image_url": "data:" + mediaType + ";base64," + data,
		}
	case "url":
		u, _ := source["url"].(string)
		if u == "" {
			return nil
		}
		return map[string]interface{}{"type": "input_image", "image_url": u}
	default:
		return nil
	}
}

// toolResultOutput converts an Anthropic tool_result content field into an
// OpenAI function_call_output output (string or content-part list).
func toolResultOutput(block map[string]interface{}) interface{} {
	prefix := ""
	if isErr, _ := block["is_error"].(bool); isErr {
		prefix = "Error: "
	}

	switch content := block["content"].(type) {
	case string:
		return prefix + content
	case []interface{}:
		var texts []string
		var parts []interface{}
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]interface{})
			if !ok {
				continue
			}
			switch part["type"] {
			case "text":
				if t, _ := part["text"].(string); t != "" {
					texts = append(texts, t)
				}
			case "image":
				if p := anthropicImagePart(part); p != nil {
					parts = append(parts, p)
				}
			}
		}
		if len(parts) == 0 {
			return prefix + strings.Join(texts, "\n")
		}
		out := make([]interface{}, 0, len(parts)+1)
		if len(texts) > 0 {
			out = append(out, map[string]interface{}{
				"type": "input_text",
				"text": prefix + strings.Join(texts, "\n"),
			})
		}
		return append(out, parts...)
	default:
		return prefix
	}
}

func convertTools(tools []anthropicTool) []interface{} {
	out := make([]interface{}, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" || len(t.InputSchema) == 0 || string(t.InputSchema) == "null" {
			// Server-side tools (web_search etc.) have no input_schema.
			if t.Name != "" {
				logx.Warn("responses convert: skipping tool %q without input_schema", t.Name)
			}
			continue
		}
		converted := map[string]interface{}{
			"type":       "function",
			"name":       t.Name,
			"parameters": json.RawMessage(t.InputSchema),
		}
		if t.Description != "" {
			converted["description"] = t.Description
		}
		out = append(out, converted)
	}
	return out
}

func applyToolChoice(out map[string]interface{}, choice map[string]interface{}) {
	if len(choice) == 0 {
		return
	}
	switch choiceType, _ := choice["type"].(string); choiceType {
	case "auto":
		out["tool_choice"] = "auto"
	case "any":
		out["tool_choice"] = "required"
	case "none":
		out["tool_choice"] = "none"
	case "tool":
		if name, _ := choice["name"].(string); name != "" {
			out["tool_choice"] = map[string]interface{}{"type": "function", "name": name}
		}
	}
	if disable, _ := choice["disable_parallel_tool_use"].(bool); disable {
		out["parallel_tool_calls"] = false
	}
}

// chooseReasoningEffort picks an upstream reasoning effort. An explicit
// backend config wins; otherwise thinking budget maps to a tier. When
// thinking is off we use "low" so the gateway does not burn reasoning
// tokens at its default medium effort.
func chooseReasoningEffort(thinking map[string]interface{}, override string) string {
	if override != "" {
		return override
	}
	if t, _ := thinking["type"].(string); t == "enabled" {
		budget, _ := thinking["budget_tokens"].(float64)
		switch {
		case budget < 4096:
			return "low"
		case budget < 16384:
			return "medium"
		default:
			return "high"
		}
	}
	return "low"
}

// estimateInputTokens approximates the prompt size for count_tokens and for
// message_start.usage before the upstream reports real usage.
func estimateInputTokens(req *anthropicRequest) int64 {
	var size int
	if len(req.System) > 0 {
		size += len(req.System)
	}
	for _, m := range req.Messages {
		size += len(m.Role) + len(m.Content)
	}
	if tools, err := json.Marshal(req.Tools); err == nil {
		size += len(tools)
	}
	tokens := int64(size/4) + int64(len(req.Messages))*4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}
