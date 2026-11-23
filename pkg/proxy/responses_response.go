package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mars-base/hi/pkg/logx"
)

type responsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

type responsesContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesItem struct {
	Type      string                 `json:"type"`
	ID        string                 `json:"id"`
	CallID    string                 `json:"call_id"`
	Name      string                 `json:"name"`
	Arguments string                 `json:"arguments"`
	Content   []responsesContentPart `json:"content"`
}

type responsesObject struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []responsesItem `json:"output"`
	Usage  responsesUsage  `json:"usage"`
}

// convertResponsesJSON converts a non-streaming Responses object into an
// Anthropic message. It returns the converted body and upstream usage.
func convertResponsesJSON(body []byte, originalModel string) ([]byte, responsesUsage, error) {
	var resp responsesObject
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, responsesUsage{}, fmt.Errorf("invalid Responses payload: %w", err)
	}
	msg := responsesToAnthropicMessage(resp, originalModel)
	out, err := json.Marshal(msg)
	if err != nil {
		return nil, resp.Usage, err
	}
	return out, resp.Usage, nil
}

func responsesToAnthropicMessage(resp responsesObject, originalModel string) map[string]interface{} {
	blocks := make([]interface{}, 0, len(resp.Output))
	hasTool := false
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Text == "" {
					continue
				}
				if part.Type == "output_text" || part.Type == "refusal" {
					blocks = append(blocks, map[string]interface{}{"type": "text", "text": part.Text})
				}
			}
		case "function_call":
			blocks = append(blocks, toolUseBlock(item))
			hasTool = true
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]interface{}{"type": "text", "text": ""})
	}

	model := originalModel
	if model == "" {
		model = resp.Model
	}

	incompleteReason := ""
	if resp.IncompleteDetails != nil {
		incompleteReason = resp.IncompleteDetails.Reason
	}

	return map[string]interface{}{
		"id":            anthropicMessageID(resp.ID),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   responsesStopReason(resp.Status, incompleteReason, hasTool),
		"stop_sequence": nil,
		"usage":         anthropicUsage(resp.Usage),
	}
}

func toolUseBlock(item responsesItem) map[string]interface{} {
	id := item.CallID
	if id == "" {
		id = item.ID
	}
	input := interface{}(map[string]interface{}{})
	if item.Arguments != "" {
		var v interface{}
		if err := json.Unmarshal([]byte(item.Arguments), &v); err != nil {
			input = map[string]interface{}{"_raw_arguments": item.Arguments}
		} else {
			input = v
		}
	}
	return map[string]interface{}{
		"type":  "tool_use",
		"id":    id,
		"name":  item.Name,
		"input": input,
	}
}

func anthropicMessageID(respID string) string {
	id := strings.TrimPrefix(respID, "resp_")
	if id == "" {
		return "msg_openai_response"
	}
	return "msg_" + id
}

func anthropicUsage(u responsesUsage) map[string]interface{} {
	return map[string]interface{}{
		"input_tokens":                uncachedInputTokens(u),
		"output_tokens":               u.OutputTokens,
		"cache_read_input_tokens":     u.InputTokensDetails.CachedTokens,
		"cache_creation_input_tokens": u.InputTokensDetails.CacheWriteTokens,
	}
}

// uncachedInputTokens converts Responses input_tokens (which include cache
// hits) into Anthropic's input_tokens (which exclude them).
func uncachedInputTokens(u responsesUsage) int64 {
	n := u.InputTokens - u.InputTokensDetails.CachedTokens - u.InputTokensDetails.CacheWriteTokens
	if n < 0 {
		return 0
	}
	return n
}

func responsesStopReason(status, incompleteReason string, hasTool bool) string {
	// A truncated response must not be reported as tool_use: its arguments
	// may be incomplete JSON.
	if status == "incomplete" && incompleteReason == "max_output_tokens" {
		return "max_tokens"
	}
	if hasTool {
		return "tool_use"
	}
	return "end_turn"
}

// ==============================
// Error mapping
// ==============================

func anthropicErrorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// upstreamErrorMessage extracts a human-readable message from a Responses
// error body: {"error":{"message","type","param","code"}}.
func upstreamErrorMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		msg := env.Error.Message
		if env.Error.Code != "" {
			msg += " (code: " + env.Error.Code + ")"
		}
		return msg
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 500 {
		s = s[:500]
	}
	if s == "" {
		s = "upstream returned an error"
	}
	return s
}

func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	if status < 400 {
		status = http.StatusBadGateway
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type":  "error",
		"error": map[string]interface{}{"type": anthropicErrorType(status), "message": message},
	})
}

// ==============================
// SSE helpers
// ==============================

func sseWrite(w http.ResponseWriter, flusher http.Flusher, eventType string, payload map[string]interface{}) {
	payload["type"] = eventType
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
	if flusher != nil {
		flusher.Flush()
	}
}

// scanSSEEvents reads an SSE stream and invokes handle for each event.
// Returning true from handle stops scanning.
func scanSSEEvents(r io.Reader, handle func(name string, data []byte) bool) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var name string
	var data []byte

	emit := func() bool {
		if len(data) == 0 {
			name = ""
			return false
		}
		stop := handle(name, data)
		name = ""
		data = nil
		return stop
	}

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if emit() {
				return nil
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			v := strings.TrimPrefix(line, "data:")
			v = strings.TrimPrefix(v, " ")
			data = append(data, v...)
			data = append(data, '\n')
		}
	}
	if len(data) > 0 {
		emit()
	}
	return scanner.Err()
}

// collectResponsesStream drains an SSE stream and returns the final response
// object (for clients that requested a non-streaming reply).
func collectResponsesStream(r io.Reader) (responsesObject, error) {
	var final responsesObject
	var found bool
	var streamErr error

	err := scanSSEEvents(r, func(name string, data []byte) bool {
		if strings.TrimSpace(string(data)) == "[DONE]" {
			return false
		}
		var ev map[string]interface{}
		if err := json.Unmarshal(data, &ev); err != nil {
			return false
		}
		eventType := jsonString(ev["type"])
		if eventType == "" {
			eventType = name
		}
		switch eventType {
		case "response.completed", "response.incomplete":
			resp := jsonMap(ev["response"])
			raw, err := json.Marshal(resp)
			if err == nil && json.Unmarshal(raw, &final) == nil {
				found = true
			}
			return true
		case "response.failed", "error":
			streamErr = fmt.Errorf("upstream stream failed: %s", eventErrorMessage(ev))
			return true
		}
		return false
	})
	if err != nil {
		return final, err
	}
	if streamErr != nil {
		return final, streamErr
	}
	if !found {
		return final, fmt.Errorf("upstream stream ended without a completed response")
	}
	return final, nil
}

func eventErrorMessage(ev map[string]interface{}) string {
	if msg := jsonString(ev["message"]); msg != "" {
		return msg
	}
	if resp := jsonMap(ev["response"]); resp != nil {
		if errObj := jsonMap(resp["error"]); errObj != nil {
			if msg := jsonString(errObj["message"]); msg != "" {
				return msg
			}
		}
	}
	if errObj := jsonMap(ev["error"]); errObj != nil {
		if msg := jsonString(errObj["message"]); msg != "" {
			return msg
		}
	}
	return "upstream response failed"
}

// ==============================
// Streaming conversion
// ==============================

type responsesBlock struct {
	index    int
	kind     string
	itemID   string
	open     bool
	sawDelta bool
}

type responsesStreamState struct {
	ps             *ProxyState
	w              http.ResponseWriter
	flusher        http.Flusher
	backendName    string
	originalModel  string
	estimatedInput int64

	started          bool
	finished         bool
	messageID        string
	model            string
	nextIndex        int
	current          *responsesBlock
	blocks           map[string]*responsesBlock
	pendingToolArgs  map[string]string
	usage            responsesUsage
	status           string
	incompleteReason string
	sawTool          bool
}

// streamResponsesSSE converts an upstream Responses SSE stream into an
// Anthropic Messages SSE stream.
func (ps *ProxyState) streamResponsesSSE(w http.ResponseWriter, resp *http.Response, backendName, originalModel string, estimatedInput int64) {
	s := &responsesStreamState{
		ps:              ps,
		w:               w,
		backendName:     backendName,
		originalModel:   originalModel,
		estimatedInput:  estimatedInput,
		blocks:          make(map[string]*responsesBlock),
		pendingToolArgs: make(map[string]string),
	}
	if flusher, ok := w.(http.Flusher); ok {
		s.flusher = flusher
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	err := scanSSEEvents(resp.Body, func(name string, data []byte) bool {
		s.handleEvent(name, data)
		return s.finished
	})
	if err != nil {
		logx.Warn("responses stream read error (backend=%s): %v", backendName, err)
	}
	if !s.finished {
		logx.Warn("responses stream ended without a terminal event (backend=%s)", backendName)
		s.finish()
	}
}

func (s *responsesStreamState) send(eventType string, payload map[string]interface{}) {
	if s.finished {
		return
	}
	sseWrite(s.w, s.flusher, eventType, payload)
}

func (s *responsesStreamState) capture(resp map[string]interface{}) {
	if id := jsonString(resp["id"]); id != "" {
		s.messageID = anthropicMessageID(id)
	}
	if model := jsonString(resp["model"]); model != "" {
		s.model = model
	}
	if u, ok := resp["usage"]; ok && u != nil {
		s.usage = usageFromMap(u)
	}
}

func (s *responsesStreamState) ensureStarted() {
	if s.started {
		return
	}
	if s.messageID == "" {
		s.messageID = "msg_openai_response"
	}
	model := s.originalModel
	if model == "" {
		model = s.model
	}
	s.send("message_start", map[string]interface{}{
		"message": map[string]interface{}{
			"id":            s.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  s.estimatedInput,
				"output_tokens": 0,
			},
		},
	})
	s.started = true
}

func (s *responsesStreamState) closeCurrent() {
	if s.current != nil && s.current.open {
		s.send("content_block_stop", map[string]interface{}{"index": s.current.index})
		s.current.open = false
	}
	s.current = nil
}

func (s *responsesStreamState) closeBlock(b *responsesBlock) {
	if b == nil || !b.open {
		return
	}
	s.send("content_block_stop", map[string]interface{}{"index": b.index})
	b.open = false
	if s.current == b {
		s.current = nil
	}
}

func (s *responsesStreamState) openBlock(itemID, kind, callID, name string) *responsesBlock {
	if itemID != "" {
		if b := s.blocks[itemID]; b != nil {
			return b
		}
	} else if s.current != nil && s.current.kind == kind {
		return s.current
	}

	s.closeCurrent()
	s.ensureStarted()

	b := &responsesBlock{index: s.nextIndex, kind: kind, itemID: itemID, open: true}
	s.nextIndex++

	var content map[string]interface{}
	if kind == "tool_use" {
		content = map[string]interface{}{
			"type":  "tool_use",
			"id":    callID,
			"name":  name,
			"input": map[string]interface{}{},
		}
	} else {
		content = map[string]interface{}{"type": "text", "text": ""}
	}
	s.send("content_block_start", map[string]interface{}{
		"index":         b.index,
		"content_block": content,
	})
	if itemID != "" {
		s.blocks[itemID] = b
	}
	s.current = b
	return b
}

func (s *responsesStreamState) ensureTextBlock(itemID string) *responsesBlock {
	if b := s.blocks[itemID]; b != nil {
		return b
	}
	return s.openBlock(itemID, "text", "", "")
}

func (s *responsesStreamState) openToolBlock(item map[string]interface{}) *responsesBlock {
	id := itemIDOf(item)
	if b := s.blocks[id]; b != nil {
		return b
	}
	callID := jsonString(item["call_id"])
	if callID == "" {
		callID = id
	}
	if callID == "" {
		callID = fmt.Sprintf("call_%d", s.nextIndex)
	}
	s.sawTool = true
	return s.openBlock(id, "tool_use", callID, jsonString(item["name"]))
}

func (s *responsesStreamState) textDelta(b *responsesBlock, text string) {
	if b == nil || text == "" {
		return
	}
	s.send("content_block_delta", map[string]interface{}{
		"index": b.index,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	})
	b.sawDelta = true
}

func (s *responsesStreamState) argsDelta(b *responsesBlock, args string) {
	if b == nil || args == "" {
		return
	}
	s.send("content_block_delta", map[string]interface{}{
		"index": b.index,
		"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": args},
	})
	b.sawDelta = true
}

func (s *responsesStreamState) handleEvent(name string, data []byte) {
	if strings.TrimSpace(string(data)) == "[DONE]" {
		return
	}
	var ev map[string]interface{}
	if err := json.Unmarshal(data, &ev); err != nil {
		logx.Debug("responses stream: bad JSON event (%v): %.200s", err, string(data))
		return
	}
	eventType := jsonString(ev["type"])
	if eventType == "" {
		eventType = name
	}

	switch eventType {
	case "response.created", "response.in_progress":
		resp := jsonMap(ev["response"])
		s.capture(resp)
		s.ensureStarted()

	case "response.output_item.added":
		item := jsonMap(ev["item"])
		switch jsonString(item["type"]) {
		case "message":
			if id := jsonString(item["id"]); id != "" {
				s.openBlock(id, "text", "", "")
			}
		case "function_call":
			s.openToolBlock(item)
		case "reasoning":
			// No reasoning summary available; nothing to emit.
		default:
			logx.Debug("responses stream: skipping output item type %q", jsonString(item["type"]))
		}

	case "response.content_part.added":
		part := jsonMap(ev["part"])
		if jsonString(part["type"]) == "output_text" {
			s.ensureTextBlock(jsonString(ev["item_id"]))
		}

	case "response.output_text.delta":
		s.textDelta(s.ensureTextBlock(jsonString(ev["item_id"])), jsonString(ev["delta"]))

	case "response.output_text.done":
		if b := s.blocks[jsonString(ev["item_id"])]; b != nil && !b.sawDelta {
			s.textDelta(b, jsonString(ev["text"]))
		}

	case "response.function_call_arguments.delta":
		itemID := jsonString(ev["item_id"])
		if b := s.blocks[itemID]; b != nil {
			s.argsDelta(b, jsonString(ev["delta"]))
		} else {
			s.pendingToolArgs[itemID] += jsonString(ev["delta"])
		}

	case "response.function_call_arguments.done":
		itemID := jsonString(ev["item_id"])
		args := jsonString(ev["arguments"])
		if b := s.blocks[itemID]; b != nil {
			if !b.sawDelta {
				s.argsDelta(b, args)
			}
		} else if args != "" {
			s.pendingToolArgs[itemID] = args
		}

	case "response.content_part.done":
		if b := s.blocks[jsonString(ev["item_id"])]; b != nil && !b.sawDelta {
			part := jsonMap(ev["part"])
			if jsonString(part["type"]) == "output_text" {
				s.textDelta(b, jsonString(part["text"]))
			}
		}

	case "response.output_item.done":
		s.handleItemDone(jsonMap(ev["item"]))

	case "response.completed", "response.incomplete":
		resp := jsonMap(ev["response"])
		s.capture(resp)
		s.usage = usageFromMap(resp["usage"])
		s.status = jsonString(resp["status"])
		s.incompleteReason = jsonString(jsonMap(resp["incomplete_details"])["reason"])
		s.emitBlocksFromOutput(jsonList(resp["output"]))
		s.finish()

	case "response.failed":
		resp := jsonMap(ev["response"])
		s.capture(resp)
		s.fail(eventErrorMessage(ev))

	case "error":
		s.fail(eventErrorMessage(ev))

	default:
		logx.Debug("responses stream: ignoring event %q", eventType)
	}
}

func (s *responsesStreamState) handleItemDone(item map[string]interface{}) {
	switch jsonString(item["type"]) {
	case "message":
		itemID := jsonString(item["id"])
		b := s.blocks[itemID]
		if b == nil {
			text := outputTextFromItem(item)
			if text == "" {
				return
			}
			b = s.openBlock(itemID, "text", "", "")
			s.textDelta(b, text)
		} else if !b.sawDelta {
			s.textDelta(b, outputTextFromItem(item))
		}
		s.closeBlock(b)

	case "function_call":
		itemID := itemIDOf(item)
		b := s.blocks[itemID]
		if b == nil {
			b = s.openToolBlock(item)
			if pending := s.pendingToolArgs[itemID]; pending != "" {
				s.argsDelta(b, pending)
			}
			delete(s.pendingToolArgs, itemID)
		}
		if !b.sawDelta {
			s.argsDelta(b, jsonString(item["arguments"]))
		}
		s.closeBlock(b)
	}
}

// emitBlocksFromOutput synthesizes blocks from the final response output for
// gateways that skip per-item delta events. Existing blocks are left alone.
func (s *responsesStreamState) emitBlocksFromOutput(output []interface{}) {
	for _, raw := range output {
		item := jsonMap(raw)
		switch jsonString(item["type"]) {
		case "message":
			id := jsonString(item["id"])
			if s.blocks[id] != nil {
				continue
			}
			text := outputTextFromItem(item)
			if text == "" {
				continue
			}
			b := s.openBlock(id, "text", "", "")
			s.textDelta(b, text)
			s.closeBlock(b)

		case "function_call":
			id := itemIDOf(item)
			if s.blocks[id] != nil {
				continue
			}
			b := s.openToolBlock(item)
			s.argsDelta(b, jsonString(item["arguments"]))
			s.closeBlock(b)
		}
	}
}

func (s *responsesStreamState) finish() {
	if s.finished {
		return
	}
	s.closeCurrent()
	if s.nextIndex == 0 {
		// No content at all (e.g. reasoning-only truncation): emit an empty
		// text block so the message has a valid content array.
		b := s.openBlock("", "text", "", "")
		s.closeBlock(b)
	}
	s.send("message_delta", map[string]interface{}{
		"delta": map[string]interface{}{
			"stop_reason":   responsesStopReason(s.status, s.incompleteReason, s.sawTool),
			"stop_sequence": nil,
		},
		"usage": anthropicUsage(s.usage),
	})
	s.send("message_stop", map[string]interface{}{})
	s.finished = true

	input := uncachedInputTokens(s.usage)
	if input > 0 || s.usage.OutputTokens > 0 {
		s.ps.costTracker.Record(s.backendName, input, s.usage.OutputTokens)
		logx.Info("#%d tokens: in=%d out=%d cached=%d (backend=%s)",
			s.ps.RequestCount(), input, s.usage.OutputTokens, s.usage.InputTokensDetails.CachedTokens, s.backendName)
	}
}

func (s *responsesStreamState) fail(message string) {
	if s.finished {
		return
	}
	s.ensureStarted()
	s.closeCurrent()
	sseWrite(s.w, s.flusher, "error", map[string]interface{}{
		"error": map[string]interface{}{"type": "api_error", "message": message},
	})
	s.finished = true
	logx.Warn("responses stream failed (backend=%s): %s", s.backendName, message)
}

// emitAnthropicMessageSSE writes a complete Anthropic message as SSE events.
// Used when the client asked for streaming but the upstream replied with JSON.
func emitAnthropicMessageSSE(w http.ResponseWriter, msg map[string]interface{}) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	var flusher http.Flusher
	if f, ok := w.(http.Flusher); ok {
		flusher = f
	}

	start := make(map[string]interface{}, len(msg))
	for k, v := range msg {
		start[k] = v
	}
	start["content"] = []interface{}{}
	start["stop_reason"] = nil
	start["stop_sequence"] = nil
	sseWrite(w, flusher, "message_start", map[string]interface{}{"message": start})

	blocks, _ := msg["content"].([]interface{})
	for i, raw := range blocks {
		block := jsonMap(raw)
		// The start event must carry an empty text/input; the full payload
		// follows through deltas. Sending it twice corrupts the block.
		startBlock := make(map[string]interface{}, len(block))
		for k, v := range block {
			startBlock[k] = v
		}
		switch jsonString(block["type"]) {
		case "text":
			startBlock["text"] = ""
		case "tool_use":
			startBlock["input"] = map[string]interface{}{}
		}
		sseWrite(w, flusher, "content_block_start", map[string]interface{}{
			"index":         i,
			"content_block": startBlock,
		})
		switch jsonString(block["type"]) {
		case "text":
			if text := jsonString(block["text"]); text != "" {
				sseWrite(w, flusher, "content_block_delta", map[string]interface{}{
					"index": i,
					"delta": map[string]interface{}{"type": "text_delta", "text": text},
				})
			}
		case "tool_use":
			if args, err := json.Marshal(block["input"]); err == nil {
				sseWrite(w, flusher, "content_block_delta", map[string]interface{}{
					"index": i,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(args)},
				})
			}
		}
		sseWrite(w, flusher, "content_block_stop", map[string]interface{}{"index": i})
	}

	sseWrite(w, flusher, "message_delta", map[string]interface{}{
		"delta": map[string]interface{}{
			"stop_reason":   msg["stop_reason"],
			"stop_sequence": nil,
		},
		"usage": msg["usage"],
	})
	sseWrite(w, flusher, "message_stop", map[string]interface{}{})
}

// ==============================
// JSON map helpers
// ==============================

func jsonMap(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}

func jsonString(v interface{}) string {
	s, _ := v.(string)
	return s
}

func jsonList(v interface{}) []interface{} {
	l, _ := v.([]interface{})
	return l
}

func itemIDOf(item map[string]interface{}) string {
	id := jsonString(item["id"])
	if id == "" {
		id = jsonString(item["call_id"])
	}
	return id
}

func outputTextFromItem(item map[string]interface{}) string {
	var sb strings.Builder
	for _, raw := range jsonList(item["content"]) {
		part := jsonMap(raw)
		partType := jsonString(part["type"])
		if partType == "output_text" || partType == "refusal" {
			sb.WriteString(jsonString(part["text"]))
		}
	}
	return sb.String()
}

func usageFromMap(v interface{}) responsesUsage {
	var u responsesUsage
	raw, err := json.Marshal(v)
	if err != nil {
		return u
	}
	json.Unmarshal(raw, &u)
	return u
}
