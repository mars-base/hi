#!/srv/venv/bin/python3
"""Probe the native Gemini generateContent endpoint and dump wire formats.

NOT a load test. Sends one representative request per case to
POST {base_url}/v1beta/models/{model}:generateContent (or
:streamGenerateContent?alt=sse) and records the exact request and raw
response so the Anthropic <-> Gemini conversion layer can be checked against
real payloads.

Config is read from ~/.hi/config.yaml (backend entry, default:
gemini-4-hydrogen). API keys of the form ${ENV_VAR} are resolved from the
process environment, then from ~/.claude/settings.json's env block. Only a
masked key is ever printed.

Examples:
  /srv/venv/bin/python3 scripts/test_gemini4_native.py --no-proxy
  /srv/venv/bin/python3 scripts/test_gemini4_native.py --case text --case stream --no-proxy
  /srv/venv/bin/python3 scripts/test_gemini4_native.py --case tool-loop --raw --no-proxy
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required: /srv/venv/bin/python3 -m pip install pyyaml")

CONFIG_PATH = "~/.hi/config.yaml"
CC_SETTINGS_PATH = "~/.claude/settings.json"

TINY_PNG_B64 = (
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
)


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


# ---------------------------------------------------------------- HTTP

def endpoint(base_url, model, stream):
    base = base_url.rstrip("/")
    for suffix in ("/v1beta", "/v1"):
        if base.endswith(suffix):
            base = base[: -len(suffix)]
            break
    action = ":streamGenerateContent?alt=sse" if stream else ":generateContent"
    return base + "/v1beta/models/" + urllib.parse.quote(model, safe="") + action


def make_opener(args):
    if args.no_proxy:
        return urllib.request.build_opener(urllib.request.ProxyHandler({}))
    proxy = args.proxy or os.environ.get("HTTPS_PROXY") or os.environ.get("https_proxy")
    if proxy:
        return urllib.request.build_opener(
            urllib.request.ProxyHandler({"http": proxy, "https": proxy})
        )
    return urllib.request.build_opener()


def iter_sse(resp):
    """Yield (data_str, raw_lines) for data-only SSE frames."""
    data_lines = []
    raw = []
    for raw_line in resp:
        line = raw_line.decode("utf-8", "replace").rstrip("\r\n")
        raw.append(line)
        if line == "":
            if data_lines:
                yield "\n".join(data_lines), raw
            data_lines, raw = [], []
        elif line.startswith("data:"):
            data_lines.append(line[len("data:"):].lstrip())
    if data_lines:
        yield "\n".join(data_lines), raw


# ---------------------------------------------------------------- prober

class Prober:
    def __init__(self, opener, args, base_url, model, api_key, out_dir):
        self.opener = opener
        self.args = args
        self.base_url = base_url
        self.model = model
        self.api_key = api_key
        self.out_dir = out_dir
        self.tool_turn = None  # {"parts": [...], "calls": [...]} captured from a tool call

    def post(self, payload, case, stream, tag=""):
        url = endpoint(self.base_url, self.model, stream)
        headers = {
            "Content-Type": "application/json",
            "Accept": "text/event-stream" if stream else "application/json",
            "Accept-Encoding": "identity",
            "x-goog-api-key": self.api_key,
        }
        data = json.dumps(payload, ensure_ascii=False).encode()
        req = urllib.request.Request(url, data=data, headers=headers, method="POST")

        name = case + tag
        result = {"case": name, "url": url, "stream": stream, "status": None,
                  "content_type": "", "raw": "", "json": None, "chunks": []}
        try:
            resp = self.opener.open(req, timeout=self.args.timeout)
            result["status"] = resp.status
            result["content_type"] = resp.headers.get("Content-Type", "")
            if stream:
                raw_lines = []
                for data_str, lines in iter_sse(resp):
                    raw_lines.extend(lines)
                    try:
                        chunk = json.loads(data_str)
                    except json.JSONDecodeError:
                        chunk = {"_unparsable": data_str}
                    result["chunks"].append(chunk)
                result["raw"] = "\n".join(raw_lines)
            else:
                body = resp.read().decode("utf-8", "replace")
                result["raw"] = body
                try:
                    result["json"] = json.loads(body)
                except json.JSONDecodeError:
                    pass
            resp.close()
        except urllib.error.HTTPError as e:
            body = e.read().decode("utf-8", "replace")
            result["status"] = e.code
            result["raw"] = body
            try:
                result["json"] = json.loads(body)
            except json.JSONDecodeError:
                pass

        self.save(payload, result)
        return result

    def save(self, payload, result):
        name = result["case"]
        with open(os.path.join(self.out_dir, f"{name}.request.json"), "w") as f:
            json.dump({"url": result["url"], "payload": payload}, f, ensure_ascii=False, indent=2)
        with open(os.path.join(self.out_dir, f"{name}.response.raw"), "w") as f:
            f.write(result["raw"])
        if result["chunks"]:
            with open(os.path.join(self.out_dir, f"{name}.chunks.jsonl"), "w") as f:
                for chunk in result["chunks"]:
                    f.write(json.dumps(chunk, ensure_ascii=False) + "\n")

    # -- helpers -------------------------------------------------

    @staticmethod
    def iter_parts(obj):
        for cand in obj.get("candidates") or []:
            content = cand.get("content") or {}
            for part in content.get("parts") or []:
                yield part

    @staticmethod
    def preview_part(part):
        if part.get("thought"):
            return f"thought {part.get('text', '')[:80]!r}"
        if part.get("functionCall"):
            fc = part["functionCall"]
            sig = "sig+first12=" + part.get("thoughtSignature", "")[:12] if part.get("thoughtSignature") else "no-sig"
            return f"functionCall {fc.get('name')} args={json.dumps(fc.get('args'), ensure_ascii=False)} id={fc.get('id')} {sig}"
        if part.get("text") is not None:
            return f"text {part.get('text', '')[:120]!r}"
        if part.get("inlineData"):
            data = part["inlineData"].get("data", "")
            return f"inlineData {part['inlineData'].get('mimeType')} ({len(data)} b64 chars)"
        return json.dumps(part, ensure_ascii=False)[:160]

    def print_parts(self, obj):
        for part in self.iter_parts(obj):
            print("  part:", self.preview_part(part))

    def capture_tool_turn(self, result):
        """Remember a model tool turn (with signatures) for replay cases."""
        parts = []
        calls = []
        for part in self.iter_parts(result.get("json") or {}):
            if part.get("functionCall"):
                parts.append(part)
                calls.append(part["functionCall"])
            elif part.get("thoughtSignature"):
                parts.append(part)
        for chunk in result.get("chunks") or []:
            for part in self.iter_parts(chunk):
                if part.get("functionCall"):
                    parts.append(part)
                    calls.append(part["functionCall"])
                elif part.get("thoughtSignature"):
                    parts.append(part)
        if calls:
            self.tool_turn = {"parts": parts, "calls": calls}
        return self.tool_turn

    def ensure_tool_turn(self):
        if self.tool_turn is None:
            print("  (capturing a tool turn first)")
            self.probe_tool(quiet=True)
        return self.tool_turn

    # -- cases ---------------------------------------------------

    @staticmethod
    def tool_decl():
        return [{
            "functionDeclarations": [{
                "name": "get_weather",
                "description": "Get current weather for a city",
                "parametersJsonSchema": {
                    "type": "object",
                    "properties": {"city": {"type": "string", "description": "City name"}},
                    "required": ["city"],
                },
            }]
        }]

    def base_payload(self, contents, **extra):
        payload = {"contents": contents}
        payload.update(extra)
        return payload

    def case_text(self):
        payload = self.base_payload(
            [{"role": "user", "parts": [{"text": "Say hello in five words."}]}],
            systemInstruction={"parts": [{"text": "You are a terse assistant."}]},
            generationConfig={"maxOutputTokens": self.args.max_tokens},
        )
        r = self.post(payload, "text", stream=False)
        print(f"  HTTP {r['status']} {r['content_type']}")
        if r.get("json"):
            print("  finishReason:", json.dumps(
                [c.get("finishReason") for c in r["json"].get("candidates") or []]))
            print("  usage:", json.dumps(r["json"].get("usageMetadata") or {}))
            self.print_parts(r["json"])
        return r["status"] == 200

    def case_stream(self):
        payload = self.base_payload(
            [{"role": "user", "parts": [{"text": "Say hello in five words."}]}],
            systemInstruction={"parts": [{"text": "You are a terse assistant."}]},
            generationConfig={"maxOutputTokens": self.args.max_tokens},
        )
        r = self.post(payload, "stream", stream=True)
        print(f"  HTTP {r['status']} {r['content_type']} chunks={len(r['chunks'])}")
        for i, chunk in enumerate(r["chunks"]):
            for part in self.iter_parts(chunk):
                print(f"  #{i}", self.preview_part(part))
        return r["status"] == 200 and bool(r["chunks"])

    def case_stream_usage(self):
        payload = self.base_payload(
            [{"role": "user", "parts": [{"text": "Count from 1 to 5."}]}],
            generationConfig={"maxOutputTokens": 64},
        )
        r = self.post(payload, "stream-usage", stream=True)
        print(f"  HTTP {r['status']} chunks={len(r['chunks'])}")
        last = {}
        for i, chunk in enumerate(r["chunks"]):
            usage = chunk.get("usageMetadata") or {}
            if usage:
                last = usage
            finish = [c.get("finishReason") for c in chunk.get("candidates") or [] if c.get("finishReason")]
            print(f"  #{i} usage={json.dumps(usage)} finish={finish}")
        print("  final usage:", json.dumps(last))
        return r["status"] == 200 and bool(last)

    def case_max_tokens(self):
        payload = self.base_payload(
            [{"role": "user", "parts": [{"text": "Write a 100-word story about a cat."}]}],
            generationConfig={"maxOutputTokens": 16},
        )
        r = self.post(payload, "max-tokens", stream=False)
        print(f"  HTTP {r['status']}")
        if r.get("json"):
            self.print_parts(r["json"])
            print("  finishReason:", json.dumps(
                [c.get("finishReason") for c in r["json"].get("candidates") or []]))
        return r["status"] == 200

    def case_role_system(self):
        payload = self.base_payload([
            {"role": "user", "parts": [{"text": "hi"}]},
            {"role": "system", "parts": [{"text": "Tail reminder text."}]},
            {"role": "user", "parts": [{"text": "say ok"}]},
        ], generationConfig={"maxOutputTokens": 64})
        r = self.post(payload, "role-system", stream=False)
        print(f"  HTTP {r['status']} (probe: does a literal role=\"system\" content work?)")
        if r["status"] != 200:
            print("  error body:", r["raw"][:600])
        return True

    def probe_tool(self, quiet=False):
        payload = self.base_payload(
            [{"role": "user", "parts": [{"text": "What's the weather in Beijing? Use the get_weather tool."}]}],
            tools=self.tool_decl(),
            generationConfig={"maxOutputTokens": self.args.max_tokens},
        )
        r = self.post(payload, "tool", stream=False)
        if not quiet:
            print(f"  HTTP {r['status']}")
            for part in self.iter_parts(r.get("json") or {}):
                print("  part:", self.preview_part(part))
        turn = self.capture_tool_turn(r)
        if turn:
            print(f"  captured {len(turn['calls'])} call(s); "
                  f"signatures={sum(1 for p in turn['parts'] if p.get('thoughtSignature'))}")
        return r["status"] == 200 and bool(turn)

    def case_tool(self):
        return self.probe_tool()

    def case_tool_stream(self):
        payload = self.base_payload(
            [{"role": "user", "parts": [{"text": "What's the weather in Beijing? Use the get_weather tool."}]}],
            tools=self.tool_decl(),
            generationConfig={"maxOutputTokens": self.args.max_tokens},
        )
        r = self.post(payload, "tool-stream", stream=True)
        print(f"  HTTP {r['status']} chunks={len(r['chunks'])}")
        for i, chunk in enumerate(r["chunks"]):
            for part in self.iter_parts(chunk):
                print(f"  #{i}", self.preview_part(part))
        self.capture_tool_turn(r)
        return r["status"] == 200

    def _replay_tool_loop(self, case, strip_signatures=False):
        turn = self.ensure_tool_turn()
        if not turn:
            print("  no tool turn captured; skipping")
            return False
        model_parts = []
        for part in turn["parts"]:
            copy = dict(part)
            if strip_signatures:
                copy.pop("thoughtSignature", None)
            model_parts.append(copy)
        responses = []
        for call in turn["calls"]:
            fr = {"name": call.get("name"), "response": {"result": "21C, sunny"}}
            if call.get("id"):
                fr["id"] = call["id"]
            responses.append({"functionResponse": fr})
        payload = self.base_payload(
            [
                {"role": "user", "parts": [{"text": "What's the weather in Beijing? Use the get_weather tool."}]},
                {"role": "model", "parts": model_parts},
                {"role": "user", "parts": responses},
            ],
            tools=self.tool_decl(),
            generationConfig={"maxOutputTokens": self.args.max_tokens},
        )
        r = self.post(payload, case, stream=False)
        print(f"  HTTP {r['status']} (signatures {'stripped' if strip_signatures else 'kept'})")
        if r["status"] != 200:
            print("  error body:", r["raw"][:800])
        else:
            self.print_parts(r.get("json") or {})
        return r["status"] == 200

    def case_tool_loop(self):
        return self._replay_tool_loop("tool-loop")

    def case_parallel_tool_loop(self):
        payload = self.base_payload(
            [{"role": "user", "parts": [{"text": "What's the weather in Beijing and Shanghai? "
                                                  "Call get_weather once per city."}]}],
            tools=self.tool_decl(),
            generationConfig={"maxOutputTokens": self.args.max_tokens},
        )
        r = self.post(payload, "parallel-tool", stream=False)
        print(f"  HTTP {r['status']} (capturing parallel calls)")
        self.capture_tool_turn(r)
        # The captured turn may be from the single-call probe; capture again below.
        turn = self.capture_tool_turn(r)
        if not turn or len(turn["calls"]) < 2:
            print(f"  got {0 if not turn else len(turn['calls'])} call(s); model may not have "
                  "produced parallel calls, replaying what was captured")
        if not turn:
            return False
        # Keep the signature only on the first functionCall to mimic the gateway.
        seen_sig = False
        model_parts = []
        for part in turn["parts"]:
            copy = dict(part)
            if copy.get("functionCall"):
                if seen_sig:
                    copy.pop("thoughtSignature", None)
                elif copy.get("thoughtSignature"):
                    seen_sig = True
            model_parts.append(copy)
        responses = [{"functionResponse": {
            "name": call.get("name"),
            "response": {"result": "21C, sunny"},
            **({"id": call["id"]} if call.get("id") else {}),
        }} for call in turn["calls"]]
        replay = self.base_payload(
            [
                {"role": "user", "parts": [{"text": "What's the weather in Beijing and Shanghai?"}]},
                {"role": "model", "parts": model_parts},
                {"role": "user", "parts": responses},
            ],
            tools=self.tool_decl(),
            generationConfig={"maxOutputTokens": self.args.max_tokens},
        )
        rr = self.post(replay, "parallel-tool-loop", stream=False)
        print(f"  replay HTTP {rr['status']}")
        if rr["status"] != 200:
            print("  error body:", rr["raw"][:800])
        else:
            self.print_parts(rr.get("json") or {})
        return rr["status"] == 200

    def case_signature_missing(self):
        return self._replay_tool_loop("signature-missing", strip_signatures=True) is False

    def case_image(self):
        payload = self.base_payload([{
            "role": "user",
            "parts": [
                {"text": "What color is this image? Answer in one word."},
                {"inlineData": {"mimeType": "image/png", "data": TINY_PNG_B64}},
            ],
        }], generationConfig={"maxOutputTokens": 32})
        r = self.post(payload, "image", stream=False)
        print(f"  HTTP {r['status']}")
        if r.get("json"):
            self.print_parts(r["json"])
        return r["status"] == 200

    def case_params(self):
        schema = {
            "$schema": "https://json-schema.org/draft/2020-12/schema",
            "$defs": {"city": {"type": "string"}},
            "type": "object",
            "properties": {
                "city": {"$ref": "#/$defs/city"},
                "unit": {"anyOf": [{"type": "string", "enum": ["C", "F"]}, {"type": "null"}]},
            },
            "required": ["city"],
            "additionalProperties": False,
        }
        payload = self.base_payload(
            [{"role": "user", "parts": [{"text": "call get_weather for Paris in C"}]}],
            tools=[{"functionDeclarations": [{
                "name": "get_weather",
                "description": "weather",
                "parametersJsonSchema": schema,
            }]}],
            generationConfig={
                "maxOutputTokens": self.args.max_tokens,
                "temperature": 0.2,
                "topP": 0.9,
                "stopSequences": ["END"],
            },
        )
        r = self.post(payload, "params", stream=False)
        print(f"  HTTP {r['status']} (full JSON Schema via parametersJsonSchema)")
        if r["status"] != 200:
            print("  error body:", r["raw"][:800])
        return r["status"] == 200

    def case_thinking_level(self):
        payload = self.base_payload(
            [{"role": "user", "parts": [{"text": "How many r's are in strawberry? One sentence."}]}],
            generationConfig={
                "maxOutputTokens": self.args.max_tokens,
                "thinkingConfig": {"thinkingLevel": "high"},
            },
        )
        r = self.post(payload, "thinking-level", stream=False)
        print(f"  HTTP {r['status']} (thinkingLevel=high)")
        if r.get("json"):
            meta = r["json"].get("usageMetadata") or {}
            print("  usage:", json.dumps(meta))
            self.print_parts(r["json"])
        return r["status"] == 200

    def case_error(self):
        payload = self.base_payload(
            [{"role": "user", "parts": [{"text": "hi"}]}],
            generationConfig={"maxOutputTokens": 16},
        )
        r = self.post(payload, "error", stream=False)
        print(f"  HTTP {r['status']} (expected 4xx)")
        print("  body:", r["raw"][:300])
        return r["status"] is not None and r["status"] >= 400


CASES = [
    "text", "stream", "stream-usage", "max-tokens", "role-system",
    "tool", "tool-stream", "tool-loop", "parallel-tool-loop",
    "signature-missing", "image", "params", "thinking-level", "error",
]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--backend", default="gemini-4-hydrogen", help="backend name in ~/.hi/config.yaml")
    ap.add_argument("--case", action="append", choices=CASES, help="case to run (repeatable, default: all)")
    ap.add_argument("--model", help="override model id")
    ap.add_argument("--max-tokens", type=int, default=256)
    ap.add_argument("--timeout", type=int, default=120)
    ap.add_argument("--raw", action="store_true", help="(reserved) do not truncate previews")
    ap.add_argument("--out-dir", help="artifact dir (default: /tmp/hi-gemini4-native-<ts>)")
    ap.add_argument("--proxy", help="HTTP(S) proxy URL (default: HTTPS_PROXY env)")
    ap.add_argument("--no-proxy", action="store_true")
    args = ap.parse_args()

    bc = load_backend(args.backend)
    if bc.get("type") not in (None, "gemini"):
        print(f"warning: backend {args.backend!r} has type={bc.get('type')!r}, expected 'gemini'")
    api_key = resolve_api_key(bc.get("api_key", ""), load_cc_env())
    if not api_key:
        api_key = os.environ.get("GEMINI_API_KEY") or os.environ.get("GOOGLE_API_KEY") or ""
    if not api_key:
        sys.exit(f"no api key resolved for backend {args.backend!r}")
    model = args.model or (bc.get("models") or {}).get("sonnet") or args.backend
    base_url = bc.get("base_url") or "https://generativelanguage.googleapis.com"

    out_dir = args.out_dir or f"/tmp/hi-gemini4-native-{time.strftime('%Y%m%d-%H%M%S')}"
    os.makedirs(out_dir, exist_ok=True)

    print(f"backend  : {args.backend}  (type={bc.get('type')})")
    print(f"base_url : {base_url}")
    print(f"model    : {model}")
    print(f"key      : {mask(api_key)}")
    print(f"endpoint : {endpoint(base_url, model, False)}")
    print(f"artifacts: {out_dir}")

    prober = Prober(make_opener(args), args, base_url, model, api_key, out_dir)
    selected = args.case or CASES
    results = {}

    for name in selected:
        if name == "error":
            prober.model = "__no_such_model__"
        print(f"\n=== case: {name} ===")
        fn = getattr(prober, "case_" + name.replace("-", "_"))
        try:
            results[name] = bool(fn())
        except Exception as e:  # keep probing the rest
            print(f"  exception: {e!r}")
            results[name] = False
        if name == "error":
            prober.model = model
        print(f"  result: {'ok' if results[name] else 'not-ok'}")
        print(f"  saved : {out_dir}/{name}.*")

    print("\n=== summary ===")
    for name in selected:
        print(f"  {name:<20} {'ok' if results.get(name) else 'not-ok'}")
    print(f"artifacts in {out_dir}")


if __name__ == "__main__":
    main()
