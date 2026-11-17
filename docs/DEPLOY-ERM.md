# CPA build and deployment

Production moved to the Mac Studio on 2026-11-09. The Ubuntu CPA services remain fenced. Do not run the historical Linux service restart commands on that host.

## Active deployment

The active installation is /Volumes/DevDrive/Services/CPA. The signed runtime is in bin/, and the system LaunchDaemons call deploy/launch.py. Since 2026-11-16, CLIProxyAPI listens directly on loopback port 8317. No shim process or port 8318 listener remains. The Manager is on 18317 and the catalog on 28417. All three jobs run under launchd supervision.

This source folds the compatibility transforms into CLIProxyAPI through internal/api/middleware/WireCompatMiddleware. It handles OpenCode Go tool declarations and replay, wire-specific role and description fixes, Gemini replay signatures, and Codex tool_search responses. Only inference POST endpoints engage it. WebSocket and realtime routes bypass the transforms.

The retired tools/cpa-responses-shim source is no longer a build target. Its protocol tests now live beside the middleware. The generic OpenCode Go transforms previously named Muse are retained under OpenCode names.

## Native build and checks

Build on the Mac Studio using its installed Go toolchain. Dynamic providers require cgo. A CGO_ENABLED=0 binary cannot load the existing Cursor and Z.ai dynamic libraries.

```sh
CGO_ENABLED=1 go build -trimpath -o /private/tmp/cli-proxy-api-candidate ./cmd/server
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 go test -race -count=1 -p 1 ./...
```

The repository must have reviewed, reproducible source before a production release. The cleanup source snapshot was recovered at backend commit 81f739d48744a22649fab4df8254dcd2e83fea0c. The old shim subtree at that commit matches production shim commit 2103df5e5756e780dffa63f0ff69228933a60246. Do not build production from the older v7 checkout.

## Cutover requirements

- Validate all supported wire formats with isolated upstream fixtures first. Keep production OAuth ownership exclusive; do not start a second credential-refreshing server against the live auth directory.
- Verify Codex tool search, Claude Code, OpenCode, Jot realtime WebSocket, Antigravity, and Vertex before accepting the candidate.
- Keep the existing panel byte-identical during a proxy cutover. Disable automatic panel replacement. Panel development and Manager updates are separate changes.
- Preserve the existing signed executable identities and Full Disk Access grants. The installed deploy/install-runtime.py stages signed snapshots through the existing keychain identity. Do not change keychain policy or privacy grants.
- Update the launcher and renamed backend config together. The single-binary config binds only 127.0.0.1:8317. The Manager's CPA base remains http://127.0.0.1:8317.
- Check selected model routes before disabling plugins. The current catalog includes cursor-grok-4.6 and zcode/glm-5.3 variants that depend on the deployed providers.
- Preserve a verified rollback set containing the old signed runtime, launcher and config. Do not delete usage data or credentials. Keep the Studio's disabled cpa-tunnel job unloaded.

The system CPA LaunchDaemons are protected by the host's working agreements. Source preparation does not authorize stopping or reconfiguring those jobs. The user explicitly approved the 2026-11-16 rollout. Its signed snapshot is releases/runtime-cleanup-20261116-reviewed. The prior signed runtime, launcher, configuration, panel and consistent SQLite backup with data.key are in recovery/cleanup-20261116.2gAzEF. The unused signed shim remains on disk because the stable runtime manifest requires it, but it is not launched.

## Historical Linux builder

The ZAI plugin source lives in [`KldsSeeGhosts/CPA-Manager-Plus/plugins/zai`](https://github.com/KldsSeeGhosts/CPA-Manager-Plus/tree/main/plugins/zai) (private repository; consolidation tracked in [CPA-Manager-Plus#3](https://github.com/KldsSeeGhosts/CPA-Manager-Plus/pull/3)).

The tools/build-erm.sh helper is Linux-only and produces one backend binary. Its optional plugin build retains the historical baseline-ISA toolchain constraints. It is not the Studio build or deployment path, and it must not activate the fenced Ubuntu service.
