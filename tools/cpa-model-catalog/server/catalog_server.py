#!/usr/bin/env python3
"""CPA catalog pilot. Read with an inference key; publish locally over SSH."""
import argparse
import fcntl
import hashlib
import hmac
import json
import math
import os
from pathlib import Path
import re
import tempfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

MAX_BYTES = 1024 * 1024
APIS = {"openai-completions", "openai-responses", "anthropic-messages"}
LEVELS = {"off", "minimal", "low", "medium", "high", "xhigh", "max"}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def validate_model(m):
    require(isinstance(m, dict), "model must be an object")
    require(set(m) <= {"id", "name", "context_window", "max_output_tokens", "input_modalities", "reasoning", "cost", "pi", "provenance"}, "unknown model field")
    for field in ("id", "name"):
        require(isinstance(m.get(field), str) and 0 < len(m[field]) <= 250 and not any(ord(c) < 32 for c in m[field]), f"invalid {field}")
    for field in ("context_window", "max_output_tokens"):
        require(type(m.get(field)) is int and 0 < m[field] <= 10_000_000, f"invalid {field}")
    require(m["max_output_tokens"] <= m["context_window"], "output exceeds context")
    require(isinstance(m.get("input_modalities"), list) and "text" in m["input_modalities"] and set(m["input_modalities"]) <= {"text", "image"}, "unsupported Pi input type")
    require(type(m.get("reasoning")) is bool, "reasoning must be boolean")
    cost = m.get("cost", {})
    cost_keys = {"input", "output", "cacheRead", "cacheWrite"}
    require(isinstance(cost, dict) and cost_keys <= set(cost) <= cost_keys | {"tiers"}, "invalid cost fields")
    tiers = cost.get("tiers", [])
    require(isinstance(tiers, list) and len(tiers) <= 20, "invalid cost tiers")
    for tier in tiers:
        require(isinstance(tier, dict) and set(tier) == cost_keys | {"inputTokensAbove"}, "invalid tier fields")
        require(type(tier["inputTokensAbove"]) is int and 0 < tier["inputTokensAbove"] <= 10_000_000, "invalid tier threshold")
    require(all(type(c[k]) in (int, float) and math.isfinite(c[k]) and 0 <= c[k] < 1_000_000 for c in [cost, *tiers] for k in cost_keys), "invalid cost value")
    pi = m.get("pi", {})
    require(isinstance(pi, dict) and set(pi) <= {"api", "headers", "compat", "thinkingLevelMap"}, "unsupported Pi transport field")
    require(pi.get("api") in APIS, "unsupported Pi API")
    headers = pi.get("headers", {})
    require(isinstance(headers, dict) and set(headers) <= {"Originator", "Version"}, "only non-secret client identity headers are allowed")
    require(all(isinstance(v, str) and len(v) < 200 and not any(ord(c) < 32 for c in v) for v in headers.values()), "invalid header value")
    compat = pi.get("compat", {})
    require(isinstance(compat, dict) and set(compat) <= {"sendSessionAffinityHeaders", "supportsDeveloperRole", "supportsOpenAIGrammarTools"}, "invalid compat object")
    require(all(type(v) is bool for v in compat.values()), "invalid compat value")
    level_map = pi.get("thinkingLevelMap", {})
    require(isinstance(level_map, dict) and set(level_map) <= LEVELS, "invalid thinking map")
    require(all(v is None or (isinstance(v, str) and v in LEVELS | {"none"}) for v in level_map.values()), "invalid mapped effort")


def validate(doc):
    require(isinstance(doc, dict) and set(doc) == {"schema_version", "models", "profiles"}, "invalid catalog fields")
    require(doc["schema_version"] == 1, "unsupported schema")
    require(isinstance(doc["models"], list) and 0 < len(doc["models"]) <= 500, "invalid model count")
    ids = set()
    for m in doc["models"]:
        validate_model(m)
        require(m["id"] not in ids, "duplicate model ID")
        ids.add(m["id"])
    require(isinstance(doc["profiles"], dict) and doc["profiles"], "profiles required")
    for name, profile in doc["profiles"].items():
        require(re.fullmatch(r"[a-z0-9_-]{1,64}", name), "invalid profile name")
        require(isinstance(profile, dict) and set(profile) == {"models"}, "invalid profile")
        selected = profile["models"]
        require(isinstance(selected, list) and selected and all(isinstance(x, str) for x in selected), "invalid selection")
        require(len(set(selected)) == len(selected) and set(selected) <= ids, "duplicate or unknown selected model")
    return doc


def encoded(doc):
    return json.dumps(doc, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()


def revision(doc):
    return hashlib.sha256(encoded(doc)).hexdigest()


def read_catalog(path):
    raw = Path(path).read_bytes()
    require(len(raw) <= MAX_BYTES, "catalog too large")
    return validate(json.loads(raw))


def export_profile(doc, profile):
    require(profile in doc["profiles"], "unknown profile")
    by_id = {m["id"]: m for m in doc["models"]}
    return {"schema_version": 1, "revision": revision(doc), "profile": profile,
            "models": [by_id[x] for x in doc["profiles"][profile]["models"]]}


def atomic_write(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp = tempfile.mkstemp(prefix=".catalog-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, path)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)


def publish(path, candidate, expected):
    doc = read_catalog(candidate)
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path.with_suffix(".lock"), "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        old = read_catalog(path) if path.exists() else None
        current = revision(old) if old else "absent"
        require(expected == current, "revision conflict; read current catalog before publishing")
        if old:
            atomic_write(path.parent / "history" / f"{current}.json", encoded(old))
        atomic_write(path, json.dumps(doc, indent=2, allow_nan=False).encode() + b"\n")
    return revision(doc)


def handler_for(catalog_path, key_loader):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass  # Never log headers, credentials, or request URLs.

        def reply(self, status, body, etag=None):
            data = encoded(body) if body is not None else b""
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Cache-Control", "private, no-cache")
            self.send_header("Vary", "Authorization")
            self.send_header("Content-Length", str(len(data)))
            if etag:
                self.send_header("ETag", etag)
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            try:
                auth = self.headers.get("Authorization", "").encode()
                allowed = any(hmac.compare_digest(auth, ("Bearer " + key).encode()) for key in key_loader())
            except Exception:
                self.reply(503, {"error": "credential configuration unavailable"})
                return
            if not allowed:
                self.reply(401, {"error": "unauthorized"})
                return
            url = urlsplit(self.path)
            # Tailscale Serve may retain or strip its configured path prefix.
            if url.path not in ("/v1/catalog/pi", "/cpa-catalog/v1/catalog/pi"):
                self.reply(404, {"error": "not found"})
                return
            query = parse_qs(url.query)
            if set(query) - {"profile"} or len(query.get("profile", ["personal"])) != 1:
                self.reply(400, {"error": "invalid query"})
                return
            profile = query.get("profile", ["personal"])[0]
            try:
                doc = read_catalog(catalog_path)
                if profile not in doc["profiles"]:
                    self.reply(404, {"error": "unknown profile"})
                    return
                result = export_profile(doc, profile)
                etag = '"' + hashlib.sha256(encoded(result)).hexdigest() + '"'
                if self.headers.get("If-None-Match") == etag:
                    self.reply(304, None, etag)
                else:
                    self.reply(200, result, etag)
            except Exception:
                self.reply(503, {"error": "catalog unavailable"})

    return Handler


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--catalog", type=Path, default=Path.home() / ".config/cpa-model-catalog/catalog.json")
    commands = parser.add_subparsers(dest="command", required=True)
    serve = commands.add_parser("serve")
    serve.add_argument("--port", type=int, default=18417)
    serve.add_argument("--proxy-config", type=Path, default=Path.home() / "cliproxyapi/config-legacy.yaml")
    commands.add_parser("status")
    publish_cmd = commands.add_parser("publish")
    publish_cmd.add_argument("candidate", type=Path)
    publish_cmd.add_argument("--expect-revision", required=True)
    args = parser.parse_args()
    if args.command == "publish":
        print(publish(args.catalog, args.candidate, args.expect_revision))
    elif args.command == "status":
        doc = read_catalog(args.catalog)
        print(json.dumps({"revision": revision(doc), "profiles": {k: len(v["models"]) for k, v in doc["profiles"].items()}}))
    else:
        import yaml
        read_catalog(args.catalog)

        def keys():
            values = yaml.safe_load(args.proxy_config.read_text()).get("api-keys", [])
            require(isinstance(values, list) and all(isinstance(x, str) and x for x in values), "invalid API keys")
            return values

        keys()
        server = ThreadingHTTPServer(("127.0.0.1", args.port), handler_for(args.catalog, keys))
        server.daemon_threads = True
        server.serve_forever()


if __name__ == "__main__":
    main()
