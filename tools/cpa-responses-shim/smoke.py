"""Tool-calling wire format smoke test for cpa-responses-shim and CLIProxyAPI.

Tests live tool calling across client wire formats (responses, messages, chat)
to detect upstream breakages and shim regressions where requests are corrupted.

Usage:
    python3 smoke.py [--base URL] [--models M1,M2] [--wires W1,W2] [--stream]
    python3 smoke.py --compare [--base URL] [--backend URL]

Exit code is 1 if any test fails, regressions occur, or upstream errors happen.
"""

from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
import json
import os
import sys
import time
from typing import Any, Iterator, Tuple
import urllib.error
import urllib.request

try:
    import yaml
except ImportError:
    yaml = None  # type: ignore[assignment]

DEFAULT_BASE = "http://127.0.0.1:8317"
DEFAULT_BACKEND = "http://127.0.0.1:8318"
DEFAULT_MODELS = (
    "opencode-go/omen-alpha,"
    "opencode-go/deepseek-v4-flash,"
    "gemini-3.8-flash,"
    "gpt-5.6-luna,"
    "claude-sonnet-4-6"
)
DEFAULT_WIRES = "responses,messages,chat"
DEFAULT_TIMEOUT = 120.0
DEFAULT_PARALLEL = 4

PROMPT = "Call get_weather for Paris. Do not answer in text."
WEATHER_PARAM_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {"city": {"type": "string"}},
    "required": ["city"],
}


@dataclass(frozen=True)
class TestCase:
    model: str
    wire: str
    stream: bool


@dataclass(frozen=True)
class ProbeResult:
    ok: bool
    latency: float
    reason: str = ""


@dataclass(frozen=True)
class TestOutcome:
    case: TestCase
    status: str
    latency: float
    reason: str = ""


def get_api_key(cli_key: str | None) -> str:
    """Retrieve API key from CLI, environment, or legacy config file."""
    if cli_key and cli_key.strip():
        return cli_key.strip()
    env_key = os.environ.get("CPA_API_KEY", "").strip()
    if env_key:
        return env_key

    config_path = os.path.expanduser("~/cliproxyapi/config-legacy.yaml")
    if os.path.isfile(config_path):
        try:
            if yaml is not None:
                with open(config_path, "r", encoding="utf-8") as f:
                    cfg = yaml.safe_load(f)
                if isinstance(cfg, dict):
                    keys = cfg.get("api-keys") or []
                    if keys and isinstance(keys, list) and len(keys) > 0 and str(keys[0]).strip():
                        return str(keys[0]).strip()
            else:
                with open(config_path, "r", encoding="utf-8") as f:
                    in_keys = False
                    for line in f:
                        stripped = line.strip()
                        if stripped.startswith("api-keys:"):
                            in_keys = True
                        elif in_keys and stripped.startswith("-"):
                            val = stripped[1:].strip().strip("\"'")
                            if val:
                                return val
                        elif in_keys and stripped and not stripped.startswith("#"):
                            break
        except Exception as exc:
            raise RuntimeError(f"Failed to read API key from {config_path}: {exc}") from exc

    raise RuntimeError(
        "API key not found. Provide --api-key, CPA_API_KEY env, or ~/cliproxyapi/config-legacy.yaml"
    )


def has_paris(raw_args: Any) -> bool:
    """Check if arguments object or JSON string contains city Paris."""
    if isinstance(raw_args, str):
        try:
            raw_args = json.loads(raw_args)
        except Exception:
            return "paris" in raw_args.lower()
    if isinstance(raw_args, dict):
        return "paris" in str(raw_args.get("city", "")).lower()
    return "paris" in str(raw_args).lower()


def check_tool(saw: bool, args: Any, text: str, fallback_err: str) -> Tuple[bool, str]:
    """Helper to return PASS or failure reason based on tool extraction."""
    if saw:
        return (True, "") if has_paris(args) else (False, f"tool called with args {args}")
    if text.strip():
        return False, f"text only: {text.strip()[:100]}"
    return False, fallback_err


def iter_sse(resp: Any) -> Iterator[dict[str, Any]]:
    """Yield parsed JSON payloads from an SSE data stream."""
    lines: list[str] = []
    for raw in resp:
        line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
        if not line:
            if lines:
                payload, lines = "\n".join(lines).strip(), []
                if payload and payload != "[DONE]":
                    try:
                        yield json.loads(payload)
                    except Exception:
                        pass
        elif line.startswith("data:"):
            lines.append(line[5:].lstrip())
    if lines:
        payload = "\n".join(lines).strip()
        if payload and payload != "[DONE]":
            try:
                yield json.loads(payload)
            except Exception:
                pass


def build_request(case: TestCase, base_url: str, api_key: str) -> urllib.request.Request:
    """Build urllib request for given test case."""
    base = base_url.rstrip("/")
    if case.wire == "responses":
        url = f"{base}/v1/responses"
        headers = {"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"}
        body: dict[str, Any] = {
            "model": case.model,
            "input": [{"role": "user", "content": PROMPT}],
            "tools": [{"type": "function", "name": "get_weather", "parameters": WEATHER_PARAM_SCHEMA}],
            "max_output_tokens": 1024,
        }
    elif case.wire == "messages":
        url = f"{base}/v1/messages"
        headers = {
            "x-api-key": api_key,
            "anthropic-version": "2023-06-01",
            "Content-Type": "application/json",
        }
        body = {
            "model": case.model,
            "max_tokens": 1024,
            "messages": [{"role": "user", "content": PROMPT}],
            "tools": [{"name": "get_weather", "input_schema": WEATHER_PARAM_SCHEMA}],
        }
    elif case.wire == "chat":
        url = f"{base}/v1/chat/completions"
        headers = {"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"}
        body = {
            "model": case.model,
            "max_tokens": 1024,
            "messages": [{"role": "user", "content": PROMPT}],
            "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": WEATHER_PARAM_SCHEMA}}],
        }
    else:
        raise ValueError(f"Unsupported wire format: {case.wire}")

    if case.stream:
        body["stream"] = True
    return urllib.request.Request(url, headers=headers, data=json.dumps(body).encode("utf-8"), method="POST")


def eval_responses(data_or_resp: Any, is_stream: bool) -> Tuple[bool, str]:
    items: list[dict[str, Any]] = []
    if is_stream:
        for obj in iter_sse(data_or_resp):
            etype = obj.get("type", "")
            if etype == "response.output_item.done" and isinstance(obj.get("item"), dict):
                items.append(obj["item"])
            elif etype == "response.completed":
                out = obj.get("response", {}).get("output", [])
                if isinstance(out, list):
                    items.extend([x for x in out if isinstance(x, dict)])
    else:
        items = [x for x in data_or_resp.get("output", []) if isinstance(x, dict)]

    saw, args, text = False, None, ""
    for item in items:
        if item.get("type") == "function_call" and item.get("name") == "get_weather":
            saw, args = True, item.get("arguments")
            if has_paris(args):
                return True, ""
        elif item.get("type") == "message":
            text += "".join(p.get("text", "") for p in item.get("content", []) if isinstance(p, dict))
    return check_tool(saw, args, text, "no function_call in output")


def eval_messages(data_or_resp: Any, is_stream: bool) -> Tuple[bool, str]:
    saw, args, text = False, None, ""
    if is_stream:
        blocks: dict[int, dict[str, Any]] = {}
        for obj in iter_sse(data_or_resp):
            etype, idx = obj.get("type", ""), obj.get("index", 0)
            if etype == "content_block_start":
                cb = obj.get("content_block", {})
                blocks[idx] = {"name": cb.get("name"), "chunks": [], "input": cb.get("input", {})}
            elif etype == "content_block_delta" and idx in blocks:
                delta = obj.get("delta", {})
                if delta.get("type") == "input_json_delta":
                    blocks[idx]["chunks"].append(delta.get("partial_json", ""))
                else:
                    text += delta.get("text") or delta.get("thinking") or ""
        for b in blocks.values():
            if b.get("name") == "get_weather":
                saw = True
                raw = "".join(b["chunks"])
                try:
                    args = json.loads(raw) if raw else b["input"]
                except Exception:
                    args = raw
                if has_paris(args):
                    return True, ""
    else:
        for b in data_or_resp.get("content", []):
            if not isinstance(b, dict):
                continue
            if b.get("type") == "tool_use" and b.get("name") == "get_weather":
                saw, args = True, b.get("input")
                if has_paris(args):
                    return True, ""
            elif b.get("type") in ("text", "thinking"):
                text += b.get("text") or b.get("thinking") or ""
    return check_tool(saw, args, text, "no tool_use in output")


def eval_chat(data_or_resp: Any, is_stream: bool) -> Tuple[bool, str]:
    saw, args, text = False, None, ""
    if is_stream:
        tools: dict[int, dict[str, Any]] = {}
        for obj in iter_sse(data_or_resp):
            for choice in obj.get("choices") or []:
                delta = choice.get("delta") or {}
                text += delta.get("content") or ""
                for tc in delta.get("tool_calls") or []:
                    idx = tc.get("index", 0)
                    if idx not in tools:
                        tools[idx] = {"name": "", "chunks": []}
                    fn = tc.get("function") or {}
                    name = fn.get("name") or ""
                    # Some providers repeat the full name on every chunk.
                    if name and not tools[idx]["name"].endswith(name):
                        tools[idx]["name"] += name
                    if fn.get("arguments"):
                        tools[idx]["chunks"].append(fn["arguments"])
        for tc in tools.values():
            if tc["name"] == "get_weather":
                saw = True
                raw = "".join(tc["chunks"])
                try:
                    args = json.loads(raw) if raw else {}
                except Exception:
                    args = raw
                if has_paris(args):
                    return True, ""
    else:
        choices = data_or_resp.get("choices", [])
        if choices and isinstance(choices, list):
            msg = choices[0].get("message", {})
            text = msg.get("content") or ""
            for tc in msg.get("tool_calls") or []:
                fn = tc.get("function", {}) if isinstance(tc, dict) else {}
                if fn.get("name") == "get_weather":
                    saw, args = True, fn.get("arguments")
                    if has_paris(args):
                        return True, ""
    return check_tool(saw, args, text, "no tool_calls in output")


def run_probe(case: TestCase, base_url: str, api_key: str, timeout: float) -> ProbeResult:
    """Execute single request and evaluate tool call pass/fail."""
    req = build_request(case, base_url, api_key)
    start_time = time.monotonic()

    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            evaluator = {
                "responses": eval_responses,
                "messages": eval_messages,
                "chat": eval_chat,
            }.get(case.wire)
            if not evaluator:
                return ProbeResult(ok=False, latency=time.monotonic() - start_time, reason=f"unknown wire {case.wire}")

            if case.stream:
                ok, reason = evaluator(resp, True)
            else:
                body_bytes = resp.read()
                try:
                    data = json.loads(body_bytes)
                except Exception as exc:
                    return ProbeResult(ok=False, latency=time.monotonic() - start_time, reason=f"invalid JSON: {exc}")
                ok, reason = evaluator(data, False)

            return ProbeResult(ok=ok, latency=time.monotonic() - start_time, reason=reason)

    except urllib.error.HTTPError as err:
        body = err.read().decode("utf-8", errors="replace")[:200].replace("\n", " ")
        return ProbeResult(ok=False, latency=time.monotonic() - start_time, reason=f"HTTP {err.code}: {body}")
    except Exception as exc:
        return ProbeResult(ok=False, latency=time.monotonic() - start_time, reason=f"error: {str(exc)[:200]}")


def execute_case(
    case: TestCase,
    base_url: str,
    backend_url: str | None,
    api_key: str,
    timeout: float,
) -> TestOutcome:
    """Run test case, optionally comparing shim against backend."""
    shim_res = run_probe(case, base_url, api_key, timeout)

    if backend_url is None:
        return TestOutcome(
            case=case,
            status="PASS" if shim_res.ok else "FAIL",
            latency=shim_res.latency,
            reason=shim_res.reason,
        )

    # Compare mode: distinguish shim regressions from upstream breakages
    if shim_res.ok:
        return TestOutcome(case=case, status="PASS", latency=shim_res.latency, reason="")

    backend_res = run_probe(case, backend_url, api_key, timeout)
    if backend_res.ok:
        return TestOutcome(
            case=case,
            status="REGRESSION",
            latency=shim_res.latency,
            reason=f"shim: {shim_res.reason} (backend passed)",
        )

    return TestOutcome(
        case=case,
        status="UPSTREAM",
        latency=shim_res.latency,
        reason=f"both failed: shim ({shim_res.reason}), backend ({backend_res.reason})",
    )


def main() -> None:
    parser = argparse.ArgumentParser(description="Live tool-calling smoke test per wire format.")
    parser.add_argument("--base", default=DEFAULT_BASE, help=f"Shim URL (default: {DEFAULT_BASE})")
    parser.add_argument(
        "--backend",
        default=DEFAULT_BACKEND,
        help=f"Backend URL for comparison (default: {DEFAULT_BACKEND})",
    )
    parser.add_argument(
        "--models",
        default=DEFAULT_MODELS,
        help=f"Comma-separated models (default: {DEFAULT_MODELS})",
    )
    parser.add_argument(
        "--wires",
        default=DEFAULT_WIRES,
        help=f"Comma-separated wire formats: responses,messages,chat (default: {DEFAULT_WIRES})",
    )
    parser.add_argument("--timeout", type=float, default=DEFAULT_TIMEOUT, help=f"Timeout sec (default: {DEFAULT_TIMEOUT})")
    parser.add_argument("--parallel", type=int, default=DEFAULT_PARALLEL, help=f"Threads (default: {DEFAULT_PARALLEL})")
    parser.add_argument("--stream", action="store_true", help="Test streaming variants (SSE)")
    parser.add_argument(
        "--compare",
        action="store_true",
        help="Compare shim vs backend, flagging regressions vs upstream failures",
    )
    parser.add_argument("--api-key", default=None, help="CPA API key override")

    args = parser.parse_args()

    try:
        api_key = get_api_key(args.api_key)
    except Exception as exc:
        print(f"Error: {exc}", file=sys.stderr)
        sys.exit(1)

    models = [m.strip() for m in args.models.split(",") if m.strip()]
    wires = [w.strip() for w in args.wires.split(",") if w.strip()]
    backend_url = args.backend if args.compare else None

    cases = [TestCase(model=m, wire=w, stream=args.stream) for m in models for w in wires]

    outcomes: list[TestOutcome] = []
    with ThreadPoolExecutor(max_workers=max(1, args.parallel)) as pool:
        futures = {
            pool.submit(execute_case, case, args.base, backend_url, api_key, args.timeout): case
            for case in cases
        }
        for fut in as_completed(futures):
            outcomes.append(fut.result())

    # Deterministic sort by model, wire, stream
    outcomes.sort(key=lambda o: (o.case.model, o.case.wire, o.case.stream))

    # Format output deterministically
    for outcome in outcomes:
        status_field = f"{outcome.status:<10}"
        model_field = f"{outcome.case.model:<30}"
        wire_field = f"{outcome.case.wire:<10}"
        stream_field = f"{'stream' if outcome.case.stream else 'sync':<8}" if args.stream else ""
        lat_field = f"{outcome.latency:6.2f}s"
        reason_field = f"  {outcome.reason}" if outcome.reason else ""

        line_parts = [status_field, model_field, wire_field]
        if stream_field:
            line_parts.append(stream_field)
        line_parts.append(lat_field)
        print(" ".join(line_parts) + reason_field)

    any_fail = any(o.status != "PASS" for o in outcomes)
    sys.exit(1 if any_fail else 0)


if __name__ == "__main__":
    main()
