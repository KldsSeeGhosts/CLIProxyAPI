# CPA shared model catalog

Source for the deployed catalog pilot and its Pi, Codex, Claude Code, and OpenCode adapters. `server/` contains the authenticated sidecar and publisher; `clients/` contains the adapter package and Linux installer. The catalog fixtures include inherited model definitions as well as the specifically verified corrections described below. They are examples, not a live account configuration.

For a fresh checkout, run:

```bash
python3 -m unittest discover -s tools/cpa-model-catalog/server -p test_catalog.py
node tools/cpa-model-catalog/clients/test_client.mjs
node tools/cpa-model-catalog/clients/test_adapters.mjs
node tools/cpa-model-catalog/clients/test_sync.mjs
```

The sidecar's serve command additionally requires `server/requirements.txt`. Read the deployment notes below before installing. The Linux installer currently targets the existing personal CPA topology; inspect the catalog URL and local inference endpoint for another installation. macOS requires launchd and a portable lock implementation and is not yet supported by the installer.

The Go proxy metadata/cache fixes are part of this branch but remain undeployed. See [provider verification](../../docs/cpa-model-verification.md).

---

# Shared CPA models across harnesses

Installed September 22, 2026. The `personal` profile on serverseesghosts is the source for the 11 shared CPA models. Edit that profile once; enrolled clients receive additions, removals, names, and supported metadata without repeating model edits on each machine.

## What is active

| Machine / harness | Installed behavior |
| --- | --- |
| Desktop Pi | Fetches the central profile at startup; last-good cache and seed fallback. |
| Desktop Codex | Catalog refreshed every minute; fresh sessions load 11 shared models plus local Qwen. Default remains GPT-6 Astra, thinking high. |
| Desktop Claude Code | Native picker refreshed every minute. `claude-cpa MODEL` supplies model-specific context limits at launch. Default remains SWE-2. |
| Desktop OpenCode | Native v2 configuration refreshed every minute, including context/output limits, image input, prices, and reasoning variants. Default remains Muse Spark. Installed binary reports `opencode v2.0.12`. |
| Server Pi | Enrolled in the same startup catalog. Existing transport and default Muse Spark are preserved. |
| Server Codex | Catalog refreshed every minute. The pre-existing default `opencode/x-preview-f-free` is retained as an explicit local exception alongside the shared models. |
| Server Claude Code | CPA picker exported to separate settings for `cpa-models claude MODEL`; direct Claude settings and default `sonnet` are preserved. |
| Server OpenCode | Export prepared; executable was not installed, so this is not an active OpenCode enrollment. |

The server Codex configuration pointed at the obsolete desktop HTTPS port 8317. Its CPA URL now uses the server's working `http://127.0.0.1:8317/v1` endpoint. No provider service was restarted.

The desktop's old MiMo-specific Codex selections and other harness-specific CPA selections outside `personal` are no longer in the generated shared list. They remain in the original templates/backups. Add a route to the central profile to expose it consistently after its metadata and adapter behavior are verified. The current profile intentionally excludes MiMo, whose toggle-only thinking needs a suitable representation before advertising effort tiers.

The Mac refused SSH, the Raspberry Pi denied the current SSH user, and Windows was offline. Those machines are not enrolled. Devin and Cursor's own clients were not modified; an installed native provider client is not necessarily a configurable CPA client.

## Daily use

On either enrolled Linux machine:

```bash
~/.local/bin/cpa-models status
~/.local/bin/cpa-models list
~/.local/bin/cpa-models sync
```

Synchronization normally runs once a minute through `cpa-catalog-sync.timer`. A failed fetch or invalid catalog retains the installed settings. Errors are visible in `journalctl --user -u cpa-catalog-sync.service`; `status` shows the last successful check and revision.

Start a fresh Pi or Codex process to reliably load a changed model selection. Existing sessions are not switched or stopped. OpenCode v2 watches its configuration; a fresh session/process gives a deterministic view, and its native `opencode reload` command is available when idle. The Codex desktop application's existing model menu can retain its startup catalog until its session/backend refreshes; this pass did not restart the application.

Claude's native `/model` picker has the shared names and capability descriptions, but it does not provide all the runtime metadata controls offered by Pi, Codex, and OpenCode. Use the model-aware launcher:

```bash
claude-cpa devin/swe-2
# Equivalent, available on both machines:
~/.local/bin/cpa-models claude devin/swe-2
```

Calling `claude-cpa` without arguments opens its catalog-backed fzf picker when fzf is installed. Arguments after the model pass through to Claude. The launcher sets the selected model's context limit, a conservative compaction window, and an output request budget capped at 32000 tokens and the model's maximum. The smaller request budget is not a claim about the provider's output capacity. A runtime check confirmed Claude reported SWE-2's 262000 context and 32000 request budget.

Claude's context environment variables apply to the whole process. **Relaunch through the wrapper when changing models to obtain the new limits.** Switching with `/model` inside that process does not update them. This pass does not claim exact native Claude picker support for each provider's distinct effort tiers or image capabilities. The descriptions show the central facts; provider/client support still governs requests.

OpenCode can be launched normally from a shell that has `CPA_API_KEY`, or with:

```bash
~/.local/bin/cpa-models opencode
# Example with a specific native v2 reasoning variant:
~/.local/bin/cpa-models opencode run --standalone \
  --model 'cpa-chat/devin/swe-2#medium' 'Reply with OK.'
```

## Updating models centrally

The source is `~/.config/cpa-model-catalog/catalog.json` on serverseesghosts. Read and preserve unrelated records. Model definitions supply metadata; `profiles.personal.models` supplies selection and ordering.

The publisher is `~/.local/lib/cpa-model-catalog/catalog_server.py`. First record the current revision, then copy and edit the catalog:

```bash
ssh -o BatchMode=yes serverseesghosts@serverseesghosts \
  'python3 ~/.local/lib/cpa-model-catalog/catalog_server.py status'
scp serverseesghosts@serverseesghosts:.config/cpa-model-catalog/catalog.json ./candidate.json
```

Verify changed facts against the actual upstream route's official documentation. Preserve client transport requirements. Copy the candidate back, then publish against the revision read before editing:

```bash
scp ./candidate.json serverseesghosts@serverseesghosts:.config/cpa-model-catalog/candidate.json
ssh -o BatchMode=yes serverseesghosts@serverseesghosts \
  'python3 ~/.local/lib/cpa-model-catalog/catalog_server.py publish ~/.config/cpa-model-catalog/candidate.json --expect-revision ORIGINAL_REVISION'
```

The publisher validates, locks, archives the old revision, and atomically replaces the file. A revision conflict requires reading and merging intervening changes. Publication does not require a restart. An inference route must already exist in CPA; catalog publication alone cannot create it. Catalog removal changes discovery/selection, not authorization or existing sessions.

The dashboard editor is not connected to this catalog yet. The historical read URL is still `/cpa-catalog/v1/catalog/pi?profile=personal` on the tailnet HTTPS host, even though all adapters consume it. It uses the existing CPA inference key and is separate from the public inference endpoint on port 8443.

## Implementation and local exceptions

| Component | Location on each enrolled machine |
| --- | --- |
| Synchronizer and adapters | `~/.local/lib/cpa-harness-sync/` |
| Target configuration | `~/.config/cpa-catalog/sync.json` |
| Codex transport templates | `~/.config/cpa-catalog/codex-templates.json` |
| Last successful sync | `~/.local/state/cpa-catalog/status.json` |
| Generated-file backups | `~/.local/state/cpa-catalog/backups/` |
| Installer backups and path manifest | `~/.local/state/cpa-catalog/install-backups/` |
| Pi client and seed | `~/.pi/agent/lib/cpa-catalog/` |

`sync.json` keeps endpoints, target files, executables, and explicit Codex `localModels` exceptions local. Credentials stay in the existing environment/key file. The remote catalog cannot specify new transport destinations, secret headers, or executable code. Codex templates retain the existing prompts and tool protocol fields; catalog facts replace context, input types, and reasoning choices. This Codex catalog format does not expose a verified independent output-limit control, so the adapter does not invent one.

OpenCode v2 uses `providers`, `package`, `modelID`, capability objects, cost tiers, and a `variants` array with `body.reasoning_effort`. Its live file is `~/.config/opencode/opencode.jsonc` on the desktop. The current file contains plain JSON; synchronization deliberately fails rather than guessing if it becomes invalid or contains unsupported JSONC comments. The server's unactivated OpenCode export uses the older schema until an installed version is detected/enrolled.

The legacy `sync-claude-pi-models` entrypoint now calls the central synchronizer. Previously it interpreted Pi's wildcard as a literal model ID and emptied Claude's picker. The fish `claude-cpa` function now reads the catalog instead of carrying its own static list.

Synchronization uses an OS file lock, validation before writes, backups, temporary-file replacement, and a comparison with the original file before replacement. It preserves unrelated Claude settings and OpenCode providers/defaults. Files update individually; the status revision advances only after all target writes succeed. A failed pass is retried by the timer.

## Enrollment on another Linux machine

This package contains `install.py`, `sync.mjs`, `adapters.mjs`, `client.mjs`, and `seed.json`. Copy the package to the machine. Requirements are Python 3.11+, Node.js 22+, `flock`, user systemd, tailnet access, and an existing CPA credential at `~/.config/environment.d/cpa.conf` or in `CPA_API_KEY`.

The installer enrolls existing CPA Codex catalogs, creates a separate Claude CPA settings export by default, detects an installed OpenCode v2, and starts a new sync timer. It does not install harness executables or create new inference tunnels.

```bash
python3 install.py
# Only when ordinary Claude on this machine already uses CPA:
python3 install.py --claude-user-settings
# For an existing compatible, not-yet-enrolled CPA Pi extension:
python3 install.py --enroll-pi
```

Configure `inferenceBase` for that machine's reachable CPA URL before using the launchers. Pi retains its existing transport endpoint; Codex retains its provider configuration. Do not blindly copy another machine's extension: it contains local transport dependencies. macOS needs a launchd enrollment path, and Windows needs an appropriate scheduler; this Linux installer is not a promise of cross-platform installation support.

## Validation

- Unit/integration tests cover correct metadata and effort mapping, preservation of defaults and unrelated settings, central additions/removals, no-op writes, invalid-response rejection, and retaining installed files when offline.
- Codex's actual app-server `model/list` accepted the generated catalogs on both machines and exposed SWE-2's medium/high/max levels and image input.
- Pi's installed CLI loaded all 11 shared CPA models on the server after enrollment; the desktop pilot was already verified.
- OpenCode v2's own OpenAPI schema validated the native configuration. An isolated v2 server returned all shared models with their corrected limits and reasoning variants.
- A real Claude request through SWE-2 returned `CPA_CLAUDE_OK`; another confirmed the 262000 context in runtime usage metadata.
- A real OpenCode v2 request through `cpa-chat/devin/swe-2#medium` returned `CPA_OPENCODE_OK`.
- A central publication added `devin/swe-2-high`; both machines' Codex, Claude, and OpenCode files gained it. Restoring the original profile removed it everywhere. The shared selection is back to 11. Codex has one local exception on each machine, so its total is 12.
- Both new timer services completed with exit code 0. Existing CPA/gateway/dashboard PIDs remained 1638468, 1059371, and 380091.

Run package tests with `node test_adapters.mjs` and `node test_sync.mjs`. The v2 schema check and runtime/inference checks above used the installed executable; unit tests alone do not prove model inference.

## Pause and recovery

Stop automatic writes on an affected machine with:

```bash
systemctl --user disable --now cpa-catalog-sync.timer
```

This does not stop any harness or inference service. Inspect `sync.json` for managed targets. The installer manifest maps original helper/config paths to backups, and generated-file backups are grouped by SHA-256 of the target path, then SHA-256 of the previous content. Codex's pre-enrollment catalog is also retained as `codex-templates.json`. Merge or restore the specific affected file, preserving newer user edits. Start a fresh client to read it.

To roll back a central selection, publish an archived catalog from `~/.config/cpa-model-catalog/history/` using the current expected revision. The clients then synchronize the restored selection normally. Do not restart CPA for catalog recovery.
