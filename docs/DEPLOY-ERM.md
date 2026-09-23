# ERM Build & Deploy Runbook

The running production binary on `serverseesghosts` must always match a pushed
commit of this repo (running == GitHub). Building and deploying a replacement
is safe **only** under the constraints below. The critical one: **build with
`CGO_ENABLED=0`** — anything else gets silently reverted to
`cli-proxy-api.backup-known-good` by a systemd safety net at the next service
start.

- Baseline established 2026-09-22: `v7.2.146-17-g9cca0d10`, commit `9cca0d10`
  (== `erm/zai-oauth` head == `main`)
- Toolchain: user-space Go 1.26.8 at `~/go-toolchain/go1.26.8/bin/go` on
  `kidsseeghosts` (no system Go exists on any machine)

## Why CGO_ENABLED=0 is mandatory

1. Default CGO links the backend against **CachyOS glibc**, whose ELF
   `.note.gnu.property` stamps `x86 ISA needed: … x86-64-v4`.
2. The server CPU is an **i7-8750H**; `execve` of the v4-noted binary fails
   with `CPU ISA level is lower than required` (exit 127). Same toolchain,
   `CGO_ENABLED=0` → statically linked → runs fine.
3. The failure is **silent by design**: the systemd dropin
   `~/.config/systemd/user/cliproxyapi.service.d/isa-safety.conf` runs
   `cli-proxy-api -help` as `ExecStartPre` and, on failure, `cp -a`-restores
   `cli-proxy-api.backup-known-good` and starts that instead. The service
   shows *active* while running a months-old binary. You detect it by the
   version line in the log printing `dev / none / unknown`.

(History: the `broken-isa` / `before-isa-fix` backups in `~/cliproxyapi/` show
this CPU has bitten us before. The safety net is good — respect it, don't
remove it.)

## 1. Build (from a clean tree — commit or stash first)

```bash
cd ~/Projects/CLIProxyAPI
tools/build-erm.sh              # outputs to /tmp/cpa-build by default
# or: tools/build-erm.sh /tmp/cpa-build --plugins
```

The script enforces the runbook rules: refuses tracked modifications and
untracked `*.go` files (they would be compiled in and make the build
irreproducible from GitHub), forces `GOAMD64=v1`, and stamps
`Version`/`Commit`/`BuildDate` from git.

### Two build modes

- **Default (static, no plugins):** `CGO_ENABLED=0`. Use only while the
  deployment runs zero dlopen plugins.
- **`--plugins` (cgo host):** the binary must be built `CGO_ENABLED=1` —
  dynamic-library plugin loading (dlopen) is compiled out otherwise and the
  host logs `standard dynamic library plugin loading requires cgo on this
  platform` for every `.so` in `plugins/`. A plain cgo build on CachyOS is
  NOT deployable: the linker merges x86-64-v4 ISA notes from the distro's
  crt objects and the i7-8750H server rejects the binary (the isa-safety
  net then silently reverts). The `--plugins` mode therefore compiles with
  zig as the C toolchain:

  ```
  CC="zig cc -target x86_64-linux-gnu.2.34" CGO_ENABLED=1 GOAMD64=v1
  ```

  which produces a binary with **no ISA notes** and glibc symbol
  requirements capped at GLIBC_2.34 (server glibc 2.44 satisfies this).
  Verified in production 2026-09-22 (`v7.3.14-20-g2d9245e9` + cpa-zai-plugin).
  The plugin `.so` itself is built separately (repo: `KldsSeeGhosts/cpa-zai-plugin`,
  its `build.sh` does the same GOAMD64=v1 + readelf ISA verification) and is
  deployed to `~/cliproxyapi/plugins/zai.so` — the host derives the plugin id
  from the filename, so the file MUST be named `zai.so` to match
  `plugins.configs.zai`.

The `cpa-responses-shim` is stdlib-only and always builds `CGO_ENABLED=0`
in both modes.

Equivalent manual invocation:

```bash
export PATH="$HOME/go-toolchain/go1.26.8/bin:$PATH"
export CGO_ENABLED=0
export GOAMD64=v1

VERSION="$(git describe --tags --always)"
COMMIT="$(git rev-parse --short HEAD)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

go build -trimpath -ldflags "-s -w \
  -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILD_DATE}" \
  -o /tmp/cpa-build/cli-proxy-api ./cmd/server

go build -trimpath -ldflags "-s -w" \
  -o /tmp/cpa-build/cpa-responses-shim ./tools/cpa-responses-shim
```

## 2. Pre-flight on the server (BEFORE touching the live path)

```bash
scp /tmp/cpa-build/cli-proxy-api serverseesghosts@serverseesghosts:/tmp/cli-proxy-api
ssh serverseesghosts@serverseesghosts '
  install -m 755 /tmp/cli-proxy-api /tmp/cpa-test
  /tmp/cpa-test -help >/dev/null 2>&1; echo "exit=$?"'   # require exit 0
```

If this is non-zero, do not deploy — the safety net would revert anyway.

## 3. Deploy

```bash
ssh serverseesghosts@serverseesghosts
STAMP=$(date +%Y%m%dT%H%M%SZ)
cd ~/cliproxyapi
cp cli-proxy-api "cli-proxy-api.backup-<label>-$STAMP"
cp cpa-responses-shim "cpa-responses-shim.before-<label>-$STAMP"
install -m 755 /tmp/cli-proxy-api  ~/cliproxyapi/cli-proxy-api
install -m 755 /tmp/cpa-responses-shim ~/cliproxyapi/cpa-responses-shim
UID_=$(id -u); export XDG_RUNTIME_DIR=/run/user/$UID_
systemctl --user restart cliproxyapi.service
```

## 4. Verify

```bash
systemctl --user is-active cliproxyapi.service          # must be: active
grep "CLIProxyAPI Version" ~/.cli-proxy-api/logs/main.log | tail -1
# must show the NEW Version/Commit. "dev / none / unknown" == the safety net
# reverted you — stop and re-check the build.
```

Then run the inference canaries (expects HTTP 200 + echo from each):

```bash
~/.local/bin/cpa-canary          # or: cpa-canary gpt-6-sol gpt-6-luna gpt-6-astra zai/glm-5.3-flash
```

(`gpt-6-*` exercises the Codex route + the auth-file `Version` header
override; `zai/glm-5.3-flash` exercises the Z.AI provider and the
embedded-zai fallback.)

## 5. Rollback

```bash
cd ~/cliproxyapi
install -m 755 cli-proxy-api.backup-pre-baseline-20260922T153312Z cli-proxy-api
UID_=$(id -u); export XDG_RUNTIME_DIR=/run/user/$UID_
systemctl --user restart cliproxyapi.service
```

Backups follow `cli-proxy-api.backup-<label>-<STAMP>`; restoring the pre-deploy
binary + restart returns to the pre-deploy state. Auth files and catalogs are
unaffected by a binary swap.

## Critical runtime-config dependencies (do not "clean up" these)

- **Codex auth files carry a `Version` header override.** Each
  `~/.cli-proxy-api/codex-*.json` has a top-level
  `"headers": {"Version": "0.155.1"}`. The ChatGPT backend rejects `gpt-6-*`
  requests for clients advertising older versions (`gpt-5.6` era headers →
  "model is not supported when using Codex with a ChatGPT account"). Removing
  this key breaks GPT-6 routing for every harness except real Codex CLI. Bump
  it when the real Codex CLI version moves ahead (keep it ≥ the current
  codex-cli release; it must also match the `X-Codex-Beta-Features` the CLI
  sends). Rollback copies: `*.bak-pre-gpt6-version-*`.
- **Embedded `zai` section in `internal/registry/models/models.json`.**
  Upstream's catalog has no `zai` section; if you ever re-sync the embedded
  catalogs from `router-for-me/models`, re-splice the `zai` array (see commit
  `5cfd2d7d`) or GLM routing dies at next build. The model updater's
  `applyEmbeddedZAIFallback` preserves it across remote refreshes as long as
  the embedded copy has it.

## Upstream pulls

After merging a new upstream tag into `erm/zai-oauth`, fast-forward `main` to
the merge commit so `main` always tracks the integration line. Aim for a
regular (monthly) cadence so the delta never grows as large as the v7.2.x →
v7.3.x gap (372+ commits) again.
