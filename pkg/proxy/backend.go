// Package proxy provides the HTTP proxy server and backend management.
package proxy

import (
	"crypto/tls"
	"fmt"
	"github.com/mars-base/hi/pkg/logx"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mars-base/hi/pkg/config"
)

// Backend represents an upstream API backend.
type Backend interface {
	// Name returns the backend identifier (e.g. "claude", "deepseek").
	Name() string

	// TargetURL returns the upstream API base URL.
	TargetURL() *url.URL

	// SetAuth sets the authentication header on an outgoing request.
	SetAuth(req *http.Request)

	// MapModel translates a Claude model name to the backend's equivalent.
	// Returns the original name if no mapping exists.
	MapModel(model string) string

	// NeedsThinkingStrip returns true if thinking blocks must be removed
	// from message content[] before forwarding.
	NeedsThinkingStrip() bool

	// StripTopLevelThinking returns true if the top-level "thinking" request
	// field should be removed before forwarding. Defaults to config value:
	// true for deepseek, false for anthropic.
	StripTopLevelThinking() bool

	// IsAnthropicAPI returns true only for the official Anthropic API
	// (api.anthropic.com). Third-party anthropic-compatible backends return false.
	// Used to decide whether to keep Anthropic-specific request fields like
	// context_management.
	IsAnthropicAPI() bool

	// ReasoningEffort returns the output_config.effort level for this backend.
	// Used by deepseek-type backends to set max reasoning. Empty means disabled.
	ReasoningEffort() string

	// ModelInfo returns the backend's tier model names for logging.
	ModelInfo() map[string]string
}

// NewBackend creates a Backend from a BackendConfig.
func NewBackend(name string, cfg config.BackendConfig) (Backend, error) {
	switch cfg.Type {
	case "anthropic":
		return newAnthropicBackend(name, cfg)
	case "deepseek":
		return newDeepSeekBackend(name, cfg)
	default:
		return nil, fmt.Errorf("unknown backend type: %s", cfg.Type)
	}
}

// ==============================
// Anthropic (Claude) Backend
// ==============================

type anthropicBackend struct {
	name          string
	target        *url.URL
	apiKey        string
	modelMap      map[string]string
	stripTopThink bool
	reasonEffort  string
}

func newAnthropicBackend(name string, cfg config.BackendConfig) (*anthropicBackend, error) {
	u, err := url.Parse("https://api.anthropic.com")
	if err != nil {
		return nil, fmt.Errorf("invalid default Anthropic URL: %w", err)
	}
	if cfg.BaseURL != "" {
		u, err = url.Parse(cfg.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("invalid base_url for %s: %w", name, err)
		}
	}

	apiKey := config.ResolveAPIKey(cfg.APIKey)
	if apiKey == "" {
		// Fall back to ANTHROPIC_API_KEY env var.
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}

	hasKey := apiKey != ""
	logx.Info("backend %s: url=%s auth=%s key=%s",
		name, u.String(), authMethod(hasKey, apiKey), maskKeyLog(apiKey))

	return &anthropicBackend{
		name:          name,
		target:        u,
		apiKey:        apiKey,
		stripTopThink: cfg.ShouldStripThinking(),
		reasonEffort:  cfg.ReasoningEffort,
		modelMap: map[string]string{
			"claude-opus-4-6":            cfg.Models.Opus,
			"claude-opus-4-7":            cfg.Models.Opus,
			"claude-opus-4-8":            cfg.Models.Opus,
			"claude-opus-5":              cfg.Models.Opus,
			"claude-sonnet-4-6":          cfg.Models.Sonnet,
			"claude-sonnet-4-5-20250929": cfg.Models.Sonnet,
			"claude-sonnet-5":            cfg.Models.Sonnet,
			"claude-haiku-4-5-20251001":  cfg.Models.Haiku,
		},
	}, nil
}

func (b *anthropicBackend) Name() string                { return b.name }
func (b *anthropicBackend) NeedsThinkingStrip() bool    { return true }
func (b *anthropicBackend) StripTopLevelThinking() bool { return b.stripTopThink }
func (b *anthropicBackend) ReasoningEffort() string     { return b.reasonEffort }
func (b *anthropicBackend) IsAnthropicAPI() bool {
	return strings.Contains(b.target.Host, "anthropic.com")
}

func (b *anthropicBackend) TargetURL() *url.URL { return b.target }

func (b *anthropicBackend) SetAuth(req *http.Request) {
	if b.apiKey != "" {
		req.Header.Set("x-api-key", b.apiKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
}

func (b *anthropicBackend) MapModel(model string) string {
	if mapped, ok := b.modelMap[model]; ok {
		return mapped
	}
	return model
}

func (b *anthropicBackend) ModelInfo() map[string]string {
	return map[string]string{
		"opus":   b.modelMap["claude-opus-5"],
		"sonnet": b.modelMap["claude-sonnet-5"],
		"haiku":  b.modelMap["claude-haiku-4-5-20251001"],
	}
}

// ==============================
// DeepSeek Backend
// ==============================

type deepseekBackend struct {
	name          string
	target        *url.URL
	apiKey        string
	modelMap      map[string]string
	stripTopThink bool
	reasonEffort  string
}

func newDeepSeekBackend(name string, cfg config.BackendConfig) (*deepseekBackend, error) {
	defaultURL := "https://api.deepseek.com/anthropic"
	if cfg.BaseURL != "" {
		defaultURL = cfg.BaseURL
	}
	u, err := url.Parse(defaultURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base_url for %s: %w", name, err)
	}

	apiKey := config.ResolveAPIKey(cfg.APIKey)

	hasKey := apiKey != ""
	logx.Info("backend %s: url=%s auth=%s key=%s",
		name, u.String(), authMethod(hasKey, apiKey), maskKeyLog(apiKey))

	return &deepseekBackend{
		name:          name,
		target:        u,
		apiKey:        apiKey,
		stripTopThink: cfg.ShouldStripThinking(),
		reasonEffort:  cfg.ReasoningEffort,
		modelMap: map[string]string{
			"claude-opus-4-6":            cfg.Models.Opus,
			"claude-opus-4-7":            cfg.Models.Opus,
			"claude-opus-4-8":            cfg.Models.Opus,
			"claude-opus-5":              cfg.Models.Opus,
			"claude-sonnet-4-6":          cfg.Models.Sonnet,
			"claude-sonnet-4-5-20250929": cfg.Models.Sonnet,
			"claude-sonnet-5":            cfg.Models.Sonnet,
			"claude-haiku-4-5-20251001":  cfg.Models.Haiku,
		},
	}, nil
}

func (b *deepseekBackend) Name() string                { return b.name }
func (b *deepseekBackend) NeedsThinkingStrip() bool    { return false }
func (b *deepseekBackend) StripTopLevelThinking() bool { return b.stripTopThink }
func (b *deepseekBackend) ReasoningEffort() string     { return b.reasonEffort }
func (b *deepseekBackend) IsAnthropicAPI() bool        { return false }

func (b *deepseekBackend) TargetURL() *url.URL { return b.target }

func (b *deepseekBackend) SetAuth(req *http.Request) {
	req.Header.Set("x-api-key", b.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
}

func (b *deepseekBackend) MapModel(model string) string {
	if mapped, ok := b.modelMap[model]; ok {
		return mapped
	}
	return model
}

func (b *deepseekBackend) ModelInfo() map[string]string {
	return map[string]string{
		"opus":   b.modelMap["claude-opus-5"],
		"sonnet": b.modelMap["claude-sonnet-5"],
		"haiku":  b.modelMap["claude-haiku-4-5-20251001"],
	}
}

// ==============================
// HTTP Transport
// ==============================

// newTransport returns an http.Transport configured for proxying.
func newTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		MaxIdleConns:       100,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: false,
	}
}

// Strip hop-by-hop headers from the response.
func stripHopByHop(h http.Header) {
	hopByHop := []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
	}
	for _, hdr := range hopByHop {
		h.Del(hdr)
	}
}

// Strip hop-by-hop and other headers from the incoming request.
func cleanRequestHeaders(h http.Header) {
	h.Del("Connection")
	h.Del("Keep-Alive")
	h.Del("Proxy-Connection")
}

// IsModelRequest returns true if the path is /v1/messages.
func IsModelRequest(path string) bool {
	// Strip query string.
	if idx := strings.Index(path, "?"); idx >= 0 {
		path = path[:idx]
	}
	return path == "/v1/messages" || path == "/v1/messages/"
}

// --- debug helpers ---

func authMethod(hasKey bool, key string) string {
	if !hasKey || key == "" {
		return "none"
	}
	return "x-api-key"
}

func maskKeyLog(s string) string {
	if len(s) <= 8 {
		return "<empty>"
	}
	return "***" + s[len(s)-4:]
}
