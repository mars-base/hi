package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mars-base/hi/pkg/logx"
)

// serveGemini handles Anthropic Messages and count_tokens requests for
// backends that speak the native Gemini generateContent protocol.
func (ps *ProxyState) serveGemini(w http.ResponseWriter, r *http.Request, backend Backend) {
	gb, ok := backend.(*geminiBackend)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "gemini backend type mismatch")
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	r.Body.Close()

	req, err := parseAnthropicRequest(bodyBytes)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The gateway does not implement :countTokens, so estimate locally.
	if IsCountTokensRequest(r.URL.Path) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"input_tokens": estimateInputTokens(req)})
		return
	}

	mappedModel, originalModel := ps.resolveModel(req.Model, backend)
	convertedReq, err := convertRequestToGemini(req, backend.ReasoningEffort(), gb.sigs)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	converted, err := json.Marshal(convertedReq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "failed to encode Gemini request")
		return
	}
	logx.Debug("gemini convert: model %s → %s, request %d → %d bytes (stream=%v)",
		req.Model, mappedModel, len(bodyBytes), len(converted), req.Stream)
	if bytes.Contains(bodyBytes, []byte("CRITICAL: Respond with TEXT ONLY")) {
		logx.Debug("gemini convert: request carries the compact summary prompt")
	}

	upstreamURL := geminiEndpointURL(backend.TargetURL(), mappedModel, req.Stream)

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL.String(), bytes.NewReader(converted))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "failed to create upstream request")
		return
	}

	for key, values := range r.Header {
		for _, v := range values {
			upstreamReq.Header.Add(key, v)
		}
	}
	cleanRequestHeaders(upstreamReq.Header)
	upstreamReq.Header.Del("Authorization")
	upstreamReq.Header.Del("x-api-key")
	upstreamReq.Header.Del("x-goog-api-key")
	upstreamReq.Header.Del("anthropic-version")
	upstreamReq.Header.Del("anthropic-beta")
	upstreamReq.Header.Del("Content-Length")
	upstreamReq.Header.Del("Transfer-Encoding")
	// Force identity encoding: Go's transport only auto-decompresses what it
	// negotiated itself, and a copied Accept-Encoding would break SSE reads.
	upstreamReq.Header.Set("Accept-Encoding", "identity")
	upstreamReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		upstreamReq.Header.Set("Accept", "text/event-stream")
	} else {
		upstreamReq.Header.Set("Accept", "application/json")
	}
	backend.SetAuth(upstreamReq)

	startTime := time.Now()
	resp, err := newTransport().RoundTrip(upstreamReq)
	if err != nil {
		logx.Error("Upstream error for %s (gemini): %v", backend.Name(), err)
		writeAnthropicError(w, http.StatusBadGateway, "upstream connection error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	elapsed := time.Since(startTime)
	logx.Info("#%d %s %s %s %d %s", ps.RequestCount(), r.Method, backend.Name(), r.URL.Path, resp.StatusCode, elapsed.Round(time.Millisecond))
	logx.Debug("  -> upstream: POST %s", upstreamURL.String())

	contentType := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(contentType, "text/event-stream")
	isJSON := strings.Contains(contentType, "application/json")

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		message := upstreamErrorMessage(errBody)
		logx.Warn("#%d upstream error (status=%d backend=%s): %s", ps.RequestCount(), resp.StatusCode, backend.Name(), message)
		writeAnthropicError(w, resp.StatusCode, message)
		return
	}

	switch {
	case isSSE && req.Stream:
		ps.streamGeminiSSE(r.Context(), w, resp, gb, originalModel, estimateInputTokens(req))

	case isSSE && !req.Stream:
		builder, err := collectGeminiStream(resp.Body)
		if err != nil {
			logx.Warn("#%d gemini stream collect failed: %v", ps.RequestCount(), err)
			writeAnthropicError(w, http.StatusBadGateway, err.Error())
			return
		}
		msg, err := builder.anthropicMessage(originalModel, gb.sigs)
		if err != nil {
			logx.Warn("#%d gemini conversion failed: %v", ps.RequestCount(), err)
			writeAnthropicError(w, http.StatusBadGateway, err.Error())
			return
		}
		ps.recordGeminiUsage(backend.Name(), builder.usage)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(msg)

	case isJSON:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "failed to read upstream response")
			return
		}
		msg, usage, err := convertGeminiJSON(body, originalModel, gb.sigs)
		if err != nil {
			logx.Warn("#%d gemini conversion failed: %v", ps.RequestCount(), err)
			writeAnthropicError(w, http.StatusBadGateway, err.Error())
			return
		}
		ps.recordGeminiUsage(backend.Name(), usage)
		if req.Stream {
			emitAnthropicMessageSSE(w, msg)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(msg)

	default:
		logx.Warn("#%d unexpected upstream content type %q (backend=%s)", ps.RequestCount(), contentType, backend.Name())
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("unexpected upstream content type: %s", contentType))
	}
}

func (ps *ProxyState) recordGeminiUsage(backendName string, usage geminiUsage) {
	input := geminiUncachedInput(usage)
	output := geminiOutputTokens(usage)
	if input == 0 && output == 0 {
		return
	}
	ps.costTracker.Record(backendName, input, output)
	logx.Info("#%d tokens: in=%d out=%d cached=%d (backend=%s)",
		ps.RequestCount(), input, output, usage.CachedContentTokenCount, backendName)
}
