// Package config manages YAML configuration for hi.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mars-base/hi/pkg/logx"
	"gopkg.in/yaml.v3"
)

// ccSettings represents the relevant parts of ~/.claude/settings.json.
type ccSettings struct {
	Env map[string]string `json:"env"`
}

// ccEnv caches env vars loaded from ~/.claude/settings.json.
var ccEnv map[string]string

// CCMissing reports whether no .claude/settings.json was found or it had no env block.
func CCMissing() bool { return len(ccEnv) == 0 }

// GetPricing returns per-backend pricing for cost tracking from config.
func (c *Config) GetPricing() map[string]PricingPerMillion {
	out := make(map[string]PricingPerMillion, len(c.Backends)+1)
	out["_default"] = PricingPerMillion{Input: 0.42, Output: 0.83}
	for name, b := range c.Backends {
		if b.Pricing.Input > 0 || b.Pricing.Output > 0 {
			out[name] = b.Pricing
		}
	}
	return out
}

// GenerateSlashCommands creates ~/.claude/commands/{name}.md for each
// configured backend so users can switch with /deepseek or /claude.
func GenerateSlashCommands(cfg *Config) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".claude", "commands")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	proxyPort := cfg.ProxyPort
	fmt.Println("hi: Generated slash commands:")
	for name, bc := range cfg.Backends {
		model := bc.Models.Sonnet
		if model == "" {
			model = name
		}
		tmpl := "Switch the hi proxy to the **{{name}}** backend.\n\nMake an HTTP POST to `http://127.0.0.1:{{port}}/_proxy/mode` with body `backend={{name}}`. Report the result.\n\nIf the response contains `\"mode\": \"{{name}}\"`, say \"Switched to {{name}}\"."
		content := strings.NewReplacer("{{name}}", name, "{{port}}", fmt.Sprint(proxyPort)).Replace(tmpl)
		path := filepath.Join(dir, name+".md")
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			logx.Warn("failed to create slash command %s: %v", path, err)
			continue
		}
		fmt.Printf("  /%s → %s\n", name, model)
		logx.Info("slash command: /%s → %s (proxy :%d)", name, model, proxyPort)
	}
	return nil
}

// statusLineStorePath returns the path to the file storing the original
// statusLine.command, used by cmdStatusline to delegate rendering.
func statusLineStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hi", ".statusline-command"), nil
}

// OriginalStatusCommand reads the original statusLine.command saved by CCOverride.
func OriginalStatusCommand() string {
	sp, err := statusLineStorePath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(sp)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// setOriginalStatusCommand persists the original command to disk.
func setOriginalStatusCommand(cmd string) {
	sp, err := statusLineStorePath()
	if err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(sp), 0755)
	os.WriteFile(sp, []byte(cmd), 0600)
}

// ModelMapping maps Claude model tiers to backend-specific model names.
type ModelMapping struct {
	Opus   string `yaml:"opus"`
	Sonnet string `yaml:"sonnet"`
	Haiku  string `yaml:"haiku"`
}

// PricingPerMillion is the cost in USD per 1 million tokens.
type PricingPerMillion struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`
}

// BackendConfig defines a single API backend.
type BackendConfig struct {
	// Type is "anthropic" for Claude or "deepseek" for DeepSeek.
	Type string `yaml:"type"`
	// BaseURL is the API endpoint. Empty means use the default.
	BaseURL string `yaml:"base_url"`
	// APIKey is the auth token. Supports ${ENV_VAR} expansion.
	APIKey string `yaml:"api_key"`
	// Models maps Claude model tiers to this backend's model IDs.
	Models ModelMapping `yaml:"models"`
	// Pricing is the cost in USD per 1M input/output tokens.
	Pricing PricingPerMillion `yaml:"pricing"`
	// StripThinking removes the top-level "thinking" config from requests
	// before forwarding. Some backends (e.g. DeepSeek) require every
	// request in a conversation to have the same thinking enabled/disabled
	// state, and Claude Code's tool-use requests may break this invariant.
	// Default based on backend type: true for "anthropic", false for "deepseek".
	StripThinking *bool `yaml:"strip_thinking,omitempty"`
	// ContextWindow is the max context window size for this backend.
	// Used by statusline. Default: 1M for deepseek, 200k for anthropic.
	ContextWindow *int64 `yaml:"context_window,omitempty"`
	// ReasoningEffort sets the output_config.effort level for deepseek-type
	// backends. Values: "high" (default), "max". Ignored for anthropic type.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty"`
}

// ShouldStripThinking returns whether the top-level "thinking" config should be
// stripped. If unset, defaults to true for anthropic and false for deepseek.
func (b BackendConfig) ShouldStripThinking() bool {
	if b.StripThinking != nil {
		return *b.StripThinking
	}
	return b.Type != "deepseek"
}

// ContextWindowOr returns the context window size. If unset, uses defaults.
func (b BackendConfig) ContextWindowOr() int64 {
	if b.ContextWindow != nil {
		return *b.ContextWindow
	}
	if b.Type == "deepseek" {
		return 1_000_000 // 1M
	}
	return 200_000 // 200k for anthropic and others
}

// CCEnvConfig holds environment variables passed to Claude Code.
type CCEnvConfig struct {
	AutoCompactWindow          int   `yaml:"auto_compact_window"`          // CLAUDE_CODE_AUTO_COMPACT_WINDOW
	AutocompactPctOverride     int   `yaml:"autocompact_pct_override"`     // CLAUDE_AUTOCOMPACT_PCT_OVERRIDE
	DisableNonessentialTraffic *bool `yaml:"disable_nonessential_traffic"` // CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC
}

// Config is the root configuration structure.
type Config struct {
	ActiveBackend string                   `yaml:"active_backend"`
	ProxyPort     int                      `yaml:"proxy_port"`
	Backends      map[string]BackendConfig `yaml:"backends"`
	Env           CCEnvConfig              `yaml:"env"`
}

// DefaultBackends returns the built-in backend definitions.
func DefaultBackends() map[string]BackendConfig {
	return map[string]BackendConfig{

		"claude": {
			Type:    "anthropic",
			BaseURL: "https://api.anthropic.com",
			APIKey:  "${ANTHROPIC_API_KEY}",
			Pricing: PricingPerMillion{Input: 3.00, Output: 15.00},
			Models: ModelMapping{
				Opus:   "claude-opus-4-8",
				Sonnet: "claude-sonnet-5",
				Haiku:  "claude-haiku-4-5-20251001",
			},
		},
		"deepseek": {
			Type:    "deepseek",
			BaseURL: "https://api.deepseek.com/anthropic",
			APIKey:  "${DEEPSEEK_API_KEY}",
			Pricing: PricingPerMillion{Input: 0.42, Output: 0.83},
			Models: ModelMapping{
				Opus:   "deepseek-v4-pro[1m]",
				Sonnet: "deepseek-v4-pro[1m]",
				Haiku:  "deepseek-v4-flash[1m]",
			},
		},
	}
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	disableTraffic := true
	return &Config{
		ActiveBackend: "deepseek",
		ProxyPort:     18799,
		Backends:      DefaultBackends(),
		Env: CCEnvConfig{
			AutoCompactWindow:          200000,
			AutocompactPctOverride:     90,
			DisableNonessentialTraffic: &disableTraffic,
		},
	}
}

// Path returns the config file path: ~/.hi/config.yaml
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".hi", "config.yaml"), nil
}

// Load reads the config from disk, or returns defaults if it doesn't exist.
// It also loads ~/.claude/settings.json env vars as a fallback for API key resolution.
func Load() (*Config, error) {
	cfgPath, err := Path()
	if err != nil {
		return nil, err
	}

	// Load Claude Code settings.json env block (if present) for API key fallback.
	loadCCSettings()

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := DefaultConfig()
			if saveErr := cfg.Save(); saveErr != nil {
				return nil, fmt.Errorf("failed to write default config: %w", saveErr)
			}
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to read config %s: %w", cfgPath, err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config %s: %w", cfgPath, err)
	}

	// Merge missing backends from defaults.
	defaults := DefaultConfig()
	if cfg.Backends == nil {
		cfg.Backends = make(map[string]BackendConfig)
	}
	for k, v := range defaults.Backends {
		if _, ok := cfg.Backends[k]; !ok {
			cfg.Backends[k] = v
		}
	}
	if cfg.ProxyPort == 0 {
		cfg.ProxyPort = defaults.ProxyPort
	}
	if cfg.ActiveBackend == "" {
		cfg.ActiveBackend = defaults.ActiveBackend
	}
	if cfg.Env.AutoCompactWindow == 0 {
		cfg.Env.AutoCompactWindow = defaults.Env.AutoCompactWindow
	}
	if cfg.Env.AutocompactPctOverride == 0 {
		cfg.Env.AutocompactPctOverride = defaults.Env.AutocompactPctOverride
	}
	if cfg.Env.DisableNonessentialTraffic == nil {
		cfg.Env.DisableNonessentialTraffic = defaults.Env.DisableNonessentialTraffic
	}

	return cfg, nil
}

// Save writes the config to disk.
func (c *Config) Save() error {
	cfgPath, err := Path()
	if err != nil {
		return err
	}

	// Ensure directory exists.
	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory %s: %w", dir, err)
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config %s: %w", cfgPath, err)
	}
	return nil
}

// ResolveAPIKey expands ${VAR} references in the API key string.
// It checks: 1) process env, 2) ~/.claude/settings.json env fallback.
func ResolveAPIKey(raw string) string {
	if len(raw) < 5 {
		return raw
	}
	// Match ${VAR} pattern.
	if raw[0] == '$' && raw[1] == '{' && raw[len(raw)-1] == '}' {
		envName := raw[2 : len(raw)-1]
		if v := os.Getenv(envName); v != "" {
			logx.Debug("env: %s=%s (from OS env)", envName, maskKey(v))
			return v
		}
		if v := ccEnv[envName]; v != "" {
			logx.Debug("env: %s=%s (from ~/.claude/settings.json)", envName, maskKey(v))
			return v
		}
		logx.Warn("env: %s=<not set>", envName)
	}
	return raw
}

// maskKey shows only the last 4 characters of a key, for safe logging.
func maskKey(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return "..." + s[len(s)-4:]
}

// loadCCSettings reads ~/.claude/settings.json and caches its env block.
func loadCCSettings() {
	if ccEnv != nil {
		return // already loaded
	}

	home, err := os.UserHomeDir()
	if err != nil {
		ccEnv = map[string]string{}
		return
	}

	settingsPath := filepath.Join(home, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		ccEnv = map[string]string{}
		return
	}

	var s ccSettings
	if err := json.Unmarshal(data, &s); err != nil {
		ccEnv = map[string]string{}
		return
	}

	ccEnv = s.Env
	if ccEnv == nil {
		ccEnv = map[string]string{}
	}

	if len(ccEnv) > 0 {
		logx.Info("loaded %d env vars from %s", len(ccEnv), settingsPath)
	}
}

// settingsPath returns the path to ~/.claude/settings.json.
func ccSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// CCOverride temporarily overrides env vars in ~/.claude/settings.json
// so Claude Code picks up dscli's values instead of what's in settings.
// If overrideStatusline is true, also replaces statusLine.command with hi statusline.
// Returns a restore function.
//
// Multiple agents: uses a reference count file (.hi-refcount) so that only
// the LAST agent to exit restores the original settings.json. The first agent
// creates the backup; subsequent agents share it.
func CCOverride(vars map[string]string, overrideStatusline bool) (restore func(), _ error) {
	sp, err := ccSettingsPath()
	if err != nil {
		return func() {}, err
	}

	bakPath := sp + ".hi-backup"
	refPath := sp + ".hi-refcount"

	// Step 0: if a stale backup exists and NO agents are active (refcount=0),
	// a previous run was kill -9'd. Restore settings.json before proceeding.
	// If refcount > 0, other agents ARE running — the backup is legitimate.
	if readRefCount(refPath) == 0 {
		if err := recoverFromBackup(sp, bakPath, refPath); err != nil {
			logx.Warn("failed to recover from stale backup: %v", err)
		}
	}

	// Reference counting: only the first agent creates the backup,
	// only the last agent restores it.
	// Lock-protected to avoid races between concurrent hi cc processes.
	refCount := atomicRefCount(refPath, +1)
	logx.Debug("CCOverride refcount: %d", refCount+1)

	data, err := os.ReadFile(sp)
	if err != nil {
		atomicRefCount(refPath, -1)
		return func() {}, err
	}

	// Strip UTF-8 BOM if present (PowerShell Set-Content -Encoding UTF8 adds one).
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}

	// Deep copy for backup.
	backup := make([]byte, len(data))
	copy(backup, data)

	// Unmarshal the full document (preserving unknown fields).

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		atomicRefCount(refPath, -1)
		return func() {}, err
	}

	envRaw, ok := doc["env"]
	if !ok {
		doc["env"] = map[string]any{}
		envRaw = doc["env"]
	}

	envMap, ok := envRaw.(map[string]any)
	if !ok {
		atomicRefCount(refPath, -1)
		return func() {}, err
	}

	for k, v := range vars {
		envMap[k] = v
	}

	// If the user already configured a statusLine command, save the original
	// and replace with hi statusline to enable model-name live updates.
	// Preserve original when overrideStatusline is false.
	if overrideStatusline {
		if sl, ok := doc["statusLine"].(map[string]any); ok {
			if cmd, ok := sl["command"].(string); ok && cmd != "" {
				// Only save the original command if it is not already
				// hi statusline (a later agent joining an already-patched
				// session would otherwise overwrite the real original).
				if !strings.Contains(cmd, "hi statusline") {
					setOriginalStatusCommand(cmd)
				}

				// Find or install a stable hi binary.
				home, _ := os.UserHomeDir()
				curExe, _ := os.Executable()
				var candidates []string
				if runtime.GOOS == "windows" {
					appData := os.Getenv("LOCALAPPDATA")
					if appData == "" && home != "" {
						appData = filepath.Join(home, "AppData", "Local")
					}
					if appData != "" {
						candidates = append(candidates, filepath.Join(appData, "hi", "hi.exe"))
					}
					candidates = append(candidates, filepath.Join(home, "hi", "hi.exe"))
				} else {
					candidates = []string{"/usr/local/bin/hi"}
					if home != "" {
						candidates = append(candidates, filepath.Join(home, ".local", "bin", "hi"))
					}
				}
				candidates = append(candidates, curExe)

				targetExe := curExe
				for _, dest := range candidates[:len(candidates)-1] {
					if _, err := os.Stat(dest); err == nil {
						targetExe = dest
						break
					}
					if data, err := os.ReadFile(curExe); err == nil {
						dir := filepath.Dir(dest)
						os.MkdirAll(dir, 0755)
						perm := os.FileMode(0755)
						if runtime.GOOS == "windows" {
							perm = 0644
						}
						writeErr := os.WriteFile(dest, data, perm)
						if writeErr == nil {
							logx.Info("installed hi to %s", dest)
							targetExe = dest
							break
						}
						logx.Debug("auto-install to %s failed: %v", dest, writeErr)
					}
				}

				doc["statusLine"] = map[string]any{
					"type":            "command",
					"command":         targetExe + " statusline",
					"refreshInterval": 120,
				}
				logx.Info("statusline: %s → hi statusline (model live update)", cmd)
			}
		}
	}

	newData, err := json.MarshalIndent(doc, "", "    ")
	if err != nil {
		return func() {}, err
	}

	if err := os.WriteFile(sp, newData, 0600); err != nil {
		return func() {}, err
	}

	logx.Info("patched ~/.claude/settings.json env: %d vars overwritten", len(vars))

	// Write backup to disk so we can recover if kill -9'd.
	// Only the first agent creates the backup (refcount 0→1, so new value is 1).
	if refCount == 1 {
		os.WriteFile(bakPath, backup, 0600)
		logx.Info("CCOverride: created backup (%d agent(s) active)", refCount)
	} else {
		logx.Info("CCOverride: joined (%d agent(s) active)", refCount)
	}

	restore = func() {
		remaining := atomicRefCount(refPath, -1)
		logx.Info("CCOverride restore: %d agent(s) remaining", remaining)
		if remaining > 0 {
			return
		}
		// Last agent: restore original settings.json from backup FILE.
		// (Not from in-memory 'backup' — later agents captured the already-patched version.)
		if orig, err := os.ReadFile(bakPath); err == nil {
			if err := os.WriteFile(sp, orig, 0600); err != nil {
				logx.Warn("failed to restore ~/.claude/settings.json: %v", err)
			} else {
				logx.Info("restored ~/.claude/settings.json")
			}
		} else {
			logx.Warn("failed to read backup for restore: %v", err)
		}
		os.Remove(bakPath)
		os.Remove(refPath)
	}
	return restore, nil
}

// readRefCount reads the reference count without locking (for crash detection only).
func readRefCount(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, b := range data {
		if b >= '0' && b <= '9' {
			n = n*10 + int(b-'0')
		}
	}
	return n
}

// atomicRefCount atomically adjusts the reference count by delta (+1 or -1)
// and returns the NEW value. Uses flock to prevent races between concurrent
// hi cc processes.
func atomicRefCount(path string, delta int) int {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return 0
	}
	defer f.Close()

	// Exclusive lock.
	if err := flockLock(f); err != nil {
		return 0
	}
	defer flockUnlock(f)

	// Read current value.
	data, _ := io.ReadAll(f)
	n := 0
	for _, b := range data {
		if b >= '0' && b <= '9' {
			n = n*10 + int(b-'0')
		}
	}

	// Adjust.
	n += delta
	if n < 0 {
		n = 0
	}

	if n <= 0 {
		f.Close()
		os.Remove(path)
		return 0
	}

	// Write new value.
	f.Truncate(0)
	f.Seek(0, 0)
	fmt.Fprintf(f, "%d\n", n)
	return n
}

// recoverFromBackup checks if a stale backup exists (meaning the last run
// was kill -9'd without restoring). If so, restores settings.json from it.
func recoverFromBackup(sp, bakPath, refPath string) error {
	data, err := os.ReadFile(bakPath)
	if err != nil {
		return nil // no backup, nothing to recover
	}
	if err := os.WriteFile(sp, data, 0600); err != nil {
		return err
	}
	os.Remove(bakPath)
	os.Remove(refPath) // stale refcount too
	logx.Info("recovered ~/.claude/settings.json from backup (previous run was interrupted)")
	return nil
}
