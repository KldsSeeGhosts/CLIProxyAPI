# CPA model verification and prepared fixes

Verified September 22, 2026. Production services and configuration were not changed. Three small inference requests tested synthetic code images through the existing CPA routes. All returned the correct random identifier. They used 138 completion tokens in total.

## Verified findings

| CPA route | Context | Maximum output | Reasoning | Image input |
|---|---|---|---|---|
| `opencode-go/mimo-v2.6-pro` | Official MiMo docs say 1M | 131072, explicitly documented | Thinking enabled/disabled, enabled by default. No documented low/medium/high distinction found | Officially supported; inline PNG succeeded through CPA and Go |
| `devin/swe-2` | 262000 | 128000 | medium/high/max; existing CPA bare ID starts at medium | Inline PNG succeeded through CPA and the Devin bridge |
| `opencode-go/deepseek-v4.1-flash` | Official DeepSeek docs say 1M | 393216, explicitly documented | low/high/max, high by default; thinking can also be disabled | Officially supported; inline PNG succeeded through CPA and Go |

SWE-2's numbers came from a fresh authenticated `GetCliModelConfigs` response, not the proxy's fallback defaults or cached model list. The context field and nested output field explicitly held 262000 and 128000 for all three tiers. Cognition's public SWE-2 article confirms the medium/high/max tiers, but does not provide these numeric limits.

OpenCode Go's official page confirms the MiMo and DeepSeek routes and Chat Completions endpoint. Its public model endpoint exposes IDs without context/output limits. Its current list includes both `deepseek-flash` and `deepseek-v4.1-flash`; the existing CPA alias routes to `deepseek-flash` and the probe succeeded. No routing rename is needed for this correction.

The MiMo and DeepSeek documents state “1M” context. I did not find an exact decimal token count in the inspected primary documents. The existing 1048576 value is therefore not established as an exact route boundary by this investigation. A conservative interim configured cap is 1000000, with the documented “1M” and the exact-boundary uncertainty retained separately. Output limits above are exact, not inferred from the abbreviations.

Provider limits and Go route enforcement must remain separate facts. These tiny requests confirm basic input handling, not acceptance of a million-token prompt or an output at the documented maximum. Audio, video, remote image URLs, and every possible image format were not tested.

## Corrections to carry into the central catalog

- Replace SWE-2's inherited 272000 context with 262000 and advertise 128000 maximum output.
- Correct SWE-2 and DeepSeek input support to include images. Their text-only CPA declarations are stale. The earlier design report only identified a disagreement; this investigation establishes that the local image capability was usable on both routes.
- Advertise DeepSeek's distinct effort levels as low/high/max. Preserve accepted compatibility values separately: minimal maps to low; medium and xhigh map to high; ultra maps to max. Six accepted spellings do not mean six distinct reasoning levels.
- Replace MiMo's assumed low/medium/high picker behavior with an explicit thinking toggle in adapters that support it. A client without a toggle should use provider-default thinking and avoid claiming that effort labels change model behavior.
- Advertise MiMo's exact 131072 output limit and DeepSeek's exact 393216 output limit. The existing Pi DeepSeek limit of 384000 is a conservative lower value, not the documented maximum.
- Record MiMo and DeepSeek's documented 1M context with the exact-boundary qualification above. Do not inherit a generic 272000-token template.

The existing CPA `ThinkingSupport.Levels` field participates in request normalization as well as discovery. Do not simply remove compatibility aliases from live config to tidy the picker. The central catalog needs separate canonical levels and wire aliases. MiMo also needs a client adapter representation for toggle-only thinking. Those production behavior changes remain part of the central-catalog implementation.

## Code implemented

The changes below are included in this source branch. They have not been deployed to the running proxy.

1. Codex template caching checks the catalog revision before copying its JSON. Repeated discovery calls reuse parsed templates without allocating another full catalog copy. Catalog refresh and synchronization behavior remain covered by tests.
2. Configured OpenAI-compatible models accept `max-completion-tokens`. The value reaches registry output metadata and the existing Codex/Anthropic catalog serializers. This is advertised metadata; it does not overwrite an individual inference request's token budget.
3. OpenAI-compatible model hashes now include context and output limits. Previously a context-only change could retain the same hash, preventing the model refresh expected from a configuration edit.

The new output-limit field is deliberately scoped to OpenAI-compatible provider entries, which cover all three inspected routes. Other provider families and dashboard form controls have not been extended by this patch.

## Validation

- Passed tests for `internal/client/codex/models`, `internal/modelconfig`, `internal/config`, `internal/watcher/diff`, and `sdk/cliproxy`.
- Passed race-detector tests for `internal/client/codex/models`, `internal/modelconfig`, and `internal/registry`.
- Formatted the changed Go files and successfully built `./cmd/server` using Go 1.26.0.
- Verified YAML-to-JSON configuration round-trip and propagation of configured output limits into client catalogs.
- Verified that context-only and output-only changes alter the reload hash.
- Added concurrent cache-read and revision-recovery coverage.

Three-run benchmark of the cached template loader:

| Measurement | Before | After |
|---|---:|---:|
| Time per cached load | 19.96–21.65 microseconds | 19.27–20.79 nanoseconds |
| Allocated bytes per load | about 376832 | 0 |
| Allocations per load | 1 | 0 |

This is a microbenchmark of template lookup. It is not an end-to-end catalog-response or inference throughput claim. The full repository test suite was not run; the affected packages, race checks, and required server build passed.

The development binary was not installed on the server. Build for the server's CPU compatibility requirements before deployment.

## Rollout while agents are active

Keep the live binary and configuration in place while finishing the central catalog and adapters. Configuration is watched, so editing a “metadata” field in the live YAML is still a live change. Staging a separate file avoids unintended reloads.

Before release, compare the candidate source against the currently deployed binary's known local patches. Build for the server, validate an isolated configuration, and test the generated catalogs. Preserve snapshots of both live/staging YAML files and the current binary. Run the existing configuration safety validator before any configuration activation.

The service includes the Responses shim and backend as one unit. A restart may interrupt active streams. Release after the active work drains, or add and verify a separate instance plus a routing change that preserves established connections. A service restart is not a no-downtime deployment.

At the final health check, the proxy, public gateway, and dashboard were all active. No restart, provider update, auth-file update, or live catalog edit was performed by this task.

## Sources

- [OpenCode Go routes and model catalog](https://opencode.ai/docs/go/)
- [OpenCode Go live model IDs](https://opencode.ai/zen/go/v1/models)
- [MiMo model capabilities and limits](https://mimo.mi.com/docs/en-US/quick-start/summary/model)
- [MiMo exact Chat Completions parameters](https://mimo.mi.com/docs/en-US/api/chat/openai-api)
- [MiMo thinking mode](https://mimo.mi.com/docs/en-US/quick-start/usage-guide/text-generation/deep-thinking)
- [DeepSeek models and limits](https://api-docs.deepseek.com/quick_start/pricing)
- [DeepSeek exact Chat Completions parameters](https://api-docs.deepseek.com/api/create-chat-completion)
- [DeepSeek reasoning levels and aliases](https://api-docs.deepseek.com/guides/thinking_mode)
- [DeepSeek image input](https://api-docs.deepseek.com/guides/vision)
- [Devin model selection documentation](https://docs.devin.ai/cli/models)
- [Cognition SWE-2 announcement](https://cognition.ai/blog/swe-2)
- Devin's authenticated `https://server.codeium.com/exa.api_server_pb.ApiServerService/GetCliModelConfigs`, read without inference. No credentials or raw account response are included in the deliverables.

The implementation and tests are committed alongside this report. The shared-catalog service and harness adapter instructions are in [tools/cpa-model-catalog](../tools/cpa-model-catalog/README.md).
