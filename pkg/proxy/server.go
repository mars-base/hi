package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mars-base/hi/pkg/config"
	"github.com/mars-base/hi/pkg/logx"
)

// Canonical Claude model names for each tier — used by reverse model lookup.
// Iteration order matters: sonnet first because it is the most common default.
// When multiple tiers map to the same backend model (e.g. deepseek opus AND sonnet
// both → "deepseek-v4-pro"), the first match wins the reverse mapping.
var tierToClaudeModel = [][2]string{
	{"sonnet", "claude-sonnet-4-6"},
	{"opus", "claude-opus-5"},
	{"haiku", "claude-haiku-4-5-20251001"},
}

// blockedDomains lists hostnames that Claude Code connects to for telemetry,
// error reporting, plugin downloads, and other non-essential traffic.
// Sourced from https://code.claude.com/docs/en/network-config and
// https://code.claude.com/docs/en/data-usage.
//
// Entries without a leading dot require exact match.
// Entries with a leading dot (e.g. ".anthropic.com") match the domain and all subdomains.
var blockedDomains = []string{
	// Anthropic / Claude branded domains — strongly associated, no other purpose
	"api.anthropic.com",       // metrics, telemetry, WebFetch safety check
	"a-api.anthropic.com",     // analytics API
	"a-cdn.anthropic.com",     // analytics CDN
	".claude.ai",              // *.claude.ai (downloads, api, etc.)
	".claudeusercontent.com",  // *.claudeusercontent.com (artifact viewing, bridge)
	".claude.com",             // *.claude.com (code, console, etc.)
}

// matchBlocked returns the matching domain rule if hostname is blocked, empty string otherwise.
func matchBlocked(hostname string) string {
	hostname = strings.ToLower(hostname)
	for _, domain := range blockedDomains {
		if domain[0] == '.' {
			if strings.HasSuffix(hostname, domain) || hostname == domain[1:] {
				return domain
			}
		} else {
			if hostname == domain {
				return domain
			}
		}
	}
	return ""
}

// ProxyState holds the running proxy's mutable state.
type ProxyState struct {
	mu sync.RWMutex

	// active is the name of the currently active backend.
	active string

	// backends holds all registered backends by name.
	backends map[string]Backend

	// reverseModelMap maps backend-specific model names back to canonical Claude model names.
	// E.g. "deepseek-v4-pro" → "claude-sonnet-4-6", enabling cross-backend model remapping
	// when Claude Code reuses a previous backend's model name in subsequent requests.
	reverseModelMap map[string]string

	// fallback is the Anthropic fallback URL for non-model requests.
	fallback *url.URL

	// costTracker tracks token usage and costs per backend.
	costTracker *CostTracker

	// hadNonAnthropic tracks whether we've used a non-Anthropic backend.
	hadNonAnthropic atomic.Bool

	// startTime records when the proxy started.
	startTime time.Time

	// reqCount counts total proxied requests.
	reqCount atomic.Uint64
}

// NewProxyState creates a new ProxyState from config.
func NewProxyState(cfg *config.Config) (*ProxyState, error) {
	backends := make(map[string]Backend, len(cfg.Backends))
	for name, bc := range cfg.Backends {
		b, err := NewBackend(name, bc)
		if err != nil {
			return nil, fmt.Errorf("failed to create backend %s: %w", name, err)
		}
		backends[name] = b
	}

	if _, ok := backends[cfg.ActiveBackend]; !ok {
		return nil, fmt.Errorf("active backend %q not found in config", cfg.ActiveBackend)
	}

	fallback, _ := url.Parse("https://api.anthropic.com")

	// Build reverse model map: backend model name → canonical Claude model name.
	// This enables cross-backend remapping when Claude Code reuses a stale
	// model name (e.g. "deepseek-v4-pro") after a backend switch.
	// Process tiers in priority order (sonnet > opus > haiku) to handle cases
	// where multiple tiers map to the same backend model.
	reverseModelMap := make(map[string]string)
	for _, entry := range tierToClaudeModel {
		tier, claudeModel := entry[0], entry[1]
		for _, b := range backends {
			info := b.ModelInfo()
			if modelName, ok := info[tier]; ok {
				// Only set if not already present — earlier tiers take priority.
				if _, exists := reverseModelMap[modelName]; !exists {
					reverseModelMap[modelName] = claudeModel
				}
			}
		}
	}

	ps := &ProxyState{
		active:          cfg.ActiveBackend,
		backends:        backends,
		reverseModelMap: reverseModelMap,
		fallback:        fallback,
		costTracker:     NewCostTracker(cfg.GetPricing()),
		startTime:       time.Now(),
	}
	// Seed request counter from persisted cost data so it survives restarts.
	ps.reqCount.Store(uint64(ps.costTracker.TotalRequests()))

	return ps, nil
}

// ActiveBackend returns the currently active backend.
func (ps *ProxyState) ActiveBackend() Backend {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.backends[ps.active]
}

// ActiveName returns the currently active backend name.
func (ps *ProxyState) ActiveName() string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.active
}

// SwitchBackend switches to the named backend. Returns the previous name.
func (ps *ProxyState) SwitchBackend(name string) (string, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if name == "anthropic" {
		prev := ps.active
		ps.active = "claude"
		return prev, nil
	}

	b, ok := ps.backends[name]
	if !ok {
		return "", fmt.Errorf("unknown backend: %s (valid: %s)", name, ps.backendNames())
	}

	prev := ps.active
	ps.active = name

	if b.Name() != "claude" {
		ps.hadNonAnthropic.Store(true)
	}

	logx.Info("Backend switched: %s → %s", prev, name)
	return prev, nil
}

// HadNonAnthropic returns true if any non-Claude backend has been used.
func (ps *ProxyState) HadNonAnthropic() bool {
	return ps.hadNonAnthropic.Load()
}

// Uptime returns the duration since the proxy started.
func (ps *ProxyState) Uptime() time.Duration {
	return time.Since(ps.startTime)
}

// RequestCount returns the total number of proxied requests.
func (ps *ProxyState) RequestCount() uint64 {
	return ps.reqCount.Load()
}

func (ps *ProxyState) incrRequestCount() {
	ps.reqCount.Add(1)
}

func (ps *ProxyState) backendNames() string {
	names := make([]string, 0, len(ps.backends))
	for n := range ps.backends {
		names = append(names, n)
	}
	return stringsJoin(names, ", ")
}

// Close shuts down the cost tracker and flushes final data to disk.
func (ps *ProxyState) Close() {
	ps.costTracker.Close()
}

// handleConnect handles CONNECT method requests (HTTPS tunneling).
// When HTTPS_PROXY is set, Claude Code's direct HTTPS connections
// (telemetry, MCP registry to api.anthropic.com) arrive here.
// We block connections to domains in blockedDomains to prevent data leakage.
func (ps *ProxyState) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	// Remove port if present for comparison.
	hostname := host
	if colonIdx := strings.LastIndex(host, ":"); colonIdx > 0 {
		hostname = host[:colonIdx]
	}

	ps.incrRequestCount()
	logx.Info("#%d CONNECT %s", ps.RequestCount(), host)

	// Block direct connections to blacklisted domains.
	// These are typically telemetry, error reporting, or plugin downloads that bypass
	// the ANTHROPIC_BASE_URL. Model calls already route through our
	// backend configuration, so blocking this is safe.
	if matched := matchBlocked(hostname); matched != "" {
		userAgent := r.Header.Get("User-Agent")
		logx.Warn("#%d BLOCKED CONNECT %s → rule=[%s] ua=%s", ps.RequestCount(), host, matched, userAgent)
		http.Error(w, fmt.Sprintf("Connections to %s are blocked by hi proxy", hostname), http.StatusForbidden)
		return
	}

	// For other HTTPS destinations (e.g. web search), tunnel the connection.
	// Establish a TCP tunnel to the target host.
	targetConn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		logx.Error("#%d CONNECT tunnel failed for %s: %v", ps.RequestCount(), host, err)
		http.Error(w, "Failed to establish tunnel", http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	// Hijack the client connection to get raw TCP access.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		logx.Error("#%d CONNECT hijack not supported", ps.RequestCount())
		http.Error(w, "Hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		logx.Error("#%d CONNECT hijack failed: %v", ps.RequestCount(), err)
		return
	}
	defer clientConn.Close()

	// Send 200 Connection Established to the client.
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Bidirectional copy between client and target.
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(targetConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, targetConn)
		done <- struct{}{}
	}()
	<-done
}

func stringsJoin(s []string, sep string) string {
	return strings.Join(s, sep)
}

// ServeHTTP handles incoming HTTP requests — routing model calls to the
// active backend and everything else to Anthropic.
func (ps *ProxyState) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Handle CONNECT method (HTTPS tunneling via HTTPS_PROXY).
	// Claude Code may make direct HTTPS connections to api.anthropic.com
	// (telemetry, MCP registry) bypassing ANTHROPIC_BASE_URL. When
	// HTTPS_PROXY is set, these arrive as CONNECT requests.
	if r.Method == http.MethodConnect {
		ps.handleConnect(w, r)
		return
	}

	// Handle control endpoints.
	if stringsHasPrefix(path, "/_proxy/") {
		ps.handleControl(w, r)
		return
	}

	ps.incrRequestCount()

	// Log every request URL (full) for visibility.
	fullURL := fmt.Sprintf("http://%s%s", r.Host, path)
	if r.URL.RawQuery != "" {
		fullURL = fmt.Sprintf("%s?%s", fullURL, r.URL.RawQuery)
	}
	logx.Info("#%d %s %s", ps.RequestCount(), r.Method, fullURL)

	// Detect and block any request targeting blacklisted domains directly
	// (HTTP mode, e.g. Host header). CONNECT method is handled earlier.
	hostOnly := r.Host
	if colonIdx := strings.LastIndex(hostOnly, ":"); colonIdx > 0 {
		hostOnly = hostOnly[:colonIdx]
	}
	if matched := matchBlocked(hostOnly); matched != "" {
		blockedURL := fmt.Sprintf("http://%s%s", r.Host, path)
		if r.URL.RawQuery != "" {
			blockedURL = fmt.Sprintf("%s?%s", blockedURL, r.URL.RawQuery)
		}
		logx.Warn("#%d BLOCKED %s %s → rule=[%s] ua=%s", ps.RequestCount(), r.Method, blockedURL, matched, r.Header.Get("User-Agent"))
		http.Error(w, fmt.Sprintf("Access to %s is blocked by hi proxy", hostOnly), http.StatusForbidden)
		return
	}

	isModel := IsModelRequest(path)
	activeName := ps.ActiveName()
	backend := ps.ActiveBackend()

	// Route ALL requests through the active backend.
	// This ensures /login, auth validation etc. also hit the same upstream.
	// Model-specific transforms (name remap, thinking strip) are only applied
	// when isModel is true in transformRequestBody.
	dest := backend.TargetURL()
	authBackend := backend

	// Build the upstream URL.
	upstreamURL := *dest
	upstreamURL.Path = joinURLPath(dest.Path, path)
	upstreamURL.RawQuery = r.URL.RawQuery

	// Read the request body.
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	// Transform the request body if needed.
	transformed, originalModel := ps.transformRequestBody(bodyBytes, isModel, activeName, backend)

	// Create the upstream request.
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), nil)
	if err != nil {
		http.Error(w, "Failed to create upstream request", http.StatusInternalServerError)
		return
	}

	// Copy headers from the original request.
	for key, values := range r.Header {
		for _, v := range values {
			upstreamReq.Header.Add(key, v)
		}
	}
	cleanRequestHeaders(upstreamReq.Header)
	upstreamReq.Header.Set("Host", dest.Host)

	// Strip original auth and inject backend auth for all requests.
	// Everything routes through the active backend now.
	upstreamReq.Header.Del("Authorization")
	upstreamReq.Header.Del("x-api-key")
	if authBackend != nil {
		authBackend.SetAuth(upstreamReq)
	}

	// Set the request body.
	upstreamReq.Body = io.NopCloser(strings.NewReader(string(transformed)))
	upstreamReq.ContentLength = int64(len(transformed))
	upstreamReq.Header.Set("Content-Length", strconv.Itoa(len(transformed)))

	startTime := time.Now()

	resp, err := newTransport().RoundTrip(upstreamReq)
	if err != nil {
		logx.Error("Upstream error for %s: %v", activeName, err)
		http.Error(w, `{"error":{"message":"upstream connection error"}}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	elapsed := time.Since(startTime)
	logx.Info("#%d %s %s %s %d %s", ps.RequestCount(), r.Method, backend.Name(), path, resp.StatusCode, elapsed.Round(time.Millisecond))
	logx.Debug("  -> upstream: %s %s", r.Method, upstreamURL.String())
	logx.Debug("  <- status=%d content-type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))

	// Log request body on non-2xx responses for diagnosis.
	if resp.StatusCode >= 400 {
		bodyStr := string(transformed)
		if len(bodyStr) > 2000 {
			head := bodyStr[:500]
			tail := bodyStr[len(bodyStr)-1000:]
			logx.Warn("  req body head (status=%d): %s", resp.StatusCode, head)
			logx.Warn("  req body tail (status=%d): %s", resp.StatusCode, tail)
		} else {
			logx.Warn("  req body (status=%d): %s", resp.StatusCode, bodyStr)
		}
	}

	// Process the response (SSE normalization, cost tracking, model name restoration).
	ps.processResponse(w, r, resp, backend, originalModel)
}

// StartServer starts the HTTP proxy server and blocks.
func StartServer(cfg *config.Config) error {
	state, err := NewProxyState(cfg)
	if err != nil {
		return fmt.Errorf("failed to create proxy state: %w", err)
	}
	defer state.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.ProxyPort)

	// Pre-acquire listener to detect port conflicts before printing startup banner.
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return fmt.Errorf("port %s already in use — another hi proxy is running. Use 'hi cc' or 'hi agent' to attach instead", addr)
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      state,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10 * time.Minute, // SSE streams can be long.
		IdleTimeout:  120 * time.Second,
	}

	logx.Info("Proxy listening on %s (active: %s)", addr, state.ActiveName())
	logx.Info("Control: curl -s http://%s/_proxy/status", addr)
	logx.Info("Switch:  curl -sX POST http://%s/_proxy/mode -d 'backend=deepseek'", addr)

	fmt.Printf("hi: Proxy started at http://%s (backend: %s)\n", addr, state.ActiveName())
	fmt.Printf("hi: Status:  curl -s http://%s/_proxy/status\n", addr)
	fmt.Printf("hi: Switch:  curl -sX POST http://%s/_proxy/mode -d 'backend=<name>'\n", addr)
	fmt.Println()

	// Graceful shutdown on SIGINT / SIGTERM.
	idleConns := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		signal.Stop(sigCh)
		logx.Info("Proxy shutting down...")
		srv.Shutdown(context.Background())
		close(idleConns)
	}()

	err = srv.Serve(ln)
	if err != nil && err != http.ErrServerClosed {
		if strings.Contains(err.Error(), "address already in use") {
			port := cfg.ProxyPort
			return fmt.Errorf("%s", portInUseMsg(port))
		}
		return err
	}
	<-idleConns
	return nil
}

// StartServerInBackground starts the proxy in a goroutine and returns
// a channel that receives any startup error, plus a shutdown function.
func StartServerInBackground(cfg *config.Config) (<-chan error, func(), error) {
	state, err := NewProxyState(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create proxy state: %w", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.ProxyPort)

	// Acquire the listener first so port-in-use is detected immediately,
	// before we risk a false-positive from polling another proxy's endpoint.
	ln, listenErr := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	ready := make(chan error, 1)

	if listenErr != nil {
		ready <- fmt.Errorf("%s", portInUseMsg(cfg.ProxyPort))
		shutdown := func() {}
		return ready, shutdown, nil
	}

	srv := &http.Server{
		Handler:      state,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		logx.Info("Proxy listening on %s (active: %s)", addr, state.ActiveName())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			ready <- err
		}
	}()
	go func() { ready <- nil }()

	shutdown := func() {
		state.Close()
		srv.Shutdown(context.Background())
	}

	return ready, shutdown, nil
}

func portInUseMsg(port int) string {
	return fmt.Sprintf("port :%d already in use — another hi proxy is running. Use 'hi cc' or 'hi agent' to attach instead", port)
}

func stringsHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func joinURLPath(base, rest string) string {
	// Strip trailing slash from base.
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	// Strip leading slash from rest.
	for len(rest) > 0 && rest[0] == '/' {
		rest = rest[1:]
	}
	return base + "/" + rest
}
