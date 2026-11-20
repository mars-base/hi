package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mars-base/hi/pkg/logx"
)

// serveOpenAIResponses handles Anthropic Messages and count_tokens requests
// for backends that speak the OpenAI Responses protocol.
func (ps *ProxyState) serveOpenAIResponses(w http.ResponseWriter, r *http.Request, backend Backend) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	r.Body.Close()

	if IsCountTokensRequest(r.URL.Path) {
		req, err := parseAnthropicRequest(bodyBytes)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"input_tokens": estimateInputTokens(req)})
		return
	}

	req, err := parseAnthropicRequest(bodyBytes)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}

	mappedModel, originalModel := ps.resolveModel(req.Model, backend)
	converted, err := convertRequestToResponses(req, mappedModel, backend.ReasoningEffort())
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	logx.Debug("responses convert: model %s → %s, request %d → %d bytes (stream=%v)",
		req.Model, mappedModel, len(bodyBytes), len(converted), req.Stream)

	upstreamURL := responsesEndpointURL(backend.TargetURL())

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
	upstreamReq.Header.Del("anthropic-version")
	upstreamReq.Header.Del("anthropic-beta")
	// Force identity encoding: a copied Accept-Encoding: gzip would leave
	// the SSE body compressed, since Go's transport only auto-decompresses
	// what it negotiated itself.
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
		logx.Error("Upstream error for %s (responses): %v", backend.Name(), err)
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
		ps.streamResponsesSSE(w, resp, backend.Name(), originalModel, estimateInputTokens(req))

	case isSSE && !req.Stream:
		obj, err := collectResponsesStream(resp.Body)
		if err != nil {
			logx.Warn("#%d responses stream collect failed: %v", ps.RequestCount(), err)
			writeAnthropicError(w, http.StatusBadGateway, err.Error())
			return
		}
		ps.writeResponsesJSON(w, obj, originalModel, backend.Name())

	case isJSON:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "failed to read upstream response")
			return
		}
		out, usage, err := convertResponsesJSON(body, originalModel)
		if err != nil {
			logx.Warn("#%d invalid Responses payload: %v", ps.RequestCount(), err)
			writeAnthropicError(w, http.StatusBadGateway, err.Error())
			return
		}
		ps.recordResponsesUsage(backend.Name(), usage)
		if req.Stream {
			var msg map[string]interface{}
			if err := json.Unmarshal(out, &msg); err != nil {
				writeAnthropicError(w, http.StatusBadGateway, "failed to encode converted response")
				return
			}
			emitAnthropicMessageSSE(w, msg)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(out)

	default:
		logx.Warn("#%d unexpected upstream content type %q (backend=%s)", ps.RequestCount(), contentType, backend.Name())
		writeAnthropicError(w, http.StatusBadGateway, "unexpected upstream content type: "+contentType)
	}
}

// writeResponsesJSON converts a collected Responses object to an Anthropic
// message and writes it as JSON.
func (ps *ProxyState) writeResponsesJSON(w http.ResponseWriter, obj responsesObject, originalModel, backendName string) {
	msg := responsesToAnthropicMessage(obj, originalModel)
	ps.recordResponsesUsage(backendName, obj.Usage)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(msg)
}

func (ps *ProxyState) recordResponsesUsage(backendName string, usage responsesUsage) {
	input := uncachedInputTokens(usage)
	if input == 0 && usage.OutputTokens == 0 {
		return
	}
	ps.costTracker.Record(backendName, input, usage.OutputTokens)
	logx.Info("#%d tokens: in=%d out=%d cached=%d (backend=%s)",
		ps.RequestCount(), input, usage.OutputTokens, usage.InputTokensDetails.CachedTokens, backendName)
}
