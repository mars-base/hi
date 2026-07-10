package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/mars-base/hi/pkg/logx"
	"io"
	"net/http"
	"strings"
)

// transformRequestBody remaps model names and strips thinking blocks before forwarding.
// Returns the transformed body and the original model name (for response restoration).
func (ps *ProxyState) transformRequestBody(body []byte, isModel bool, activeName string, backend Backend) ([]byte, string) {
	if !isModel || len(body) == 0 {
		return body, ""
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Not valid JSON — pass through.
		return body, ""
	}

	changed := false
	var originalModel string

	// Remap model name.
	if model, ok := parsed["model"].(string); ok {
		originalModel = model
		mapped := backend.MapModel(model)
		if mapped != model {
			parsed["model"] = mapped
			changed = true
			logx.Debug("Model remap: %s → %s", model, mapped)
		} else if claudeModel, ok := ps.reverseModelMap[model]; ok {
			// Model didn't match a Claude name (e.g. "deepseek-v4-pro" left over
			// from a previous backend). Reverse-lookup the canonical Claude model
			// name, then forward-map to the current backend.
			originalModel = claudeModel // Track the canonical Claude model name
			mapped = backend.MapModel(claudeModel)
			if mapped != model {
				parsed["model"] = mapped
				changed = true
				logx.Debug("Model remap (reverse): %s → %s → %s", model, claudeModel, mapped)
			}
		}
		logx.Debug("  final model: %s", parsed["model"])
	}

	// Strip top-level thinking config. Some backends (e.g. DeepSeek) require
	// once thinking is enabled, ALL subsequent requests must also have it —
	// but Claude Code's tool-use requests (web search, fetch) may omit it.
	// Removing the top-level config avoids the "thinking options type cannot
	// be disabled when reasoning_effort is set" error.
	//
	// However, when the conversation has no thinking blocks yet (fresh session
	// or switched from claude), we must KEEP the thinking config so deepseek
	// knows to enable thinking mode for its model (deepseek-v4-pro defaults
	// to thinking mode and rejects requests without thinking blocks).
	hasThinking := hasThinkingBlocks(parsed)
	if backend.StripTopLevelThinking() && hasThinking {
		if _, ok := parsed["thinking"]; ok {
			delete(parsed, "thinking")
			changed = true
			logx.Debug("Stripped top-level thinking config (backend: %s)", activeName)
		}
	}

	// For backends that keep thinking blocks (deepseek etc.), inject thinking config when missing. When switching from another backend leaves the conversation with thinking blocks in some but not all assistant messages, add empty thinking blocks to messages that lack them — deepseek-v4-pro requires ALL assistant messages to have thinking when enabled.
	if !backend.NeedsThinkingStrip() {
		mixed := isMixedThinking(parsed)
		if mixed {
			// deepseek-v4-pro requires ALL assistant messages to have thinking
			// blocks when thinking mode is on. Some messages from claude lack
			// them, causing 400. Add a minimal empty thinking block to any
			// assistant message that doesn't have one.
			if messages, ok := parsed["messages"].([]interface{}); ok {
				for _, msg := range messages {
					if msgMap, ok := msg.(map[string]interface{}); ok {
						if role, _ := msgMap["role"].(string); role != "assistant" {
							continue
						}
						if content, ok := msgMap["content"].([]interface{}); ok {
							hasThinking := false
							for _, block := range content {
								if blockMap, ok := block.(map[string]interface{}); ok {
									t := blockMap["type"].(string)
									if t == "thinking" || t == "redacted_thinking" {
										hasThinking = true
										break
									}
								}
							}
							if !hasThinking {
								emptyThink := map[string]interface{}{"type": "thinking", "thinking": ""}
								msgMap["content"] = append([]interface{}{emptyThink}, content...)
								changed = true
							}
						}
					}
				}
			}
			if _, ok := parsed["thinking"]; !ok {
				parsed["thinking"] = map[string]interface{}{"type": "enabled"}
				changed = true
			}
			logx.Debug("Mixed thinking state detected — normalized thinking blocks, set thinking=enabled (backend: %s)", activeName)
		} else {
			think, ok := parsed["thinking"].(map[string]interface{})
			if !ok {
				think = map[string]interface{}{}
				parsed["thinking"] = think
				changed = true
			}
			if t, _ := think["type"].(string); t != "enabled" {
				think["type"] = "enabled"
				changed = true
				logx.Debug("Injected thinking config (backend: %s)", activeName)
			}
		}
	}

	// Strip context_management for non-Anthropic backends.
	// This is an Anthropic-specific field (context_management.edits with
	// clear_thinking_20251015 signals that thinking blocks were cleared
	// from context). Third-party backends (deepseek, qwen, kimi, minimax,
	// glm, mimo, etc.) don't understand it and return 400.
	if !backend.IsAnthropicAPI() {
		if _, ok := parsed["context_management"]; ok {
			delete(parsed, "context_management")
			changed = true
			logx.Debug("Stripped context_management (backend: %s)", activeName)
		}
	}

	// Strip non-standard content blocks from messages (citations, thinking).
	// - citations: produced by deepseek, rejected by claude/Bedrock
	// - thinking: deepseek requires once enabled; safest to strip when switching
	if messages, ok := parsed["messages"].([]interface{}); ok {
		stripTypes := map[string]bool{"citations": true}
		if backend.NeedsThinkingStrip() {
			stripTypes["thinking"] = true
			stripTypes["redacted_thinking"] = true
		}
		stripped := 0
		for _, msg := range messages {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				if content, ok := msgMap["content"].([]interface{}); ok {
					filtered := make([]interface{}, 0, len(content))
					for _, block := range content {
						if blockMap, ok := block.(map[string]interface{}); ok {
							if stripTypes[blockMap["type"].(string)] {
								stripped++
								changed = true
								continue
							}
							// Remove "citations" field nested inside content blocks
							// (e.g. tool_use.citations). DeepSeek adds these; Bedrock rejects them.
							if _, has := blockMap["citations"]; has {
								delete(blockMap, "citations")
								stripped++
								changed = true
							}
						}
						filtered = append(filtered, block)
					}
					msgMap["content"] = filtered
					// If stripping left the content empty, insert a placeholder
					// text block. Upstream APIs reject empty content arrays.
					if len(filtered) == 0 {
						msgMap["content"] = []interface{}{
							map[string]interface{}{"type": "text", "text": "..."},
						}
					}
				}
			}
		}
		if stripped > 0 {
			logx.Debug("Stripped %d block(s) from messages (backend: %s)", stripped, activeName)
		}
	}

	// Inject output_config.effort for deepseek backends if configured.
	// Only when thinking is enabled — otherwise deepseek rejects it.
	if eff := backend.ReasoningEffort(); eff != "" {
		thinkEnabled := false
		if t, ok := parsed["thinking"].(map[string]interface{}); ok {
			if tp, _ := t["type"].(string); tp == "enabled" {
				thinkEnabled = true
			}
		}
		if thinkEnabled {
			parsed["output_config"] = map[string]interface{}{"effort": eff}
			changed = true
			logx.Debug("Injected output_config.effort=%s (backend: %s)", eff, activeName)
		} else {
			// CC may have sent its own output_config — strip it to avoid
			// deepseek interpreting it as thinking-enabled.
			if _, ok := parsed["output_config"]; ok {
				delete(parsed, "output_config")
				changed = true
				logx.Debug("Stripped output_config (thinking not enabled, backend: %s)", activeName)
			} else {
				logx.Debug("Skipped output_config.effort: thinking not enabled")
			}
		}
	}

	if !changed {
		return body, originalModel
	}

	newBody, err := json.Marshal(parsed)
	if err != nil {
		return body, originalModel
	}

	// Debug: log thinking-related fields for deepseek backends.
	if !backend.NeedsThinkingStrip() {
		thinkInfo := "<missing>"
		if t, ok := parsed["thinking"]; ok {
			thinkInfo = fmt.Sprintf("%v", t)
		}
		ctxInfo := "<missing>"
		if c, ok := parsed["context_management"]; ok {
			ctxInfo = fmt.Sprintf("%v", c)
		}
		outInfo := "<missing>"
		if o, ok := parsed["output_config"]; ok {
			outInfo = fmt.Sprintf("%v", o)
		}
		logx.Debug("Final body fields -> thinking=%s context_management=%s output_config=%s", thinkInfo, ctxInfo, outInfo)
	}

	return newBody, originalModel
}

// hasThinkingBlocks returns true if any assistant message in the
// conversation contains a thinking block.
func hasThinkingBlocks(parsed map[string]interface{}) bool {
	return countAssistantWithThinking(parsed) > 0
}

// isMixedThinking returns true if some but not all assistant messages
// in the conversation contain thinking blocks — indicating a switch
// between backends with different thinking modes.
func isMixedThinking(parsed map[string]interface{}) bool {
	messages, ok := parsed["messages"].([]interface{})
	if !ok {
		return false
	}
	total := 0
	withThinking := 0
	for _, msg := range messages {
		msgMap, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msgMap["role"].(string); role != "assistant" {
			continue
		}
		total++
		content, ok := msgMap["content"].([]interface{})
		if !ok {
			continue
		}
		for _, block := range content {
			if blockMap, ok := block.(map[string]interface{}); ok {
				t, _ := blockMap["type"].(string)
				if t == "thinking" || t == "redacted_thinking" {
					withThinking++
					break
				}
			}
		}
	}
	return withThinking > 0 && withThinking < total
}

// countAssistantWithThinking returns the number of assistant messages
// that contain thinking blocks.
func countAssistantWithThinking(parsed map[string]interface{}) int {
	messages, ok := parsed["messages"].([]interface{})
	if !ok {
		return 0
	}
	count := 0
	for _, msg := range messages {
		msgMap, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msgMap["role"].(string); role != "assistant" {
			continue
		}
		content, ok := msgMap["content"].([]interface{})
		if !ok {
			continue
		}
		for _, block := range content {
			if blockMap, ok := block.(map[string]interface{}); ok {
				t, _ := blockMap["type"].(string)
				if t == "thinking" || t == "redacted_thinking" {
					count++
					break
				}
			}
		}
	}
	return count
}

// processResponse handles the upstream response — normalizing SSE, tracking costs,
// and writing back to the client. originalModel is the model name from the request
// that should be restored in the response so Claude Code sees its own naming.
func (ps *ProxyState) processResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, backend Backend, originalModel string) {
	contentType := resp.Header.Get("Content-Type")

	isSSE := strings.Contains(contentType, "text/event-stream")
	isJSON := strings.Contains(contentType, "application/json")

	backendName := backend.Name()

	logx.Debug("processResponse: content-type=%s sse=%v json=%v", contentType, isSSE, isJSON)

	if isSSE {
		ps.processSSEResponse(w, resp, backendName, originalModel)
	} else if isJSON {
		ps.processJSONResponse(w, resp, backendName, originalModel)
	} else {
		ps.processPassthrough(w, resp)
	}
}

// processSSEResponse streams an SSE response with usage normalization.
// originalModel is restored in message_start events so Claude Code sees its own model name.
func (ps *ProxyState) processSSEResponse(w http.ResponseWriter, resp *http.Response, backendName string, originalModel string) {
	// Capture the first part of the body when the upstream returns an error.
	// 4xx/5xx SSE responses contain an error message that we want to log for debugging.
	isError := resp.StatusCode >= 400

	// Copy headers.
	ps.copyHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Can't flush — just copy.
		io.Copy(w, resp.Body)
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max scan token.

	var inputTokens, outputTokens int64
	var eventBuf strings.Builder
	var errorBuf strings.Builder // Collects first events when isError=true

	for scanner.Scan() {
		line := scanner.Text()
		eventBuf.WriteString(line)
		eventBuf.WriteString("\n")

		// SSE events are separated by blank lines.
		if line == "" {
			event := eventBuf.String()
			eventBuf.Reset()

			if isError && errorBuf.Len() < 4096 {
				errorBuf.WriteString(event)
			}

			fixedEvent := ps.normalizeSSEEvent(event, &inputTokens, &outputTokens, originalModel)
			w.Write([]byte(fixedEvent))
			flusher.Flush()
		}
	}

	// Flush remaining buffer.
	if eventBuf.Len() > 0 {
		event := eventBuf.String()
		if isError && errorBuf.Len() < 4096 {
			errorBuf.WriteString(event)
		}
		fixedEvent := ps.normalizeSSEEvent(event, &inputTokens, &outputTokens, originalModel)
		w.Write([]byte(fixedEvent))
		flusher.Flush()
	}

	// Log the upstream error body so we can diagnose 400/500 errors.
	if isError && errorBuf.Len() > 0 {
		logx.Warn("#%d upstream SSE error (status=%d backend=%s): %s",
			ps.RequestCount(), resp.StatusCode, backendName, strings.TrimSpace(errorBuf.String()))
	}

	if err := scanner.Err(); err != nil {
		logx.Warn("SSE stream error: %v", err)
	}

	// Record usage if we got any tokens.
	if inputTokens > 0 || outputTokens > 0 {
		ps.costTracker.Record(backendName, inputTokens, outputTokens)
		logx.Info("#%d tokens: in=%d out=%d", ps.RequestCount(), inputTokens, outputTokens)
	}
}

// normalizeSSEEvent ensures usage fields exist in message_start and message_delta events.
// originalModel is restored in message_start events so Claude Code sees its own model name.
func (ps *ProxyState) normalizeSSEEvent(event string, inputTokens *int64, outputTokens *int64, originalModel string) string {
	// Find the data: line.
	dataPrefix := "data: "
	idx := strings.Index(event, dataPrefix)
	if idx < 0 {
		return event
	}

	dataStart := idx + len(dataPrefix)
	dataEnd := strings.IndexByte(event[dataStart:], '\n')
	if dataEnd < 0 {
		dataEnd = len(event) - dataStart
	}

	dataStr := event[dataStart : dataStart+dataEnd]
	if strings.TrimSpace(dataStr) == "[DONE]" {
		return event
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(dataStr), &parsed); err != nil {
		return event
	}

	changed := false

	eventType, _ := parsed["type"].(string)

	switch eventType {
	case "message_start":
		if msg, ok := parsed["message"].(map[string]interface{}); ok {
			// Restore the original model name so Claude Code sees its own naming.
			if originalModel != "" {
				if backendModel, _ := msg["model"].(string); backendModel != "" && backendModel != originalModel {
					msg["model"] = originalModel
					changed = true
					logx.Debug("Response model restored: %s → %s", backendModel, originalModel)
				}
			}
			if _, hasUsage := msg["usage"]; !hasUsage {
				msg["usage"] = map[string]interface{}{
					"input_tokens":  0,
					"output_tokens": 0,
				}
				changed = true
			}
			if usage, ok := msg["usage"].(map[string]interface{}); ok {
				if it, ok := usage["input_tokens"].(float64); ok {
					*inputTokens = int64(it)
				}
			}
		}

	case "message_delta":
		if _, hasUsage := parsed["usage"]; !hasUsage {
			parsed["usage"] = map[string]interface{}{
				"output_tokens": 0,
			}
			changed = true
		}
		if usage, ok := parsed["usage"].(map[string]interface{}); ok {
			if ot, ok := usage["output_tokens"].(float64); ok {
				*outputTokens = int64(ot)
			}
		}

	case "message_stop":
		// An SSE "message_stop" event has a "usage" field at the **event** level
		// that may contain the final token count. If present, prefer it.
		if usage, ok := parsed["usage"].(map[string]interface{}); ok {
			if it, ok := usage["input_tokens"].(float64); ok && int64(it) > *inputTokens {
				*inputTokens = int64(it)
			}
			if ot, ok := usage["output_tokens"].(float64); ok && int64(ot) > *outputTokens {
				*outputTokens = int64(ot)
			}
		}
	}

	if !changed {
		return event
	}

	newData, err := json.Marshal(parsed)
	if err != nil {
		return event
	}

	// Reconstruct the event with the fixed data line.
	return strings.Replace(event, dataStr, string(newData), 1)
}

// processJSONResponse handles non-streaming JSON responses.
// originalModel is restored so Claude Code sees its own model name.
func (ps *ProxyState) processJSONResponse(w http.ResponseWriter, resp *http.Response, backendName string, originalModel string) {
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		ps.copyHeaders(w, resp)
		w.WriteHeader(resp.StatusCode)
		return
	}

	// Normalize: ensure usage block exists.
	var parsed map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &parsed); err == nil {
		if parsed["type"] == "message" {
			// Restore the original model name so Claude Code sees its own naming.
			if originalModel != "" {
				if backendModel, _ := parsed["model"].(string); backendModel != "" && backendModel != originalModel {
					parsed["model"] = originalModel
					logx.Debug("Response model restored: %s → %s", backendModel, originalModel)
				}
			}
			if _, ok := parsed["usage"]; !ok {
				parsed["usage"] = map[string]interface{}{
					"input_tokens":  0,
					"output_tokens": 0,
				}
			}
		}
		// Track usage.
		if usage, ok := parsed["usage"].(map[string]interface{}); ok {
			var inputTokens, outputTokens int64
			if it, ok := usage["input_tokens"].(float64); ok {
				inputTokens = int64(it)
			}
			if ot, ok := usage["output_tokens"].(float64); ok {
				outputTokens = int64(ot)
				ps.costTracker.Record(backendName, inputTokens, outputTokens)
				logx.Info("#%d tokens: in=%d out=%d", ps.RequestCount(), inputTokens, outputTokens)
			}
		}

		newBody, err := json.Marshal(parsed)
		if err == nil {
			bodyBytes = newBody
		}
	}

	ps.copyHeaders(w, resp)
	w.Header().Set("Content-Length", itoa(len(bodyBytes)))
	w.WriteHeader(resp.StatusCode)
	w.Write(bodyBytes)
}

// processPassthrough copies the response without modification.
func (ps *ProxyState) processPassthrough(w http.ResponseWriter, resp *http.Response) {
	ps.copyHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// copyHeaders copies response headers from upstream to downstream.
func (ps *ProxyState) copyHeaders(w http.ResponseWriter, resp *http.Response) {
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	stripHopByHop(w.Header())
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(byte('0'+n%10)) + s
		n /= 10
	}
	return s
}
