package proxy

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/mars-base/hi/pkg/config"
	"github.com/mars-base/hi/pkg/logx"
)

// openAIResponsesBackend speaks the OpenAI Responses API (/v1/responses).
// The proxy converts Anthropic Messages requests into this protocol and
// converts responses back, so Claude Code stays unaware of the switch.
type openAIResponsesBackend struct {
	name     string
	target   *url.URL
	apiKey   string
	modelMap map[string]string
	effort   string
}

func newOpenAIResponsesBackend(name string, cfg config.BackendConfig) (*openAIResponsesBackend, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base_url for %s: %w", name, err)
	}

	// Normalize: strip trailing slash and a trailing /v1 so that appending
	// "/v1/responses" never produces "/v1/v1/responses".
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/v1")

	apiKey := config.ResolveAPIKey(cfg.APIKey)
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}

	auth := "none"
	if apiKey != "" {
		auth = "bearer"
	}
	logx.Info("backend %s: type=openai-response url=%s auth=%s key=%s",
		name, u.String(), auth, maskKeyLog(apiKey))

	return &openAIResponsesBackend{
		name:     name,
		target:   u,
		apiKey:   apiKey,
		modelMap: backendModelMap(cfg),
		effort:   cfg.ReasoningEffort,
	}, nil
}

func (b *openAIResponsesBackend) Name() string                { return b.name }
func (b *openAIResponsesBackend) Kind() APIKind               { return KindOpenAIResponses }
func (b *openAIResponsesBackend) NeedsThinkingStrip() bool    { return true }
func (b *openAIResponsesBackend) StripTopLevelThinking() bool { return true }
func (b *openAIResponsesBackend) IsAnthropicAPI() bool        { return false }
func (b *openAIResponsesBackend) ReasoningEffort() string     { return b.effort }

func (b *openAIResponsesBackend) TargetURL() *url.URL { return b.target }

func (b *openAIResponsesBackend) SetAuth(req *http.Request) {
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
}

func (b *openAIResponsesBackend) MapModel(model string) string {
	if mapped, ok := b.modelMap[model]; ok {
		return mapped
	}
	return model
}

func (b *openAIResponsesBackend) ModelInfo() map[string]string {
	return map[string]string{
		"opus":   b.MapModel("claude-opus-5"),
		"sonnet": b.MapModel("claude-sonnet-5"),
		"haiku":  b.MapModel("claude-haiku-4-5-20251001"),
	}
}

// responsesEndpointURL returns the /v1/responses endpoint for a base URL.
func responsesEndpointURL(base *url.URL) *url.URL {
	u := *base
	u.Path = joinURLPath(base.Path, "/v1/responses")
	u.RawQuery = ""
	return &u
}
