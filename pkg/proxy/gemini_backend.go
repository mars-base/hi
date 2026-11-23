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

// geminiBackend speaks the native Google Gemini API
// (POST /v1beta/models/{model}:generateContent). The proxy converts
// Anthropic Messages requests into this protocol and converts responses
// back, so Claude Code stays unaware of the switch.
type geminiBackend struct {
	name     string
	target   *url.URL
	apiKey   string
	modelMap map[string]string
	effort   string
	sigs     *thoughtSigStore
}

func newGeminiBackend(name string, cfg config.BackendConfig) (*geminiBackend, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base_url for %s: %w", name, err)
	}

	// Normalize: strip trailing slash and a trailing /v1beta or /v1 so that
	// appending "/v1beta/models/..." never duplicates the version segment.
	u.Path = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/v1beta"), "/v1")

	apiKey := config.ResolveAPIKey(cfg.APIKey)
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		apiKey = os.Getenv("GOOGLE_API_KEY")
	}

	auth := "none"
	if apiKey != "" {
		auth = "x-goog-api-key"
	}
	logx.Info("backend %s: type=gemini url=%s auth=%s key=%s",
		name, u.String(), auth, maskKeyLog(apiKey))

	return &geminiBackend{
		name:     name,
		target:   u,
		apiKey:   apiKey,
		modelMap: backendModelMap(cfg),
		effort:   cfg.ReasoningEffort,
		sigs:     newThoughtSigStore(),
	}, nil
}

func (b *geminiBackend) Name() string                { return b.name }
func (b *geminiBackend) Kind() APIKind               { return KindGemini }
func (b *geminiBackend) NeedsThinkingStrip() bool    { return true }
func (b *geminiBackend) StripTopLevelThinking() bool { return true }
func (b *geminiBackend) IsAnthropicAPI() bool        { return false }
func (b *geminiBackend) ReasoningEffort() string     { return b.effort }

func (b *geminiBackend) TargetURL() *url.URL { return b.target }

func (b *geminiBackend) SetAuth(req *http.Request) {
	if b.apiKey != "" {
		req.Header.Set("x-goog-api-key", b.apiKey)
	}
}

func (b *geminiBackend) MapModel(model string) string {
	if mapped, ok := b.modelMap[model]; ok {
		return mapped
	}
	return model
}

func (b *geminiBackend) ModelInfo() map[string]string {
	return map[string]string{
		"opus":   b.MapModel("claude-opus-5"),
		"sonnet": b.MapModel("claude-sonnet-5"),
		"haiku":  b.MapModel("claude-haiku-4-5-20251001"),
	}
}

// geminiEndpointURL returns the native Gemini endpoint for a model.
// Streaming uses :streamGenerateContent with alt=sse.
func geminiEndpointURL(base *url.URL, model string, stream bool) *url.URL {
	model = strings.TrimPrefix(model, "models/")
	action := ":generateContent"
	if stream {
		action = ":streamGenerateContent"
	}
	u := *base
	u.Path = joinURLPath(base.Path, "/v1beta/models/"+url.PathEscape(model)+action)
	u.RawQuery = ""
	if stream {
		u.RawQuery = "alt=sse"
	}
	return &u
}
