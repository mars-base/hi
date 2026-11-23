package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/mars-base/hi/pkg/logx"
)

// geminiEmptyMaxTokensText is shown to the user when Gemini stops on
// MAX_TOKENS without producing any visible text or tool call. Silently
// streaming an empty message is what makes Claude Code appear to hang.
const geminiEmptyMaxTokensText = "[Gemini returned no content: max output tokens reached]"

// ==============================
// Native Gemini response types
// ==============================

type geminiUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
}

func (u geminiUsage) isZero() bool {
	return u.PromptTokenCount == 0 && u.CandidatesTokenCount == 0 &&
		u.ThoughtsTokenCount == 0 && u.CachedContentTokenCount == 0 && u.TotalTokenCount == 0
}

type geminiPromptFeedback struct {
	BlockReason string `json:"blockReason"`
}

type geminiCandidate struct {
	Content      *geminiContent `json:"content"`
	FinishReason string         `json:"finishReason"`
	Index        int            `json:"index"`
}

type geminiErrorBody struct {
	Message string `json:"message"`
	Status  string `json:"status"`
	Code    int    `json:"code"`
}

type geminiResponse struct {
	ResponseID     string                `json:"responseId"`
	ModelVersion   string                `json:"modelVersion"`
	Candidates     []geminiCandidate     `json:"candidates"`
	PromptFeedback *geminiPromptFeedback `json:"promptFeedback"`
	UsageMetadata  geminiUsage           `json:"usageMetadata"`
}

// geminiChunk is one JSON frame from either a stream or a non-stream reply;
// it additionally detects gateway error envelopes returned with HTTP 200.
type geminiChunk struct {
	geminiResponse
	Error *geminiErrorBody `json:"error"`
}

func geminiErrorOf(chunk *geminiChunk) error {
	if chunk.Error == nil {
		return nil
	}
	if chunk.Error.Message != "" {
		return fmt.Errorf("upstream error: %s", chunk.Error.Message)
	}
	return fmt.Errorf("upstream error (status=%s code=%d)", chunk.Error.Status, chunk.Error.Code)
}

// ==============================
// Response accumulation
// ==============================

// geminiBuilder merges one or more Gemini responses (a single JSON body or a
// sequence of SSE chunks) into a single Anthropic message.
type geminiBuilder struct {
	messageID  string
	model      string
	parts      []geminiPart
	usage      geminiUsage
	finish     string
	blocked    string
	thoughtSig string
}

func (b *geminiBuilder) addResponse(resp *geminiResponse) {
	if resp.ResponseID != "" {
		b.messageID = "msg_" + resp.ResponseID
	}
	if resp.ModelVersion != "" {
		b.model = resp.ModelVersion
	}
	if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
		b.blocked = resp.PromptFeedback.BlockReason
	}
	if !resp.UsageMetadata.isZero() {
		// Gemini reports cumulative usage; keep the latest non-zero snapshot.
		b.usage = resp.UsageMetadata
	}
	if len(resp.Candidates) == 0 {
		return
	}
	cand := resp.Candidates[0]
	if cand.FinishReason != "" {
		b.finish = cand.FinishReason
	}
	if cand.Content == nil {
		return
	}
	for _, part := range cand.Content.Parts {
		if part.ThoughtSignature != "" && b.thoughtSig == "" {
			b.thoughtSig = part.ThoughtSignature
		}
		if part.Thought {
			continue
		}
		switch {
		case part.FunctionCall != nil:
			b.parts = append(b.parts, part)
		case part.Text != "":
			if n := len(b.parts); n > 0 && b.parts[n-1].FunctionCall == nil && b.parts[n-1].InlineData == nil &&
				b.parts[n-1].FileData == nil && b.parts[n-1].FunctionResponse == nil {
				b.parts[n-1].Text += part.Text
			} else {
				b.parts = append(b.parts, geminiPart{Text: part.Text})
			}
		case part.InlineData != nil || part.FileData != nil:
			logx.Debug("gemini convert: dropping inline/file data from response")
		}
	}
}

// anthropicMessage renders the accumulated response as an Anthropic message.
func (b *geminiBuilder) anthropicMessage(originalModel string, sigs *thoughtSigStore) (map[string]interface{}, error) {
	truncated := b.finish == "MAX_TOKENS"

	var blocks []interface{}
	var toolIDs []string
	for _, part := range b.parts {
		switch {
		case part.FunctionCall != nil:
			if truncated {
				// Arguments may be cut off mid-JSON; never hand them to a client
				// as an executable tool call.
				continue
			}
			id := part.FunctionCall.ID
			if id == "" {
				id = "gemini_" + randomHex(12)
			}
			blocks = append(blocks, map[string]interface{}{
				"type":  "tool_use",
				"id":    id,
				"name":  part.FunctionCall.Name,
				"input": geminiToolInput(part.FunctionCall.Args),
			})
			toolIDs = append(toolIDs, id)
		case part.Text != "":
			blocks = append(blocks, map[string]interface{}{"type": "text", "text": part.Text})
		}
	}

	if len(toolIDs) > 0 {
		sigs.putAll(toolIDs, b.thoughtSig)
	}

	if len(blocks) == 0 {
		switch {
		case b.blocked != "":
			return nil, fmt.Errorf("Gemini blocked the response: %s", b.blocked)
		case truncated:
			blocks = append(blocks, map[string]interface{}{"type": "text", "text": geminiEmptyMaxTokensText})
		case b.finish != "":
			return nil, fmt.Errorf("Gemini returned no visible content (finishReason=%s)", b.finish)
		default:
			return nil, fmt.Errorf("Gemini returned an empty response")
		}
	}

	model := originalModel
	if model == "" {
		model = b.model
	}

	stopReason := "end_turn"
	switch {
	case truncated:
		stopReason = "max_tokens"
	case len(toolIDs) > 0:
		stopReason = "tool_use"
	}

	id := b.messageID
	if id == "" {
		id = "msg_gemini_" + randomHex(12)
	}

	return map[string]interface{}{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         geminiAnthropicUsage(b.usage),
	}, nil
}

func geminiToolInput(args json.RawMessage) interface{} {
	if len(args) == 0 || string(args) == "null" {
		return map[string]interface{}{}
	}
	var v interface{}
	if err := json.Unmarshal(args, &v); err != nil {
		logx.Warn("gemini convert: invalid tool arguments: %v", err)
		return map[string]interface{}{"_raw_arguments": string(args)}
	}
	if v == nil {
		return map[string]interface{}{}
	}
	return v
}

func geminiAnthropicUsage(u geminiUsage) map[string]interface{} {
	input := u.PromptTokenCount - u.CachedContentTokenCount
	if input < 0 {
		input = 0
	}
	return map[string]interface{}{
		"input_tokens":                input,
		"output_tokens":               u.CandidatesTokenCount + u.ThoughtsTokenCount,
		"cache_read_input_tokens":     u.CachedContentTokenCount,
		"cache_creation_input_tokens": 0,
	}
}

func geminiUncachedInput(u geminiUsage) int64 {
	n := u.PromptTokenCount - u.CachedContentTokenCount
	if n < 0 {
		return 0
	}
	return n
}

func geminiOutputTokens(u geminiUsage) int64 {
	return u.CandidatesTokenCount + u.ThoughtsTokenCount
}

// convertGeminiJSON converts a non-streaming Gemini response body into an
// Anthropic message.
func convertGeminiJSON(body []byte, originalModel string, sigs *thoughtSigStore) (map[string]interface{}, geminiUsage, error) {
	var chunk geminiChunk
	if err := json.Unmarshal(body, &chunk); err != nil {
		return nil, geminiUsage{}, fmt.Errorf("invalid Gemini payload: %w", err)
	}
	if err := geminiErrorOf(&chunk); err != nil {
		return nil, geminiUsage{}, err
	}
	b := &geminiBuilder{}
	b.addResponse(&chunk.geminiResponse)
	msg, err := b.anthropicMessage(originalModel, sigs)
	return msg, b.usage, err
}

// collectGeminiStream drains an SSE stream and returns the accumulated
// builder (used when the client asked for a non-streaming reply).
func collectGeminiStream(r io.Reader) (*geminiBuilder, error) {
	b := &geminiBuilder{}
	var streamErr error

	err := scanSSEEvents(r, func(name string, data []byte) bool {
		var chunk geminiChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			logx.Debug("gemini stream: bad JSON frame: %.200s", string(data))
			return false
		}
		if err := geminiErrorOf(&chunk); err != nil {
			streamErr = err
			return true
		}
		b.addResponse(&chunk.geminiResponse)
		return false
	})
	if err != nil {
		return b, err
	}
	if streamErr != nil {
		return b, streamErr
	}
	return b, nil
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
