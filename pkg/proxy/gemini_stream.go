package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mars-base/hi/pkg/logx"
)

// geminiPingInterval is how often a ping event is sent while waiting for
// upstream data. It is a variable so tests can shorten it.
var geminiPingInterval = 10 * time.Second

type geminiStreamTool struct {
	id   string
	name string
	args json.RawMessage
}

type geminiStreamEvent struct {
	chunk *geminiChunk
	err   error
}

type geminiStreamState struct {
	ps             *ProxyState
	w              http.ResponseWriter
	flusher        http.Flusher
	backendName    string
	backend        *geminiBackend
	originalModel  string
	estimatedInput int64
	messageID      string

	started    bool
	finished   bool
	nextIndex  int
	textIndex  int
	textOpen   bool
	sawText    bool
	tools      []geminiStreamTool
	usage      geminiUsage
	finish     string
	blocked    string
	thoughtSig string
}

// streamGeminiSSE converts an upstream Gemini SSE stream into an Anthropic
// Messages SSE stream.
//
// message_start is sent before the first upstream byte and pings are sent
// every geminiPingInterval while the model is thinking, so Claude Code never
// sits on a silent connection. A pump goroutine reads the upstream stream
// while only the main goroutine writes events, avoiding write races.
func (ps *ProxyState) streamGeminiSSE(ctx context.Context, w http.ResponseWriter, resp *http.Response, backend *geminiBackend, originalModel string, estimatedInput int64) {
	s := &geminiStreamState{
		ps:             ps,
		w:              w,
		backendName:    backend.Name(),
		backend:        backend,
		originalModel:  originalModel,
		estimatedInput: estimatedInput,
		messageID:      "msg_gemini_" + randomHex(12),
	}
	if flusher, ok := w.(http.Flusher); ok {
		s.flusher = flusher
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	s.start()

	done := make(chan struct{})
	defer close(done)
	events := make(chan geminiStreamEvent, 16)

	go func() {
		err := scanSSEEvents(resp.Body, func(name string, data []byte) bool {
			var chunk geminiChunk
			if err := json.Unmarshal(data, &chunk); err != nil {
				logx.Debug("gemini stream: bad JSON frame: %.200s", string(data))
				return false
			}
			if err := geminiErrorOf(&chunk); err != nil {
				select {
				case events <- geminiStreamEvent{err: err}:
				case <-done:
				}
				return true
			}
			select {
			case events <- geminiStreamEvent{chunk: &chunk}:
			case <-done:
				return true
			}
			return false
		})
		if err != nil {
			select {
			case events <- geminiStreamEvent{err: err}:
			case <-done:
			}
		}
	}()

	ticker := time.NewTicker(geminiPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.finishAll()
			return

		case <-ticker.C:
			if !s.finished {
				s.emit("ping", map[string]interface{}{})
			}

		case ev, ok := <-events:
			if !ok {
				s.finishAll()
				return
			}
			if ev.err != nil {
				s.fail(ev.err.Error())
				return
			}
			s.handleChunk(&ev.chunk.geminiResponse)
			if s.finished {
				return
			}
		}
	}
}

func (s *geminiStreamState) emit(eventType string, payload map[string]interface{}) {
	sseWrite(s.w, s.flusher, eventType, payload)
}

func (s *geminiStreamState) send(eventType string, payload map[string]interface{}) {
	if s.finished {
		return
	}
	s.emit(eventType, payload)
}

func (s *geminiStreamState) start() {
	if s.started {
		return
	}
	s.started = true
	s.emit("message_start", map[string]interface{}{
		"message": map[string]interface{}{
			"id":            s.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         s.originalModel,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  s.estimatedInput,
				"output_tokens": 0,
			},
		},
	})
}

func (s *geminiStreamState) handleChunk(resp *geminiResponse) {
	if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
		s.blocked = resp.PromptFeedback.BlockReason
	}
	if !resp.UsageMetadata.isZero() {
		s.usage = resp.UsageMetadata
	}
	if len(resp.Candidates) == 0 {
		return
	}

	cand := resp.Candidates[0]
	if cand.FinishReason != "" {
		s.finish = cand.FinishReason
	}
	if cand.Content != nil {
		for _, part := range cand.Content.Parts {
			if part.ThoughtSignature != "" && s.thoughtSig == "" {
				s.thoughtSig = part.ThoughtSignature
			}
			if part.Thought {
				continue
			}
			switch {
			case part.FunctionCall != nil:
				id := part.FunctionCall.ID
				if id == "" {
					id = "gemini_" + randomHex(12)
				}
				s.tools = append(s.tools, geminiStreamTool{
					id:   id,
					name: part.FunctionCall.Name,
					args: part.FunctionCall.Args,
				})
			case part.Text != "":
				s.textDelta(part.Text)
			}
		}
	}

	if s.finish != "" {
		s.finishAll()
	}
}

func (s *geminiStreamState) textDelta(text string) {
	if s.finished || text == "" {
		return
	}
	if !s.textOpen {
		s.textIndex = s.nextIndex
		s.nextIndex++
		s.send("content_block_start", map[string]interface{}{
			"index":         s.textIndex,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
		s.textOpen = true
	}
	s.send("content_block_delta", map[string]interface{}{
		"index": s.textIndex,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	})
	s.sawText = true
}

func (s *geminiStreamState) closeText() {
	if s.textOpen {
		s.emit("content_block_stop", map[string]interface{}{"index": s.textIndex})
		s.textOpen = false
	}
}

func (s *geminiStreamState) finishAll() {
	if s.finished {
		return
	}
	s.finished = true

	truncated := s.finish == "MAX_TOKENS"
	tools := s.tools
	if truncated {
		// Truncated functionCall arguments may be invalid JSON.
		tools = nil
	}

	s.closeText()

	if s.blocked != "" && !s.sawText && len(tools) == 0 {
		s.emitStreamError("Gemini blocked the response: " + s.blocked)
		return
	}

	if len(tools) > 0 {
		ids := make([]string, 0, len(tools))
		for _, tool := range tools {
			idx := s.nextIndex
			s.nextIndex++
			s.emit("content_block_start", map[string]interface{}{
				"index": idx,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    tool.id,
					"name":  tool.name,
					"input": map[string]interface{}{},
				},
			})
			args := "{}"
			if len(tool.args) > 0 && string(tool.args) != "null" {
				args = string(tool.args)
			}
			s.emit("content_block_delta", map[string]interface{}{
				"index": idx,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": args},
			})
			s.emit("content_block_stop", map[string]interface{}{"index": idx})
			ids = append(ids, tool.id)
		}
		s.backend.sigs.putAll(ids, s.thoughtSig)
	}

	if !s.sawText && len(tools) == 0 {
		switch {
		case truncated:
			idx := s.nextIndex
			s.nextIndex++
			s.emit("content_block_start", map[string]interface{}{
				"index":         idx,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			s.emit("content_block_delta", map[string]interface{}{
				"index": idx,
				"delta": map[string]interface{}{"type": "text_delta", "text": geminiEmptyMaxTokensText},
			})
			s.emit("content_block_stop", map[string]interface{}{"index": idx})
		case s.finish != "":
			s.emitStreamError(fmt.Sprintf("Gemini returned no visible content (finishReason=%s)", s.finish))
			return
		default:
			s.emitStreamError("Gemini stream ended without content")
			return
		}
	}

	stopReason := "end_turn"
	switch {
	case truncated:
		stopReason = "max_tokens"
	case len(tools) > 0:
		stopReason = "tool_use"
	}

	s.emit("message_delta", map[string]interface{}{
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": geminiAnthropicUsage(s.usage),
	})
	s.emit("message_stop", map[string]interface{}{})

	input := geminiUncachedInput(s.usage)
	output := geminiOutputTokens(s.usage)
	if input == 0 && output == 0 {
		return
	}
	s.ps.costTracker.Record(s.backendName, input, output)
	logx.Info("#%d tokens: in=%d out=%d cached=%d (backend=%s)",
		s.ps.RequestCount(), input, output, s.usage.CachedContentTokenCount, s.backendName)
}

func (s *geminiStreamState) fail(message string) {
	if s.finished {
		return
	}
	s.finished = true
	s.closeText()
	s.emitStreamError(message)
}

func (s *geminiStreamState) emitStreamError(message string) {
	s.emit("error", map[string]interface{}{
		"error": map[string]interface{}{"type": "api_error", "message": message},
	})
	logx.Warn("gemini stream failed (backend=%s): %s", s.backendName, message)
}
