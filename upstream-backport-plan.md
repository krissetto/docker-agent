# Selective upstream backport plan

**Implemented locally on 2026-10-08.** All eight recommended groups are integrated in 13 signed commits on `subagents-reborn`, ending at `cb5c7d0b4f76bb3d7539957218e951e675f2bd1f`. No Git push or Kit publication has been made for these backports. The assessment below is retained as source attribution and design context; its statements about checks not run refer to the original assessment, not the later implementation.

## Implementation record

| Commit | Change |
|---|---|
| `c0e55e537` | Give OpenAI WebSocket streams exclusive connection ownership |
| `175b0cf04` | Reject dangling symlinks in filesystem policies |
| `ad681f924` | Confine tool installation to its opened destination |
| `761f69819` | Confine Kit prompt and skill staging |
| `de6525286` | Confine skill supporting-file reads |
| `868ebb400` | Synchronize Anthropic stream retry and close |
| `aab577138` | Preserve cumulative Anthropic usage across sparse events |
| `25ead3a77` | Count Gemini tool-use prompt tokens |
| `b2f9698c4` | Preserve Gemini tool identities and replay signatures |
| `574f450cf` | Account for actual OpenAI service-tier pricing |
| `2abac866a` | Keep tool output bounded after spill and hook failures |
| `44f0e15e0` | Bound historical tool results in model-facing projections |
| `cb5c7d0b4` | Preserve OpenAI Responses state when explicitly enabled |

The integrated patch series and source/test evidence are under `/tmp/da-backports.aliWH1/patch-series/`. Parent preservation and combined-source integration records are under `/tmp/docker-agent-backport-integration.wfdkvysw/`. Parent focused checks `parent-backports-shared` and `parent-backports-providers` passed (retained under `/tmp/da-spinner.ToF4hr/`), supplementing the team's security and targeted concurrency checks. No full suite/build/lint/live-provider or cross-platform certification.

Responses state remains opt-in (`provider_opts.preserve_reasoning: true`), first-party endpoint/model gated, and excluded from diagnostic logs. Captured opaque data remains in local durable history after opt-out. WS connection leases do not address the existing pool-wide default continuation-ID scoping. Service-tier rates remain allowlisted/time-sensitive. Conditional/skip extras below were not imported. Restoration defects and sidebar gestures are separate work, not part of these commits. Paused edits and the six numbered upstream-proposal worktrees remain excluded.

## Exact comparison and freshness

- Fork: committed `aeb0d5cab601bea8ac6a0cf1a453dc37613049c8` on `subagents-reborn` (2026-10-08 11:16:59 +02:00).
- Public `docker/docker-agent` main: `7a69c316f03c635d8d1951bc4677c59b47beedc7` (2026-10-08 09:35:36Z), rechecked with HTTPS `git ls-remote` at **2026-10-08 11:04:41Z**. It remains the previously inspected public tip.
- Local `upstream/main` remains stale at `5517a597150afefa6e0d441f1d9893db4ed712ad`, 99 commits behind that public tip. It was not updated; all required objects were already available.
- Merge base: `4c8ddfb02c1ab0faeb4003239972f89b7f2a77ca`.
- Dirty worktree changes, user configuration, rotating logs, live providers/databases/sandboxes, and the six numbered runtime proposal branches/worktrees were excluded. Those proposals are neither merged public main nor the fork baseline.
- Whole-commit `git cherry` found no patch-equivalent upstream commits in this range. **That does not establish missing functionality:** several upstream fixes are already implemented differently in fork source. Coverage conclusions below use committed-source comparison.

## Recommendation

Start with small security boundaries and stream ownership fixes, not a merge of upstream runtime architecture. The eight groups below are ordered by practical value and risk. Independent patches within a group should remain separately mergeable.

### Port now: bounded corrections

| Priority | Candidate and upstream source | Confirmed fork gap / benefit | Recommended route and dependencies |
|---|---|---|---|
| **1** | **Exclusive OpenAI WebSocket leases and synchronized close** — [33acc2845][ws-lease], [a5ff5c681][ws-close]. `pkg/model/provider/openai/{ws_pool,ws_stream}.go` | Fork leaves an active socket in the reusable pool; pooled early Close sets an unsynchronized bool and may leave a blocked reader/unread response. Independent streams can overlap on one connection. Especially relevant when concurrent agents share a client. | Take the pair together; near-direct port, not a pool rewrite. Reconcile fork `PreviousResponseID.Valid()` versus newer SDK omission handling. Preserve the fork's **6522ee360 semantic-completion EOF latch**. This fixes socket ownership, **not** shared `lastResponseID` conversation scoping. Moderate concurrency risk. |
| **2** | **Filesystem policy and installer extraction confinement** — [76ccf9390][dangling], [e7fa3958c][extraction]. `pkg/tools/builtin/filesystem/filesystem_paths.go`, `pkg/toolinstall/{archive,installer}.go` | Fork path resolution treats dangling symlinks as ordinary missing paths. Installer checks lexical paths but later writes/chmods by pathname. Upstream rejects the former and roots extraction through `os.Root`, protecting against escaping/replaced destination symlinks. | Two independent small ports. Dangling-link patch is six production lines; strong direct-pick candidate. Installer is a coherent five-file patch; port complete root-relative API/caller changes. Keep fork download limits. `os.Root` already exists in the fork; check platform behavior. Rejecting formerly accepted dangling-link writes is intentional. |
| **3** | **Kit prompt/skill staging and supporting-file confinement** — [50bc87521][prompt-stage], [96e215d6f][skill-stage], [868d428d3][skill-read]. `pkg/sandbox/kit/kit.go`, `pkg/tools/builtin/skills/skills.go` | Fork lacks local prompt-name rejection; staging checks symlinks before later pathname reads/writes. Supporting-file reads check lexical containment then call `os.ReadFile`, so a symlink can escape the skill directory. Directly relevant to loading third-party/local skills into Kit. | Prompt + staging patches are sequential port candidates after context reconciliation. Selectively extract rooted supporting-file reads: upstream guard integration is not present in fork. Keep existing policy checks. Deliberate compatibility changes include rejecting nonlocal prompt names and some symlinked roots/absolute links. |

“Direct-pick candidate” describes source similarity only: no apply-check or cherry-pick was performed.

### Adapt carefully: worthwhile, but preserve fork semantics

| Priority | Candidate and upstream source | Confirmed fork gap / benefit | Recommended route and dependencies |
|---|---|---|---|
| **4** | **Anthropic retry closure and cumulative usage** — [e91aadf45][anthropic-close], [7bbc2c614][anthropic-usage]. `pkg/model/provider/anthropic/{retry,adapter,beta_adapter}.go` | Retry may install a replacement after Close; adapter Close reads the stream pointer unsynchronized. Separately, fork maps usage only from `message_delta`, losing start-event input/cache counts when later deltas contain only output. | **Retry fix can port now independently**, with one-line adapter Close changes. Usage is selective adaptation: fork lacks upstream accumulated-message/raw-state scaffolding from [e2715467e][anthropic-state]. Do not import that broader feature as a dependency. Preserve sparse-field semantics: absent/null ≠ explicit zero; cumulative counts must not be summed as deltas. |
| **5** | **Gemini tool protocol fidelity and tool-use input accounting** — [acf814ced][gemini-ids], [bd9b0135e][gemini-usage]. `pkg/model/provider/gemini/{adapter,client}.go`, `pkg/tools/tools.go`, `pkg/runtime/streaming.go` | Fork discards native call IDs, uses local call IDs as function-response names, and attaches thought signatures incorrectly across replayed calls. Trailing usage-only chunks can affect finish reason. It also omits `ToolUsePromptTokenCount` from fresh input. | **Usage arithmetic is a tiny independent port now.** Identity fix needs `ToolCall.ProviderID` plus runtime accumulation/history replay, call-name/order lookup and signature tests. Selectively adapt to the fork's queued per-part streaming; never replace its ordered **Presentation** behavior with upstream aggregate emission. Moderate cross-layer change, no new execution owner. |
| **6** | **Fail-bounded tool output and historical model projections** — [df389ba21][output-bound]. `pkg/hooks/builtins/limit_large_tool_results*`, `pkg/runtime/{transforms,toolexec/dispatcher,compactor}`, `pkg/tools/builtin/backgroundjobs` | Fork already has configurable per-result/history caps and an owner-keyed spill limiter. Missing are the independent fail-bounded backstop after spill failure/post-hooks, background-job coverage, and known-tool historical projection bounding. Fork spill failure currently returns unchanged output. | Extract pure bounding helpers/post-hook and job backstops first; historical model/compactor projection second. Preserve owner-keyed temp cleanup, canonical immutable history, configurable aggregate text/document limits, transform policy and Presentation. Upstream unknown-tool histories deliberately remain untouched; saved definitions matter. No wholesale limiter/runtime replacement. |
| **7** | **Price the actual OpenAI service tier** — [07a3a4a73][tier-cost] plus [af03ff01d][tier-correction]. `pkg/chat/chat.go`, provider adapters, `pkg/modelsdev/service_tier.go`, `pkg/runtime/loop.go` | Fork can request `service_tier`, but Usage has no actual tier and pricing uses normal catalogue rates. Faster billed tiers can therefore be underreported in cost/budgets. Conditional value: matters when those tiers are actually used. | Selective adapter → persisted Usage → canonical cost-ledger adaptation. Include the correction accepting explicitly configured official OpenAI URLs. Preserve fallback overrides, custom endpoints/router exclusions and nil-versus-free cost semantics. Model multipliers are allowlisted/time-sensitive, not universal. The Vercel catalogue-boundary adjustment in the correction is separable. |
| **8** | **Opt-in OpenAI Responses opaque reasoning/output preservation** — [a3ce461e9][responses-state]. `pkg/chat/openai.go`, `pkg/model/provider/openai/{response_state,response_stream,response_logging,client}.go` and runtime persistence | Fork preserves visible reasoning/order, not provider continuation state: no OpenAIResponse replay metadata, encrypted-content request/state retention, or guarded output replay. This is a genuinely missing capability, not fixed by Presentation or the EOF latch. | **Larger follow-up, not a direct pick:** the 41-file commit bundles cache diagnostics and native tool search. Extract only wanted preservation behavior, with provider/model/endpoint gating, edit invalidation, cloning, persistence/compaction and WS continuation interaction. Redact nested `encrypted_content` in diagnostic payloads; that small helper can be extracted independently (fork still logs raw incomplete/unhandled payloads). No claim such data exists in live logs. Keep the EOF latch and Presentation. No new runtime/owner architecture is warranted. |

### Focused checks for a future implementation

The following upstream test source was examined, **not run**. Reuse/adapt these cases rather than demanding an architectural rewrite:

1. WS: `TestWSPool_ExclusiveStreamLease`, early-close discard, repeated close, shutdown unblocks reader, canceled admission, concurrent requests; `TestWSStreamConcurrentCloseAndNext`. Retain fork `TestResponseStream_CompletedDoesNotWaitForTransportEOF` and `TestResponseStream_CompletedPreservesPooledConnection`.
2. Paths/extraction: `TestFilesystemTool_RejectsDanglingSymlinks`, `TestExtractionRejectsEscapingSymlinks`; final/ancestor links, contained links, missing ordinary paths, root/directory replacement, tar/zip/raw, no writes outside root.
3. Kit/skills: prompt traversal/nonlocal names/destination symlinks; opened source identity after replacement; contained/escaping/dangling skill links; read errors/cancellation. Adapt guard-specific upstream fixtures only if corresponding fork APIs exist.
4. Anthropic: `TestStreamAdapterCloseDuringRetry`, beta equivalent, successful retry; `TestStreamUsagePreservesCumulativeCounts` with sparse/zero/null fields, both adapters, tracking disabled, previous snapshot immutability.
5. Gemini: `TestStreamAdapter_ToolUsePromptTokens`, `TestStreamAdapter_SignatureOnlyChunk`, `TestConvertMessagesToGemini_ToolResponseMatching`, `TestCreateChatCompletionStream_ToolCallReplay`, `TestHandleStream_PreservesProviderToolCallID`; include reversed parallel results, usage-only terminal chunks and fork ordered media/reasoning/tool parts.
6. Output bounds: spill failure, hooks expanding output, long notices, UTF-8/idempotence, background recall/status retention, historical stream/compactor projections without mutating original/nested content. Also retain fork configurable-cap and owner-cleanup tests.
7. Cost: `TestApplyModelCostEndpointEligibility`, `TestRunStreamServiceTierCost`, `TestServiceTierMixedTurnsPersistCost`; actual-tier downgrade, custom endpoints/overrides, fallback, shared catalogue immutability.
8. Responses: `TestResponseStateDoesNotBypassEdits`, `TestResponseStateKeepsCompletedReasoningItem`, `TestResponseStateUsesResolvedModel`, opt-in/default wire compatibility, multiple assistant phases, recursive encrypted-content redaction and numeric diagnostic fidelity; add fork clone/reload/attachment coverage.

Future ports need focused unit/concurrency checks and normal project validation. **No tests, build, lint, race tests, live incident probes, or patch applicability checks were executed for this assessment.**

## Already covered, skip, or conditional extras

- **Already covered by source, not whole-commit patch equivalence:** cache-token budget accounting [665cd4c87][budget] (`PromptTokens()+OutputTokens`); userconfig lost-update protection [a30d5c604][userconfig]; request-scoped/ambiguous MCP callback routing from `45bb9a779`; encrypted-config recorder scrubbing from `bb6c47495`.
- **API manifest budgets:** `50d783f1c` is already achieved through fork `cmd/root/api.go` → `pkg/host/runtime.go:47,65`, which loads config and supplies both budget options. Do not recreate upstream runtime construction beside the canonical host.
- **A2A resumed limits:** `8378f1350` fixes upstream's nonpersisted tool/history settings. Fork `pkg/session/execution_settings.go` already persists those fields, covered by `TestSQLiteExecutionSettingsRoundTrip`. Changing limits when a manifest changes or migrating older histories is a separate policy decision; do not overwrite canonical state from the adapter.
- **MCP args race:** `5abe396f2` fixes upstream lazy args mutation; fork builds final gateway args before construction. Absence of upstream `argsMu` is not proof of the same bug.
- **MCP optional OAuth store injection:** [0c504265c][oauth-store] is a useful compatibility seam only if a concrete embedding caller needs isolated credentials for the same resource. Fork currently selects the default keyring store. Selectively adapt injection while keeping the existing keyring split and owner lifetime; do not import [b20c28de3][owned-stdio]'s parallel owned-stdio lifecycle merely to obtain it.
- **Harness-only follow-up:** [51956e688][harness-resume] retains a first failed/canceled turn's newly reported thread ID. Fork returns before remembering it, but a literal four-line move is insufficient: fork canonical `commitSessionAttribute` can reject a canceled context. Adapt owner/generation-guarded finalization if harness agents matter. The newer harness dependency from `277b161d5` also has protocol-hardening tests; its external implementation was not inspected. Do not copy the temporary Codex runner that upstream added and then removed.
- **SQLite is storage-topology-dependent:** [101a9b39a][sqlite-mode] selects DELETE inside sandboxes to avoid virtiofs WAL shared-memory incoherence. Fork deliberately uses WAL and immediate transactions; `97df9ab10` puts Kit state on internal `/home/agent/.cagent`, not a host mount. Revisit mode only for a deployment actually storing SQLite on a problematic shared mount. Do not universally switch journals, remove fork transaction safeguards, or attribute a live incident from this comparison.
- **Skip structural churn:** tool wire-package extraction `b3f823697`, dialog extraction `a44c2c94a`, legacy background-task ownership, broad callback/runtime refactors and unrelated toolsets/UI enhancements are not prerequisites for these fixes. No broad architecture adoption is recommended.

## Evidence and boundaries

Detailed committed-source notes: `/tmp/da-inbound-assessment.5ElWKp/{providers.md,tools-safety.md,independent.md}`. History inventory and patch-equivalence output are alongside them. Specific gaps are confirmed by source paths/behavior, not diagnosed incidents. Recommendations remain unimplemented; exact conflict resolution, SDK compatibility and operational effect require a separately authorized port.

[ws-lease]: https://github.com/docker/docker-agent/commit/33acc2845c8ba54014b9a5c131c71a45362bc70e
[ws-close]: https://github.com/docker/docker-agent/commit/a5ff5c68104c67559027814b02d472532d0301d0
[dangling]: https://github.com/docker/docker-agent/commit/76ccf9390723d1494985354749390b6abebc608f
[extraction]: https://github.com/docker/docker-agent/commit/e7fa3958c3be524ec62f10664c20d537c7215f09
[prompt-stage]: https://github.com/docker/docker-agent/commit/50bc87521a33a3bf86dc716927ce2284201bc639
[skill-stage]: https://github.com/docker/docker-agent/commit/96e215d6f4aa833b69abf9921bbafa76f4ff2d63
[skill-read]: https://github.com/docker/docker-agent/commit/868d428d376a5f7779dd04385b42e9ab08b53cc5
[anthropic-close]: https://github.com/docker/docker-agent/commit/e91aadf45be6a5dfcf0e6db32bb0c4b6912ffe76
[anthropic-usage]: https://github.com/docker/docker-agent/commit/7bbc2c614f95615a678d7dfac1d847e913e4b23f
[anthropic-state]: https://github.com/docker/docker-agent/commit/e2715467eb3856c52d71de31661047fd5314eace
[gemini-ids]: https://github.com/docker/docker-agent/commit/acf814cedbfe7804b9657fd63499fce3855bf2c9
[gemini-usage]: https://github.com/docker/docker-agent/commit/bd9b0135e23f13137026fd8792d3d6d3eafe129b
[output-bound]: https://github.com/docker/docker-agent/commit/df389ba218077271901632bf20b0e0ec940ce684
[tier-cost]: https://github.com/docker/docker-agent/commit/07a3a4a739fb75e62bb2b02ad4e3f352b77c67ba
[tier-correction]: https://github.com/docker/docker-agent/commit/af03ff01d6dd6f6986651880c191f3e77ca72f0e
[responses-state]: https://github.com/docker/docker-agent/commit/a3ce461e9b42dad2a5fbc4e803a827da4d76e55c
[budget]: https://github.com/docker/docker-agent/commit/665cd4c8711f41fdef29094b49ac066d32327dda
[userconfig]: https://github.com/docker/docker-agent/commit/a30d5c604160e41019f6ffd45b9442c2f1c055d4
[oauth-store]: https://github.com/docker/docker-agent/commit/0c504265cc6436540ed0898e64c285e6b9b7e866
[owned-stdio]: https://github.com/docker/docker-agent/commit/b20c28de3b8928c786c8341e923907f26b4f159b
[harness-resume]: https://github.com/docker/docker-agent/commit/51956e688d8f0dc270c15eba21bcb6164b735745
[sqlite-mode]: https://github.com/docker/docker-agent/commit/101a9b39a8017b0c599d6c7c731d3b58e0a29972
