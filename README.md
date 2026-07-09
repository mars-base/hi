[English](README.md) | [中文](README_CN.md)

# hi

**Transparent multi-backend proxy for Claude Code — simple, fast, reliable, and
elastic.** No Claude Code modifications, no forks, no plugins. hi sits between
Claude Code and the network — intercepting only API calls — and restores
everything on exit. Your Claude Code stays vanilla.

**Privacy baked in.** hi proxy intercepts **all** network traffic from Claude Code
by injecting `HTTP_PROXY`/`HTTPS_PROXY` into the subprocess. Any telemetry,
analytics, or suspicious backdoor requests targeting Anthropic/Claude-branded
domains are blocked at the tunnel level — before any data leaves your machine.
Blocked domains include `api.anthropic.com` (telemetry, metrics), `a-api.anthropic.com`
(analytics), `a-cdn.anthropic.com` (analytics CDN), `*.claude.ai` (downloads, API),
`*.claudeusercontent.com` (artifact viewing, Chrome extension bridge), and
`*.claude.com` (code, console).
Legitimate non-Anthropic destinations (web search, MCP servers, etc.) tunnel
through normally.

> ⭐ **If you like hi, please give us a star on [GitHub](https://github.com/mars-base/hi)!**

Connect to **any** Anthropic‑compatible API. Officially supports **Claude**,
**DeepSeek**, **MiniMax**, **GLM**, **Kimi (K2.6 / K2.7‑code)**, **Qwen**, **MIMO**,
OpenRouter, internal gateways, and custom endpoints. Hot‑switch between all of
them without restarting your session.

The name comes from the first word you type into any Claude Code or AI agent
session — **hi**. The agent replies "Hello, how can I help you?" and the work
begins. Simple, memorable, and fitting for the AI era. May evolve if a better
name comes along.

### Why hi?

hi currently prioritizes **Claude Code** — arguably the best harness agent tool
available today. However, CC is also the most expensive. For everyday users
who rely on harness agents heavily but don't have a bottomless budget, the
cost can be prohibitive. DeepSeek, on the other hand, offers some of the most
affordable large models in the world. Pairing Claude Code with DeepSeek makes
for a fantastic combination: top-tier agent capabilities at a fraction of the
price.

Looking ahead, hi plans to support more agent tools and the OpenAI API
protocol, making it a universal multi-backend proxy.

## Quick start

### Prerequisites

- [Claude Code](https://code.claude.com/docs/en/quickstart#step-1-install-claude-code) installed and logged in
- **hi binary** — download from [Releases](https://github.com/mars-base/hi/releases) (recommended) or [build from source](https://github.com/mars-base/hi#building-from-source)
- **API key** — hi works with any Anthropic-compatible endpoint. You need an API key from one of:
  - [Anthropic Console](https://console.anthropic.com/) — Claude API key
  - [DeepSeek Platform](https://platform.deepseek.com/api_keys) — DeepSeek API key
  - Your organization's internal API gateway

| Feature | Status |
|---------|--------|
| Claude Code version | Tested up to **2.1.172** |
| Hot-switch backends | ✅ Claude, DeepSeek, and all custom backends |
| Web Search | ✅ Works on all supported models |

### Install (Linux / macOS)

```bash
curl -fsSL https://raw.githubusercontent.com/mars-base/hi/main/scripts/install.sh | sh
```

This downloads the latest release binary and installs it to `/usr/local/bin/hi`
(or `~/.local/bin/hi` if `/usr/local/bin` requires `sudo`).

### Install (Windows)

**PowerShell:**

```powershell
irm https://raw.githubusercontent.com/mars-base/hi/main/scripts/install-windows.ps1 | iex
```

**CMD:**

```batch
curl -fsSL https://raw.githubusercontent.com/mars-base/hi/main/scripts/install-windows.cmd -o install.cmd && install.cmd && del install.cmd
```

Installs to `%USERPROFILE%\.local\bin\hi.exe` and adds the directory to your
user PATH. Restart your terminal after installation.

### Run

```bash
# 1. Initialize configuration (auto-detects settings.json)
hi init-config

# 2. Edit config (Linux/macOS: ~/.hi/config.yaml, Windows: %USERPROFILE%/.hi/config.yaml)
#    Or use the sample template with all providers pre-configured:
#      cp config.sample.yaml ~/.hi/config.yaml
#    Then fill in your API keys.

# 3. Launch proxy + Claude Code
hi

# Or with inline API keys (no config file needed)
ANTHROPIC_API_KEY=sk-xxx DEEPSEEK_API_KEY=sk-xxx hi

# Windows PowerShell
$env:ANTHROPIC_API_KEY="sk-xxx"; $env:DEEPSEEK_API_KEY="sk-xxx"; hi

# Windows CMD
set ANTHROPIC_API_KEY=sk-xxx && set DEEPSEEK_API_KEY=sk-xxx && hi

# Attach additional agents to an already-running proxy
hi cc

# Same as above, explicit
hi launch --backend deepseek

# Proxy only (no Claude Code, no settings.json patch)
hi proxy --log-file /tmp/hi.log

# Show config and status
hi status
```

### Command reference

| Command | Proxy | Claude Code | settings.json patch | Slash commands | Use case |
|------|--------|--------|---------|--------|------|
| `hi` | ✅ | ✅ | ✅ | ✅ | Full workflow (recommended) |
| `hi launch` | ✅ | ✅ | ✅ | ✅ | Same as bare command |
| `hi proxy` | ✅ | ❌ | ❌ | ✅ | Proxy only |
| `hi agent` / `hi cc` | ❌ | ✅ | ✅ | ✅ | Attach to existing proxy |
| `hi status` | ❌ | ❌ | ❌ | ❌ | Show config |
| `hi statusline` | ❌ | ❌ | ❌ | ❌ | Claude Code status bar |

### CLI options

| Option | Default | Description |
|------|--------|------|
| ``-b, --backend <name>`` | `claude` | Backend: `claude` / `deepseek` |
| ``--log-file <path>`` | `~/.hi/logs/hi.log` | Log file path (auto-rotated by date) |
| ``--log-level <level>`` | `info` | Log level: `debug` / `info` / `warn` / `error` |
| `--preserve-statusline` | — | Keep existing statusLine command (don't replace with hi) |

Log files are automatically rotated by date. The filename is decorated
with today's date before the extension:

```
~/.hi/logs/hi.log  →  ~/.hi/logs/hi-2026-06-05.log
/tmp/hi.log      →  /tmp/hi-2026-06-05.log
```

At midnight the logger switches to a new file automatically — no signal
or restart needed. Old files are never deleted; manage retention manually
or with a cron job.

### Watch logs

```bash
tail -f /tmp/hi-$(date +%F).log | grep -E "#[0-9]|Control:|backend |env:"
```

All paths shown as `~/.hi/` resolve to your OS home directory
automatically:

| Platform | Path |
|------|------|
| Linux | `/home/user/.hi/` |
| macOS | `/Users/user/.hi/` |
| Windows | `C:\Users\user\.hi\` |

## Supported Models

hi works with any Anthropic‑compatible API. Here are the officially supported
providers and their endpoints:

| Provider | Type | Base URL | Official Guide |
|----------|------|----------|----------------|
| Anthropic (Claude) | `anthropic` | `https://api.anthropic.com` | — |
| DeepSeek | `deepseek` | `https://api.deepseek.com/anthropic` | — |
| MiniMax | `anthropic` | `https://api.minimax.io/anthropic` | [Docs](https://platform.minimax.io/docs/token-plan/claude-code) |
| GLM (z.ai) | `anthropic` | `https://api.z.ai/api/anthropic` | [Docs](https://docs.z.ai/devpack/tool/claude) |
| Kimi | `anthropic` | `https://api.kimi.com/coding/` | [Docs](https://www.kimi.com/code/docs/en/) |
| Qwen (Alibaba) | `anthropic` | `https://dashscope-intl.aliyuncs.com/apps/anthropic` | [Claude Code Guide](https://www.alibabacloud.com/help/en/model-studio/claude-code) |
| MIMO (Xiaomi) | `anthropic` | `https://api.xiaomimimo.com/anthropic` | [Docs](https://platform.xiaomimimo.com/docs/en-US/api/chat/anthropic-api) |

> **Kimi K2.7‑code** (256K context window) is supported as a `deepseek`‑type
> backend. Set `context_window: 256000` in your config for accurate statusline
> display.

> Any gateway or proxy that speaks the Anthropic API protocol (e.g. Leihuo,
> OpenRouter, OneAPI, internal gateways) also works — just set `type: anthropic`
> and the appropriate `base_url`.

## Configuration

First run of `hi status` auto-generates `~/.hi/config.yaml`:

```yaml
active_backend: deepseek
proxy_port: 18799

env:
  auto_compact_window: 200000       # CLAUDE_CODE_AUTO_COMPACT_WINDOW
  autocompact_pct_override: 64      # CLAUDE_AUTOCOMPACT_PCT_OVERRIDE (64% × 200K = 128K trigger)
  disable_nonessential_traffic: true  # CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC

backends:
  claude:
    type: anthropic
    base_url: https://api.anthropic.com
    api_key: "${ANTHROPIC_API_KEY}"
    strip_thinking: true
    pricing:
      input: 3.00
      output: 15.00
    models:
      opus: claude-opus-4-8
      sonnet: claude-sonnet-4-6
      haiku: claude-haiku-4-5-20251001

  deepseek:
    type: deepseek
    base_url: https://api.deepseek.com/anthropic
    api_key: "${DEEPSEEK_API_KEY}"
    strip_thinking: true     # remove top-level thinking config
    reasoning_effort: max    # optional: output_config.effort (deepseek only)
    pricing:
      input: 0.42
      output: 0.83
    models:
      opus: deepseek-v4-pro[1m]
      sonnet: deepseek-v4-pro[1m]
      haiku: deepseek-v4-flash[1m]
```

The `type: anthropic` backend works with any Anthropic‑compatible API —
OpenRouter, OneAPI, internal gateways, Azure, etc. Just update `base_url`
and `api_key`. See [Adding custom backends](#adding-custom-backends) for details.


## API key resolution priority

When hi expands `${VAR}` references in `~/.hi/config.yaml`, it checks:

1. OS environment variable (`os.Getenv`)
2. `~/.claude/settings.json` `env` block (fallback)
3. Non-`${...}` format — passed through as-is

> This is hi's own resolution order. Claude Code's startup priority is the
> reverse — `settings.json env > OS process env` — which is why hi must
> patch settings.json to redirect API traffic through the proxy.

Startup logs show the resolution source:

```
[hi] DEBUG env: ANTHROPIC_API_KEY=...KoKG (from OS env)
[hi] DEBUG env: ANTHROPIC_API_KEY=...KoKG (from ~/.claude/settings.json)
[hi] DEBUG env: DEEPSEEK_API_KEY=<not set>
```

### Model name mapping

Claude Code uses Anthropic native model names. hi auto-remaps them:

| Claude model | deepseek backend | claude backend |
|-------------|---------------|-------------|
| `claude-opus-4-8` | `deepseek-v4-pro` | `claude-opus-4-8` |
| `claude-sonnet-4-6` | `deepseek-v4-pro` | `claude-sonnet-4-6` |
| `claude-haiku-4-5-20251001` | `deepseek-v4-flash` | `claude-haiku-4-5-20251001` |

### Adding custom backends

You can add any Anthropic‑compatible API endpoint as a backend. Define it in
`~/.hi/config.yaml` under `backends`, then restart hi:

```yaml
backends:
  claude:
    type: anthropic
    base_url: https://api.anthropic.com
    api_key: "${ANTHROPIC_API_KEY}"
    pricing: { input: 3.00, output: 15.00 }
    models:
      opus: claude-opus-4-8
      sonnet: claude-sonnet-4-6
      haiku: claude-haiku-4-5-20251001

  deepseek:
    type: deepseek
    base_url: https://api.deepseek.com/anthropic
    api_key: "${DEEPSEEK_API_KEY}"
    pricing: { input: 0.42, output: 0.83 }
    models:
      opus: deepseek-v4-pro[1m]
      sonnet: deepseek-v4-pro[1m]
      haiku: deepseek-v4-flash[1m]

  # Example: custom backend via an internal gateway
  internal:
    type: anthropic
    base_url: https://llm.internal.example.com
    api_key: "${INTERNAL_API_KEY}"
    pricing: { input: 0.50, output: 1.00 }
    models:
      opus: claude-opus-4-8
      sonnet: claude-sonnet-4-6
      haiku: claude-haiku-4-5-20251001
```

Key points:
- `type: anthropic` — for Anthropic‑compatible API endpoints
- `type: deepseek` — strips thinking blocks before forwarding
- `api_key` — supports `${ENV_VAR}` expansion or literal keys
- `models.opus/sonnet/haiku` — maps Claude model names to backend‑specific IDs
- `pricing` — USD per 1M tokens, used by cost tracking
- `context_window` — max context window size in tokens for statusline display. Default: ``1000000`` for `deepseek`, ``200000`` for other types
- `reasoning_effort` — sets `output_config.effort` for deepseek backends: `max` / `high`. Empty to disable. Default: empty
- `env` — environment variables passed to Claude Code at launch:
  - `auto_compact_window` — auto-compact trigger window. Default: ``200000``
  - `autocompact_pct_override` — compact at this percentage of window. Default: ``64`` (128K trigger)
  - `disable_nonessential_traffic` — disable product telemetry. Default: ``true``

> **DeepSeek 1M context window**: for the official DeepSeek API, append
> ``[1m]`` to the model name to unlock the 1M‑token context window. Write
> ``deepseek-v4-pro[1m]`` and ``deepseek-v4-flash[1m]`` in the `models`
> block. If your API gateway rejects the ``[1m]`` suffix, just use the
> plain names (``deepseek-v4-pro``, ``deepseek-v4-flash``) instead.

## Hot-switching backends

Switch backends mid-session without restarting Claude Code. The switch only
affects the next API call.

From within Claude Code, use the auto-generated slash commands:

```
/deepseek   →  Switch to DeepSeek
/claude     →  Switch to Claude
```

Or manually via the control endpoint:

```bash
curl -sX POST http://127.0.0.1:18799/_proxy/mode -d 'backend=deepseek'
```

Or create a slash command `~/.claude/commands/deepseek.md`:

```markdown
Switch the proxy to DeepSeek. Run silently and report the result:
curl -sX POST http://127.0.0.1:18799/_proxy/mode -d 'backend=deepseek'
If HTTP 200, say "Switched to DeepSeek." Otherwise report the error.
```

Then type `/deepseek` in any Claude Code session to switch instantly.

### Switch log output

```
[hi] INFO  Control: backend switched deepseek → claude
[hi] INFO  New backend env:
[hi] INFO    ANTHROPIC_MODEL                = claude-sonnet-4-6
[hi] INFO    ANTHROPIC_DEFAULT_OPUS_MODEL   = claude-opus-4-8
[hi] INFO    ANTHROPIC_DEFAULT_SONNET_MODEL = claude-sonnet-4-6
[hi] INFO    ANTHROPIC_DEFAULT_HAIKU_MODEL  = claude-haiku-4-5-20251001
```

## Multi‑agent workflows

A single `hi proxy` instance can serve multiple Claude Code agents
simultaneously. Each agent's API calls go through the same proxy, sharing
cost tracking and backend switching.

There are two ways to run multiple agents against a single proxy:

#### Method 1: `hi proxy` + `hi cc` (recommended)

Start the proxy once, then attach agents with zero manual config:

```bash
# Terminal 1: start proxy
hi proxy

# Terminals 2+: attach agents
hi cc
hi cc --backend claude
hi cc
```

#### Method 2: `hi launch` + bare `claude`

`hi launch` patches `settings.json` on startup. Once patched, any bare
`claude` invocation picks up the proxy address automatically:

```bash
# Terminal 1: full launch (proxy + agent + settings patch)
hi launch --backend deepseek

# Terminals 2+: just claude — settings.json already points to proxy
claude
claude
```

**Caveat:** `hi launch` shuts down the proxy when its Claude Code exits,
killing the shared proxy for everyone. Use Method 1 when you need the proxy
to stay alive independently, or when launching agents in arbitrary order.

All agents share the same hot-switch endpoint — switching the backend via
`/_proxy/mode` affects every connected agent immediately. Cost tracking
aggregates across all sessions into a single `~/.hi/.cost.json`.

## Status line live model update

hi auto-discovers your existing `statusLine` configuration in
`~/.claude/settings.json` and replaces it with the built-in `hi statusline`
command. After a backend switch, the model name in the status bar updates
automatically (within 120s). No manual config needed.

Pass `--preserve-statusline` to skip this override and keep your existing
statusLine command untouched:

```bash
hi launch --preserve-statusline
hi cc --preserve-statusline
```

### How it works

```
hi launch
  ├── Discover settings.json statusLine.command
  ├── Save original to ~/.hi/.statusline-command
  └── Replace with hi statusline

Claude Code status bar refresh (every 120s)
  ├── Run hi statusline
  ├── Query proxy → get active backend model
  ├── Patch model field in stdin JSON
  └── Delegate to original script for cost, context, cache stats
```

### Effect

| Backend | Status bar shows |
|------|------|
| deepseek | `🤖 deepseek-v4-pro` |
| claude | `🤖 claude-sonnet-4-6` |

### Test manually

```bash
echo '{"model":{},"workspace":{},"context_window":{},"cost":{}}' | hi statusline
# 📁 tmp | 🤖 deepseek-v4-pro | 🧠 ctx:-- | 💰 $0.010
```

## Control endpoints

```bash
# Status
curl -s http://127.0.0.1:18799/_proxy/status | python3 -m json.tool
# {
#   "active_backend": "deepseek",
#   "backends": ["claude", "deepseek"],
#   "requests": 47,
#   "uptime_seconds": 3600
# }

# Cost tracking
curl -s http://127.0.0.1:18799/_proxy/cost | python3 -m json.tool
# {
#   "backends": {
#     "deepseek": {
#       "input_tokens": 23223,
#       "output_tokens": 166,
#       "cost": 0.0104,
#       "anthropic_equivalent": 0.0722
#     }
#   },
#   "total_cost": 0.0104,
#   "anthropic_equivalent": 0.0722,
#   "savings": 0.0618
# }

# Switch backend
curl -sX POST http://127.0.0.1:18799/_proxy/mode -d 'backend=deepseek'
# {"mode":"deepseek","previous":"claude"}
```

### Cost persistence

Cost data persists across proxy restarts. Token usage is accumulated in memory
and flushed to disk on a regular cadence:

| Trigger | Behavior |
|------|------|
| `Record()` per request | Memory-only update (no disk write) |
| Background goroutine | Flush to `~/.hi/.cost.json` every 30s |
| Proxy shutdown | Final flush via `Close()` |
| Next startup | Load cumulative data from file |

This avoids excessive disk I/O during heavy sub‑agent workloads while ensuring
data survives process restarts and system reboots.

```bash
# Check cumulative cost across all sessions
cat ~/.hi/.cost.json | python3 -m json.tool
```

## Debugging

### Log levels

| Level | Shows | Use case |
|------|------|----------|
| `debug` | Everything (env resolution, model remap, upstream URL) | Troubleshooting |
| `info` | Request trace, backend switch, cost stats (default) | Daily use |
| `warn` | Warnings + errors | Watch for issues |
| `error` | Errors only | Minimal noise |

### Request log examples (debug level)

```
[hi] INFO  15:07:03 #2 POST deepseek /v1/messages 200 634ms
[hi] INFO  15:07:03 #2 tokens: in=114 out=120
[hi] DEBUG 15:07:03   -> upstream: POST https://api.anthropic.com/v1/messages
[hi] DEBUG 15:07:03   <- status=200 content-type=application/json
[hi] DEBUG 15:07:03 Model remap: claude-sonnet-4-6 → deepseek-v4-pro
```

### hi status output

```
Config:  /home/fish/.hi.yaml
Claude:  detected at /home/fish/.claude/settings.json
Proxy:   http://127.0.0.1:18799
Active:  deepseek
Log:     /tmp/hi.log (level=info)
Backends:
    claude
  * deepseek
```

## Building from source

Requires [Go 1.21+](https://go.dev/dl/).

```bash
git clone https://github.com/mars-base/hi.git
cd hi
make build
make install
```

## Cost comparison

| Backend | Input/M | Output/M |
|------|--------|--------|
| Claude Opus | $3.00 | $15.00 |
| DeepSeek V4 | $0.42 | $0.83 |
| MiniMax M3 | $0.30 | $1.20 |
| GLM 5.1 | $0.959 | $3.836 |
| Kimi K2.6 | $0.89 | $3.74 |
| Kimi K2.7‑code | $0.89 | $3.699 |
| Qwen 3.7-Max | $0.822 | $2.466 |
| MIMO V2.5-Pro | $0.411 | $0.822 |

Heavy usage (25 days/month): as low as $15-30 vs $200 — 85-92% savings.

The proxy tracks and reports savings in real time:

```
total_cost: 0.0104              ← actual spend
anthropic_equivalent: 0.0722    ← what Anthropic would cost
savings: 0.0618                 ← 85.6% saved
```

## License

[PolyForm Noncommercial License 1.0.0](https://polyformproject.org/licenses/noncommercial/1.0.0/) — free for personal and non-commercial use. Commercial use requires explicit permission.
