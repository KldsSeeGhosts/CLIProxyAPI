#!/usr/bin/env bash
# Build the ERM line of CLIProxyAPI for deployment to serverseesghosts.
# Static-only: CGO_ENABLED=0 is MANDATORY (see docs/DEPLOY-ERM.md) — cgo
# builds link CachyOS glibc with x86-64-v4 ISA notes and fail on the
# i7-8750H server; the isa-safety systemd dropin then silently reverts
# to cli-proxy-api.backup-known-good.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
    echo "ERROR: tracked modifications present — commit or stash first." >&2
    echo "  (runbook rule: never deploy a dirty build; baseline pattern is 9cca0d10)" >&2
    exit 1
fi
untracked_go="$(git status --porcelain --untracked-files=all | awk '$1=="??" && $2 ~ /\.go$/ {print $2}')"
if [ -n "$untracked_go" ]; then
    echo "ERROR: untracked .go files would be compiled into the build:" >&2
    echo "$untracked_go" | sed 's/^/  /' >&2
    echo "  commit, stash, or move them first." >&2
    exit 1
fi

GO_BIN="${GO:-$HOME/go-toolchain/go1.26.8/bin/go}"
if ! command -v "$GO_BIN" >/dev/null 2>&1; then
    echo "ERROR: go not found at $GO_BIN (override with GO=/path/to/go)" >&2
    exit 1
fi

export CGO_ENABLED=0 GOAMD64=v1
VERSION="$(git describe --tags --always)"
COMMIT="$(git rev-parse --short HEAD)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
OUT="${1:-/tmp/cpa-build}"
mkdir -p "$OUT"

echo "==> $VERSION (commit $COMMIT) -> $OUT (CGO_ENABLED=0, GOAMD64=v1)"

"$GO_BIN" build -trimpath -ldflags "-s -w \
  -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILD_DATE}" \
  -o "$OUT/cli-proxy-api" ./cmd/server

"$GO_BIN" build -trimpath -ldflags "-s -w" \
  -o "$OUT/cpa-responses-shim" ./tools/cpa-responses-shim

sha256sum "$OUT/cli-proxy-api" "$OUT/cpa-responses-shim"
echo "==> next: pre-flight + deploy per docs/DEPLOY-ERM.md sections 2-4"
