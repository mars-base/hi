#!/srv/venv/bin/python3
"""Probe the gpt-6-luna OpenAI Responses endpoint and dump wire formats.

NOT a load test. Sends a handful of representative requests to
POST {base_url}/v1/responses and records the exact request body and raw
response (JSON or SSE) so the Anthropic -> OpenAI Responses conversion layer
can be written and tested against real payloads.

Config is read from ~/.hi/config.yaml (backend entry, default: gpt-6-luna).
API keys of the form ${ENV_VAR} are resolved from the process environment,
then from ~/.claude/settings.json's env block.

Examples:
  /srv/venv/bin/python3 scripts/test_gpt6_luna.py
  /srv/venv/bin/python3 scripts/test_gpt6_luna.py --case text --case stream
  /srv/venv/bin/python3 scripts/test_gpt6_luna.py --raw --no-proxy
"""

import argparse
import collections
import json
import os
import sys
import time
import urllib.error
import urllib.request

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required: /srv/venv/bin/python3 -m pip install pyyaml")

CONFIG_PATH = "~/.hi/config.yaml"
CC_SETTINGS_PATH = "~/.claude/settings.json"


# ---------------------------------------------------------------- config

def load_backend(name):
    path = os.path.expanduser(CONFIG_PATH)
    with open(path) as f:
        cfg = yaml.safe_load(f) or {}
    bc = (cfg.get("backends") or {}).get(name)
    if not bc:
        sys.exit(f"backend {name!r} not found in {path}")
    return bc


def load_cc_env():
    try:
        with open(os.path.expanduser(CC_SETTINGS_PATH)) as f:
            return (json.load(f).get("env") or {})
    except Exception:
        return {}


def resolve_api_key(raw, cc_env):
    if raw and raw.startswith("${") and raw.endswith("}"):
        var = raw[2:-1]
        return os.environ.get(var) or cc_env.get(var) or ""
    return raw or ""


def mask(s, keep=4):
    if not s:
        return "<empty>"
    return "***" + s[-keep:] if len(s) > keep else "***"


def build_url(base_url):
    base = base_url.rstrip("/")
    if base.endswith("/v1"):
        return base + "/responses"
    return base + "/v1/responses"


# ---------------------------------------------------------------- HTTP

def make_opener(args):
    if args.no_proxy:
        return urllib.request.build_opener(urllib.request.ProxyHandler({}))
    proxy = args.proxy or os.environ.get("HTTPS_PROXY") or os.environ.get("https_proxy")
    if proxy:
        return urllib.request.build_opener(
            urllib.request.ProxyHandler({"http": proxy, "https": proxy})
        )
    return urllib.request.build_opener()


def send(opener, url, api_key, payload, stream, timeout):
    headers = {
        "Content-Type": "application/json",
        "Accept": "text/event-stream" if stream else "application/json",
        "Authorization": f"Bearer {api_key}",
        "Accept-Encoding": "identity",
    }
    data = json.dumps(payload, ensure_ascii=False).encode()
    req = urllib.request.Request(url, data=data, headers=headers, method="POST")
    try:
        resp = opener.open(req, timeout=timeout)
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "replace")
        return None, e, body
    return resp, None, None


# ---------------------------------------------------------------- SSE

def iter_sse(resp):
    """Yield (event_name, data_str, raw_line_count) from an SSE response."""
    event_name = None
    data_lines = []
    raw = []
    for raw_line in resp:
        line = raw_line.decode("utf-8", "replace").rstrip("\r\n")
        raw.append(line)
        if line == "":
            if data_lines:
                yield event_name, "\n".join(data_lines), raw
            event_name, data_lines, raw = None, [], []
        elif line.startswith("event:"):
            event_name = line[len("event:"):].strip()
        elif line.startswith("data:"):
            data_lines.append(line[len("data:"):].lstrip())
        elif line.startswith(":"):
            continue
    if data_lines:
        yield event_name, "\n".join(data_lines), raw


def preview(obj, raw_mode, limit=240):
    s = obj.get("delta") or obj.get("partial_json") or obj.get("text") or ""
    if isinstance(s, str) and s:
        if not raw_mode and len(s) > limit:
            s = s[:limit] + f"...({len(s)} chars)"
        return repr(s)
    item = obj.get("item") or {}
    if item:
        return f"item.type={item.get('type')} id={item.get('id')} call_id={item.get('call_id')}"
    return ""


def run_stream_case(opener, url, api_key, payload, args, out_dir, case):
    resp, err, err_body = send(opener, url, api_key, payload, True, args.timeout)
    if err is not None:
        print(f"  HTTP {err.code} {err.reason}")
        print(err_body[:4000])
        save(out_dir, case, payload, f"HTTP {err.code}\n{err_body}", [])
        return False

    print(f"  HTTP {resp.status} {resp.headers.get('Content-Type','')}")
    for h in ("openai-processing-ms", "x-request-id", "openai-organization"):
        if resp.headers.get(h):
            print(f"  {h}: {resp.headers.get(h)}")

    histogram = collections.Counter()
    completed = None
    raw_all = []
    events_jsonl = []
    n = 0
    print("  --- SSE events ---")
    for event_name, data_str, raw_lines in iter_sse(resp):
        raw_all.extend(raw_lines)
        n += 1
        try:
            obj = json.loads(data_str)
        except json.JSONDecodeError:
            obj = {}
        etype = obj.get("type") or event_name or "?"
        histogram[etype] += 1
        events_jsonl.append({"event": event_name, "data": obj})
        if etype in ("response.completed", "response.failed", "response.incomplete"):
            completed = obj
        extra = preview(obj, args.raw)
        print(f"  #{n:<3} {etype:<45} {extra}")
    resp.close()

    print("  --- event histogram ---")
    for etype, cnt in histogram.most_common():
        print(f"  {cnt:>4}  {etype}")
    if completed:
        r = completed.get("response") or {}
        print("  --- final response ---")
        print(f"  status={r.get('status')} model={r.get('model')} id={r.get('id')}")
        usage = r.get("usage") or {}
        print(f"  usage={json.dumps(usage, ensure_ascii=False)}")
        out_types = [(it.get("type"), it.get("status")) for it in (r.get("output") or [])]
        print(f"  output items={out_types}")
    save(out_dir, case, payload, "\n".join(raw_all), events_jsonl)
    return True


def run_json_case(opener, url, api_key, payload, args, out_dir, case):
    resp, err, err_body = send(opener, url, api_key, payload, False, args.timeout)
    if err is not None:
        print(f"  HTTP {err.code} {err.reason}")
        print(err_body[:4000])
        save(out_dir, case, payload, f"HTTP {err.code}\n{err_body}", [])
        return False
    body = resp.read().decode("utf-8", "replace")
    print(f"  HTTP {resp.status} {resp.headers.get('Content-Type','')}")
    try:
        obj = json.loads(body)
        print(json.dumps(obj, ensure_ascii=False, indent=2)[:8000])
    except json.JSONDecodeError:
        print(body[:8000])
    resp.close()
    save(out_dir, case, payload, body, [])
    return True


def save(out_dir, case, payload, raw, events):
    with open(os.path.join(out_dir, f"{case}.request.json"), "w") as f:
        json.dump(payload, f, ensure_ascii=False, indent=2)
    with open(os.path.join(out_dir, f"{case}.response.raw"), "w") as f:
        f.write(raw)
    if events:
        with open(os.path.join(out_dir, f"{case}.events.jsonl"), "w") as f:
            for ev in events:
                f.write(json.dumps(ev, ensure_ascii=False) + "\n")


# ---------------------------------------------------------------- cases

def build_cases(model, max_tokens, args):
    text_in = [{"role": "user", "content": [{"type": "input_text", "text": "Say hello in five words."}]}]
    tool = {
        "type": "function",
        "name": "get_weather",
        "description": "Get current weather for a city",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string", "description": "City name"}},
            "required": ["city"],
        },
    }
    return {
        "text": {
            "stream": False,
            "payload": {
                "model": model,
                "instructions": "You are a terse assistant.",
                "input": text_in,
                "max_output_tokens": max_tokens,
                "store": False,
            },
        },
        "stream": {
            "stream": True,
            "payload": {
                "model": model,
                "instructions": "You are a terse assistant.",
                "input": text_in,
                "max_output_tokens": max_tokens,
                "store": False,
                "stream": True,
            },
        },
        "history": {
            "stream": True,
            "payload": {
                "model": model,
                "input": [
                    {"role": "user", "content": "What is 2+2?"},
                    {"role": "assistant", "content": "4"},
                    {"role": "user", "content": "And 3+3? Answer in one word."},
                ],
                "max_output_tokens": max_tokens,
                "store": False,
                "stream": True,
            },
        },
        "tool": {
            "stream": True,
            "payload": {
                "model": model,
                "input": "What's the weather in Beijing right now? Use the get_weather tool.",
                "tools": [tool],
                "tool_choice": "auto",
                "max_output_tokens": max_tokens,
                "store": False,
                "stream": True,
            },
        },
        "tool-loop": {
            "stream": True,
            "payload": {
                "model": model,
                "input": [
                    {"role": "user", "content": "What's the weather in Beijing?"},
                    {
                        "type": "function_call",
                        "call_id": "call_probe_1",
                        "name": "get_weather",
                        "arguments": "{\"city\":\"Beijing\"}",
                    },
                    {
                        "type": "function_call_output",
                        "call_id": "call_probe_1",
                        "output": "{\"temperature\":\"21C\",\"condition\":\"sunny\"}",
                    },
                ],
                "tools": [tool],
                "max_output_tokens": max_tokens,
                "store": False,
                "stream": True,
            },
        },
        "reasoning": {
            "stream": True,
            "payload": {
                "model": model,
                "input": "How many r's are in the word strawberry? Answer in one sentence.",
                "reasoning": {"effort": args.effort, "summary": "auto"},
                "max_output_tokens": max_tokens,
                "store": False,
                "stream": True,
            },
        },
        "truncate": {
            "stream": True,
            "payload": {
                "model": model,
                "input": "Write a 100-word story about a cat.",
                "max_output_tokens": 16,
                "store": False,
                "stream": True,
            },
        },
        "error": {
            "stream": False,
            "payload": {
                "model": "__no_such_model__",
                "input": "hi",
                "max_output_tokens": 16,
                "store": False,
            },
        },
    }


ALL_CASES = ["text", "stream", "history", "tool", "tool-loop", "reasoning", "truncate", "error"]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--backend", default="gpt-6-luna", help="backend name in ~/.hi/config.yaml")
    ap.add_argument("--case", action="append", choices=ALL_CASES, help="case to run (repeatable, default: all)")
    ap.add_argument("--model", help="override model id")
    ap.add_argument("--max-tokens", type=int, default=256)
    ap.add_argument("--effort", default="low", help="reasoning.effort for the reasoning case")
    ap.add_argument("--timeout", type=int, default=120)
    ap.add_argument("--raw", action="store_true", help="do not truncate delta previews")
    ap.add_argument("--out-dir", help="artifact dir (default: /tmp/hi-gpt6-luna-probe-<ts>)")
    ap.add_argument("--proxy", help="HTTP(S) proxy URL (default: HTTPS_PROXY env)")
    ap.add_argument("--no-proxy", action="store_true")
    args = ap.parse_args()

    bc = load_backend(args.backend)
    api_key = resolve_api_key(bc.get("api_key", ""), load_cc_env())
    if not api_key:
        sys.exit(f"no api key resolved for backend {args.backend!r}")
    model = args.model or (bc.get("models") or {}).get("sonnet") or args.backend
    url = build_url(bc.get("base_url") or "https://api.openai.com")

    out_dir = args.out_dir or f"/tmp/hi-gpt6-luna-probe-{time.strftime('%Y%m%d-%H%M%S')}"
    os.makedirs(out_dir, exist_ok=True)

    print(f"backend : {args.backend}  (type={bc.get('type')})")
    print(f"url     : {url}")
    print(f"model   : {model}")
    print(f"key     : {mask(api_key)}")
    print(f"artifacts: {out_dir}")

    opener = make_opener(args)
    cases = build_cases(model, args.max_tokens, args)
    selected = args.case or ALL_CASES

    for name in selected:
        c = cases[name]
        print(f"\n=== case: {name} (stream={c['stream']}) ===")
        print("  --- request body ---")
        print("  " + json.dumps(c["payload"], ensure_ascii=False, indent=2).replace("\n", "\n  "))
        ok = (run_stream_case if c["stream"] else run_json_case)(
            opener, url, api_key, c["payload"], args, out_dir, name
        )
        print(f"  result: {'ok' if ok else 'FAILED'}")
        print(f"  saved : {out_dir}/{name}.*")

    print(f"\ndone. artifacts in {out_dir}")


if __name__ == "__main__":
    main()
