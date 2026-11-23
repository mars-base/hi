package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mars-base/hi/pkg/logx"
)

// ==============================
// Native Gemini request types
// ==============================

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	FileData         *geminiFileData         `json:"fileData,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
	ID   string          `json:"id,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
	ID       string                 `json:"id,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiThinkingConfig struct {
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens int64                 `json:"maxOutputTokens,omitempty"`
	Temperature     *float64              `json:"temperature,omitempty"`
	TopP            *float64              `json:"topP,omitempty"`
	StopSequences   []string              `json:"stopSequences,omitempty"`
	ThinkingConfig  *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

type geminiFunctionDeclaration struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	ParametersJsonSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	Contents          []geminiContent         `json:"contents"`
	Tools             []geminiTool            `json:"tools,omitempty"`
	ToolConfig        map[string]interface{}  `json:"toolConfig,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

// ==============================
// Anthropic -> Gemini conversion
// ==============================

// convertRequestToGemini translates an Anthropic Messages request into a
// native Gemini generateContent body.
//
// Tool history needs special care: Gemini rejects assistant turns whose
// functionCall parts carry no thoughtSignature, and Anthropic tool_use blocks
// have no place to store one. The store remembers signatures from previous
// upstream responses; turns whose signatures are unknown are degraded to
// plain text so the conversation stays usable.
func convertRequestToGemini(req *anthropicRequest, effort string, sigs *thoughtSigStore) (*geminiRequest, error) {
	out := &geminiRequest{}

	toolNames := collectToolNames(req.Messages)
	emitted := make(map[string]bool)

	var sysTexts []string
	if t := systemText(req.System); t != "" {
		sysTexts = append(sysTexts, t)
	}

	for _, msg := range req.Messages {
		var role string
		switch msg.Role {
		case "system":
			if t := systemText(msg.Content); t != "" {
				sysTexts = append(sysTexts, t)
			}
			continue
		case "assistant":
			role = "model"
		case "user":
			role = "user"
		default:
			logx.Debug("gemini convert: skipping message with role %q", msg.Role)
			continue
		}

		var plain string
		if err := json.Unmarshal(msg.Content, &plain); err == nil {
			if plain != "" {
				appendGeminiContent(&out.Contents, role, []geminiPart{{Text: plain}})
			}
			continue
		}

		var blocks []map[string]interface{}
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			return nil, fmt.Errorf("unsupported content for %s message: %w", msg.Role, err)
		}

		var parts []geminiPart
		if role == "model" {
			parts = convertGeminiAssistantParts(blocks, sigs, emitted)
		} else {
			parts = convertGeminiUserParts(blocks, toolNames, emitted)
		}
		appendGeminiContent(&out.Contents, role, parts)
	}

	if len(sysTexts) > 0 {
		out.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: strings.Join(sysTexts, "\n\n")}},
		}
	}

	// The gateway classifies a trailing user turn by its first part: a turn
	// that starts with functionResponse and also carries text is rejected as
	// "Requests ending with a model turn are not supported." Tool results
	// merged with a following user message (summaries, reminders) hit this,
	// so keep text before functionResponse within each user turn.
	for i := range out.Contents {
		if out.Contents[i].Role == "user" {
			out.Contents[i].Parts = textBeforeFunctionResponses(out.Contents[i].Parts)
		}
	}

	if len(out.Contents) == 0 {
		out.Contents = []geminiContent{{Role: "user", Parts: []geminiPart{{Text: "."}}}}
	} else if out.Contents[len(out.Contents)-1].Role == "model" {
		// Gemini rejects histories that end with a model turn. Trailing
		// Anthropic messages that convert to no parts (system reminders,
		// empty or thinking-only user content) can leave the history
		// model-ended; append a minimal user turn so the request is accepted.
		logx.Debug("gemini convert: history ended with a model turn (tail roles: %s); appending user continuation",
			geminiTailRoles(out.Contents, 4))
		out.Contents = append(out.Contents, geminiContent{Role: "user", Parts: []geminiPart{{Text: "Continue."}}})
	}

	out.Tools = convertGeminiTools(req.Tools)
	out.ToolConfig = convertGeminiToolConfig(req.ToolChoice, len(out.Tools) > 0)
	out.GenerationConfig = buildGeminiGenerationConfig(req, effort)
	return out, nil
}

// collectToolNames maps tool_use id → tool name across the whole request so
// that functionResponse parts and degraded tool_result text can name the tool.
func collectToolNames(messages []anthropicMessage) map[string]string {
	names := make(map[string]string)
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		var blocks []map[string]interface{}
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			continue
		}
		for _, block := range blocks {
			if t, _ := block["type"].(string); t != "tool_use" {
				continue
			}
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			if id != "" && name != "" {
				names[id] = name
			}
		}
	}
	return names
}

func convertGeminiAssistantParts(blocks []map[string]interface{}, sigs *thoughtSigStore, emitted map[string]bool) []geminiPart {
	var parts []geminiPart
	var toolBlocks []map[string]interface{}
	var ids []string

	for _, block := range blocks {
		switch blockType, _ := block["type"].(string); blockType {
		case "text":
			if t, _ := block["text"].(string); t != "" {
				parts = append(parts, geminiPart{Text: t})
			}
		case "thinking", "redacted_thinking":
			// Gemini thoughts are never replayed.
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			if id == "" || name == "" {
				logx.Warn("gemini convert: dropping tool_use block without id/name")
				continue
			}
			toolBlocks = append(toolBlocks, block)
			ids = append(ids, id)
		default:
			logx.Debug("gemini convert: dropping unknown assistant block type %q", blockType)
		}
	}

	if len(toolBlocks) == 0 {
		return parts
	}

	sig, ok := sigs.getAny(ids)
	if !ok {
		logx.Debug("gemini convert: degrading %d tool_use block(s) to text (no thought signature cached)", len(toolBlocks))
		parts = append(parts, geminiPart{Text: degradedToolCallsText(toolBlocks)})
		return parts
	}

	for i, block := range toolBlocks {
		id, _ := block["id"].(string)
		name, _ := block["name"].(string)
		args := []byte("{}")
		if input, exists := block["input"]; exists && input != nil {
			if b, err := json.Marshal(input); err == nil {
				args = b
			}
		}
		part := geminiPart{FunctionCall: &geminiFunctionCall{
			Name: name,
			Args: args,
			ID:   id,
		}}
		// One signature is enough for the whole model turn; attach it to the
		// first functionCall part to keep the payload small.
		if i == 0 {
			part.ThoughtSignature = sig
		}
		parts = append(parts, part)
		emitted[id] = true
	}
	return parts
}

func degradedToolCallsText(toolBlocks []map[string]interface{}) string {
	var lines []string
	for _, block := range toolBlocks {
		name, _ := block["name"].(string)
		args := "{}"
		if input, exists := block["input"]; exists && input != nil {
			if b, err := json.Marshal(input); err == nil {
				args = string(b)
			}
		}
		lines = append(lines, fmt.Sprintf("[tool call omitted: %s(%s)]", name, args))
	}
	return strings.Join(lines, "\n")
}

func convertGeminiUserParts(blocks []map[string]interface{}, toolNames map[string]string, emitted map[string]bool) []geminiPart {
	var parts []geminiPart

	for _, block := range blocks {
		switch blockType, _ := block["type"].(string); blockType {
		case "text":
			if t, _ := block["text"].(string); t != "" {
				parts = append(parts, geminiPart{Text: t})
			}

		case "image":
			if part, ok := geminiImagePart(block); ok {
				parts = append(parts, part)
			} else {
				logx.Debug("gemini convert: dropping unrepresentable image block")
			}

		case "document":
			if part, ok := geminiDocumentPart(block); ok {
				parts = append(parts, part)
			} else {
				logx.Warn("gemini convert: replacing unrepresentable document block with placeholder")
				parts = append(parts, geminiPart{Text: "[document omitted]"})
			}

		case "tool_result":
			id, _ := block["tool_use_id"].(string)
			if id == "" {
				logx.Warn("gemini convert: dropping tool_result without tool_use_id")
				continue
			}
			name := toolNames[id]
			if name == "" {
				name = "tool"
			}
			output := toolResultText(block)
			if emitted[id] {
				part := geminiPart{FunctionResponse: &geminiFunctionResponse{
					Name:     name,
					Response: map[string]interface{}{"result": output},
					ID:       id,
				}}
				parts = append(parts, part)
			} else {
				// No matching functionCall was replayed this turn (signature
				// cache miss), so a functionResponse would be rejected.
				parts = append(parts, geminiPart{Text: "Tool result (" + name + "): " + output})
			}

		case "thinking", "redacted_thinking":
			// History thinking blocks are not replayable.

		default:
			logx.Debug("gemini convert: dropping unknown user block type %q", blockType)
		}
	}
	return parts
}

func geminiImagePart(block map[string]interface{}) (geminiPart, bool) {
	source, _ := block["source"].(map[string]interface{})
	if source == nil {
		return geminiPart{}, false
	}
	switch source["type"] {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if mediaType == "" || data == "" {
			return geminiPart{}, false
		}
		return geminiPart{InlineData: &geminiInlineData{MimeType: mediaType, Data: data}}, true
	case "url":
		u, _ := source["url"].(string)
		if u == "" {
			return geminiPart{}, false
		}
		return geminiPart{FileData: &geminiFileData{FileURI: u}}, true
	default:
		return geminiPart{}, false
	}
}

func geminiDocumentPart(block map[string]interface{}) (geminiPart, bool) {
	source, _ := block["source"].(map[string]interface{})
	if source == nil {
		return geminiPart{}, false
	}
	mediaType, _ := source["media_type"].(string)
	switch source["type"] {
	case "base64":
		data, _ := source["data"].(string)
		if data == "" {
			return geminiPart{}, false
		}
		if mediaType == "" {
			mediaType = "application/pdf"
		}
		return geminiPart{InlineData: &geminiInlineData{MimeType: mediaType, Data: data}}, true
	case "url":
		u, _ := source["url"].(string)
		if u == "" {
			return geminiPart{}, false
		}
		return geminiPart{FileData: &geminiFileData{MimeType: mediaType, FileURI: u}}, true
	default:
		return geminiPart{}, false
	}
}

// toolResultText flattens an Anthropic tool_result content into a string
// suitable for a functionResponse. Inline images cannot be carried inside a
// functionResponse, so they become "[image omitted]" markers.
func toolResultText(block map[string]interface{}) string {
	prefix := ""
	if isErr, _ := block["is_error"].(bool); isErr {
		prefix = "Error: "
	}

	switch content := block["content"].(type) {
	case string:
		return prefix + content
	case []interface{}:
		var texts []string
		omitted := 0
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
				omitted++
			}
		}
		out := prefix + strings.Join(texts, "\n")
		for i := 0; i < omitted; i++ {
			if out != "" {
				out += "\n"
			}
			out += "[image omitted]"
		}
		return out
	default:
		return prefix
	}
}

func convertGeminiTools(tools []anthropicTool) []geminiTool {
	var decls []geminiFunctionDeclaration
	for _, t := range tools {
		if t.Type != "" && t.Type != "custom" {
			logx.Debug("gemini convert: skipping tool %q (type %q)", t.Name, t.Type)
			continue
		}
		if t.Name == "" || len(t.InputSchema) == 0 || string(t.InputSchema) == "null" {
			// Server-side tools (web_search etc.) have no input_schema.
			if t.Name != "" {
				logx.Warn("gemini convert: skipping tool %q without input_schema", t.Name)
			}
			continue
		}
		decls = append(decls, geminiFunctionDeclaration{
			Name:                 t.Name,
			Description:          t.Description,
			ParametersJsonSchema: t.InputSchema,
		})
	}
	if len(decls) == 0 {
		return nil
	}
	return []geminiTool{{FunctionDeclarations: decls}}
}

func convertGeminiToolConfig(choice map[string]interface{}, hasTools bool) map[string]interface{} {
	if !hasTools || len(choice) == 0 {
		return nil
	}

	mode := ""
	var allowed []string
	switch choiceType, _ := choice["type"].(string); choiceType {
	case "auto":
		mode = "AUTO"
	case "any":
		mode = "ANY"
	case "none":
		mode = "NONE"
	case "tool":
		if name, _ := choice["name"].(string); name != "" {
			mode = "ANY"
			allowed = []string{name}
		}
	default:
		return nil
	}

	cfg := map[string]interface{}{"mode": mode}
	if len(allowed) > 0 {
		cfg["allowedFunctionNames"] = allowed
	}
	return map[string]interface{}{"functionCallingConfig": cfg}
}

func buildGeminiGenerationConfig(req *anthropicRequest, effort string) *geminiGenerationConfig {
	cfg := &geminiGenerationConfig{
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.StopSequences,
	}

	nonEmpty := cfg.Temperature != nil || cfg.TopP != nil || len(cfg.StopSequences) > 0
	if req.MaxTokens > 0 {
		cfg.MaxOutputTokens = req.MaxTokens
		nonEmpty = true
	}
	if level := geminiThinkingLevel(effort); level != "" {
		cfg.ThinkingConfig = &geminiThinkingConfig{ThinkingLevel: level}
		nonEmpty = true
	}
	if !nonEmpty {
		return nil
	}
	return cfg
}

// geminiThinkingLevel maps a backend reasoning_effort to a Gemini
// thinkingLevel. The gateway rejects "minimal" and "thinkingBudget".
func geminiThinkingLevel(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "max":
		return "high"
	case "xhigh":
		return "xhigh"
	default:
		return ""
	}
}

// textBeforeFunctionResponses moves functionResponse parts behind the other
// parts of a mixed user turn, preserving the relative order within each group.
// Pure tool-result turns are left untouched.
func textBeforeFunctionResponses(parts []geminiPart) []geminiPart {
	hasResponse, hasOther := false, false
	for _, p := range parts {
		if p.FunctionResponse != nil {
			hasResponse = true
		} else {
			hasOther = true
		}
	}
	if !hasResponse || !hasOther {
		return parts
	}
	ordered := make([]geminiPart, 0, len(parts))
	for _, p := range parts {
		if p.FunctionResponse == nil {
			ordered = append(ordered, p)
		}
	}
	for _, p := range parts {
		if p.FunctionResponse != nil {
			ordered = append(ordered, p)
		}
	}
	return ordered
}

// geminiTailRoles formats the roles of the last n contents for debug logs.
func geminiTailRoles(contents []geminiContent, n int) string {
	if n > len(contents) {
		n = len(contents)
	}
	roles := make([]string, 0, n)
	for _, c := range contents[len(contents)-n:] {
		roles = append(roles, c.Role)
	}
	return strings.Join(roles, ",")
}

// appendGeminiContent merges adjacent same-role contents: Gemini expects
// alternating turns but tolerates merged parts.
func appendGeminiContent(contents *[]geminiContent, role string, parts []geminiPart) {
	if len(parts) == 0 {
		return
	}
	if n := len(*contents); n > 0 && (*contents)[n-1].Role == role {
		(*contents)[n-1].Parts = append((*contents)[n-1].Parts, parts...)
		return
	}
	*contents = append(*contents, geminiContent{Role: role, Parts: parts})
}
