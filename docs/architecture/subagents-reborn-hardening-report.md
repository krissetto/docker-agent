# Making subagents-reborn a well-oiled machine

> Historical assessment: the findings and proposed program below refer to the checkpoints named in this report. Later runtime, recovery and provider/security fixes supersede parts of it; this is not a current defect inventory or authorization for the entire proposed program.

## Executive judgment

The fork has the right ambition and several of the right foundations: explicit session ownership, persistent asynchronous children, stable completion identities, atomic report acceptance, bounded event replay, and user interfaces that acknowledge multiple simultaneous conversations. The next leap is not another layer of functionality. It is making those foundations predictable under failure, understandable to another engineer, measurable under realistic load, and safe to operate for long periods.

My recommendation is to develop a **small, trustworthy execution kernel inside a deliberately expansive product**. Keep the experimental branch as the place where new ideas meet. Do not dismantle it to satisfy upstream review conventions. Instead, establish a release discipline that distinguishes an experiment from a supported guarantee.

A new product is a legitimate outcome. Upstream acceptance is neither its definition of success nor a prerequisite for hardening it.

This report proposes work; it does not authorize or implement production changes. The separate [upstream extraction report](subagents-reborn-upstream-strategy.md) describes contributions prepared elsewhere.

## Evidence and limits

The initial assessment inspected local commit `302f95633`. During this report's preparation, HEAD had advanced to `38af2c2fb` (`Reject stale queued message edits`), with active uncommitted editor/sidebar changes. Those changes belong to ongoing work and were not modified. Consequently, file sizes and findings are checkpoint observations, not a frozen audit of every subsequent edit.

The cached upstream reference was `aec69870b`; a read-only remote query found live main at `83fca5b2577d5548c59bde40d5d0b067d532a71a`. GitHub reported 416 additional upstream commits beyond the cached reference. The initial fork-side comparison contained 1,095 changed files and approximately 152,000 added lines, including 83,500 added Go-test lines and 63,400 added production-Go lines.

Source inspection included the session driver, public session API, lifecycle, scheduler, coordination store and delivery, event replay, tree observation, SQLite writer admission, host shutdown, selected fault-injection tests, and existing performance reports. No builds, tests, races, benchmarks, database probes, or live-provider scenarios were run for this report. Historical validation statements remain historical, and proposed investigations are not confirmed bugs.

## 1. Decide what the machine promises

Before optimizing or refactoring, write a concise behavioral contract. It should be enforceable, not aspirational.

### Recommended supported guarantees

1. A successful durable submission has one stable identity and is retained according to the documented crash-recovery policy.
2. At most one admitted execution generation operates on a given session at a time.
3. Retrying an ambiguous admission or completion acknowledgement does not invent another input, transcript row, or child report.
4. A turn is reported as durably settled only after the relevant persistence barrier and child/outbox commit succeed.
5. A permanent child stop is distinct from process shutdown, session-view closure, cancellation of one turn, and execution-driver retirement.
6. Slow or disconnected observers cannot indefinitely stall unrelated execution; they receive an explicit recovery condition where the protocol supports one.
7. Resource admission has explicit limits, failure responses, and fairness semantics.
8. A frontend is a projection and interaction client, never a competing execution authority.
9. Unsupported storage capabilities fail honestly or advertise volatile operation. A saved topology snapshot is not sufficient evidence of durable execution.
10. Security boundaries are enforced before dispatch, including implicit and deferred tools.

### Guarantees not to imply

Do not promise exactly-once external effects merely because reports are deduplicated. A shell command, remote API mutation, or file write can complete immediately before a process dies. An execution restart cannot generally infer whether that external effect happened.

Separate these concepts in documentation and APIs:

- input accepted durably;
- input promoted for execution;
- execution started;
- tool effect attempted/confirmed/unknown;
- model output persisted;
- turn settled;
- parent report accepted.

The existing pending-input recovery tests are valuable, but promotion clearing a pending flag before provider consumption makes the crash policy at that boundary especially important. A promoted request that loses its process needs a documented outcome: abandoned with explicit notification, retried by policy, or reconciled from a durable execution record. Do not infer which outcome exists from pending flags alone.

**Acceptance:** a short guarantee matrix names the authority, persistence point, failure behavior, and test oracle for each promise. Every documented guarantee has a corresponding contract test.

## 2. Priority-zero work: repair the foundations before polishing them

### 2.1 Triage missing upstream correctness and security fixes

Maintain a living compatibility ledger with four classifications: applicable unchanged, applicable through adaptation, superseded by a proven fork design, and intentionally unsupported. Record source commit, affected path, rationale, fork test, and disposition. A different implementation is not automatically evidence of supersession.

Two concrete inspection findings deserve immediate attention:

- `pkg/model/provider/openai/ws_pool.go` retains a single connection while streams use it, with a comment requiring callers not to overlap requests. The pool mutex serializes stream creation, not stream consumption. Upstream has exclusive leases for overlapping streams. The fork also writes `inner.done` directly during pooled stream closure, whereas upstream has synchronization. Reproduce shared-provider overlap, cancellation, and close/read interleavings; then adapt the upstream ownership fix or prove an equivalent isolation boundary. This is conditional on WebSocket transport and provider sharing, not a claim that every concurrent run currently fails.
- `pkg/runtime/budget.go` counts `InputTokens + OutputTokens`, while cached upstream's fix uses `PromptTokens() + OutputTokens` to include cache tokens. Specify token-budget semantics and adopt inclusive accounting where that is the intended budget. Test uncached, cached-read, cache-write, and mixed usage, not just an integer counter.

Also triage compaction charging, stream closure, session-scoped authentication, request callbacks, filesystem confinement, and sandbox staging. Live upstream has evolved in these areas. Treat each as an applicability investigation, not as permission to blindly merge a large update into active work.

**Acceptance:** targeted tests fail on the old applicable behavior and pass on the adapted behavior; remaining upstream fixes have explicit dispositions. No “we are current” claim based solely on ancestry.

### 2.2 Establish a current-checkpoint validation result

The performance documents are unusually candid and useful, but they establish particular revisions and fixtures. They do not certify current HEAD.

Create a validation manifest for a frozen release candidate: source SHA, patch digest if any, OS/architecture, Go/tool versions, command, exit code, duration, test seed, and retained artifacts. Distinguish source failures, environmental blockers, flaky assertions, and timing budgets.

Run the normal supported package suite, production builds, lint/vet, targeted races, and a broader race program at that checkpoint. Add real entrypoint coverage for CLI, HTTP, embedded chat, ACP, and whichever other adapters are supported. Do not turn an isolated rerun into a green full-suite result.

The known ignored `dist/` artifact issue in historical evidence calls for a clean-source build environment, not deleting user artifacts. Release validation should run from an isolated source snapshot without experimental output directories.

**Acceptance:** one current candidate has an explicit green/blocked/failed matrix. Red checks remain red until repaired or a supported scope is genuinely changed and documented.

### 2.3 Protect database identity and evolution

The fork introduces several migrations and persistence capabilities. Fork/upstream divergence can create schema collisions or misunderstandings even when migration integers look familiar.

Compare migration identities and SQL definitions with the intended upstream baseline before sharing databases. Define whether fork and upstream can safely open the same database. If not, use separate data directories by default and provide explicit import/export, not opportunistic reuse.

Test migration interruption, unknown newer schema rejection, rollback after partial failure, reopening after upgrade, old session adoption, malformed references, and preservation of model settings/todos/media/child records. Back up before migrations that cannot be reversed reliably.

**Acceptance:** supported upgrade paths are fixture-tested. Unsupported downgrade or cross-product reuse fails before mutation with a useful message. Recovery instructions identify the exact database and backup procedure.

## 3. Make the execution kernel easier to reason about

### 3.1 Keep one authority, but divide responsibilities

At inspection, `session_driver.go` was 1,917 lines, `session_handle.go` 1,174, and `subagents.go` 1,209. The issue is not a numerical line limit. It is how many independent invariants one edit can disturb.

The driver combines generation lifecycle, admission, pending/steering inputs, persistence recovery, compaction, skills, model bindings, pause state, interactions, subscriptions, and reclamation. Preserve authoritative ownership while making these responsibilities visible:

- lifecycle/admission transitions;
- accepted-input mailbox and promotion;
- maintenance operations and reservations;
- interaction correlation;
- model/execution settings;
- settlement and persistence barriers;
- observation and immutable snapshots.

Start with behavior-preserving internal extractions and contract tests. Do not create separate writable state stores for each responsibility. Do not make a clean-looking abstraction that simply hides a lock inversion.

### 3.2 Tell the truth about concurrency

The driver comment describes an actor with an owner goroutine, but the implementation includes mutex-mediated transitions across cooperating goroutines. Either document that model accurately or deliberately evolve toward serialized transition commands. Do not undertake a wholesale actor rewrite just to match a label.

Write a lock-order and callback policy covering registry `runMu`/`mu`, driver `mu`, manager transition/restore/metrics/persistence locks, journal locks, and store transactions. Identify allowed nesting, external I/O under locks, and callbacks that must run after unlocking.

Storage under a driver lock may be necessary for a linearization point, but it can also turn a slow write into broad responsiveness loss. Measure lock-hold time and blocked admission before changing it. If a reservation/commit/publish sequence replaces the lock, fence generations and prove cancellation/stop interleavings.

**Acceptance:** another engineer can trace submit, cancel, settle, stop, and restore from a small set of named transitions; tests cover the linearization boundaries; no arbitrary callbacks execute under ownership locks.

### 3.3 Narrow capability surfaces carefully

`SessionHandle` is a broad public interface, including execution, models, thinking, skills, todos, compaction, and presentation information. This creates implementation burden for local, remote, and embedded handles.

Define a stable minimum execution/observation contract and optional capability interfaces. Group capabilities by genuine transport/support boundaries, not one interface per method. Preserve compatibility through adapters or a documented versioned transition. Keep unsupported defaults explicit; never let fallback implementations return success for an operation they did not perform.

**Acceptance:** a minimal remote or embedded implementation can satisfy the core contract without dozens of meaningless methods, and clients negotiate optional features consistently.

## 4. Prove crash recovery, not just retry recovery

Existing lost-acknowledgement and FIFO tests are strong. Extend them to actual process death.

Use a subprocess harness with instrumented failpoints around:

1. accepted-input commit;
2. mailbox publication;
3. promotion commit;
4. provider request start;
5. tool effect dispatch;
6. transcript append/update;
7. child outcome/outbox commit;
8. parent inbox acceptance;
9. generation settlement publication;
10. stop/delete/migration commit.

Kill the subprocess at each boundary, reopen the database in a fresh process, and inspect externally observable outcomes. The harness should retain the database, action log, seed, and failpoint on failure.

For side-effecting tools, add an explicit uncertain-outcome policy. Where a tool supports idempotency keys, supply stable operation identities. Where it does not, expose uncertainty rather than silently replaying a dangerous action. This may require a durable tool-attempt ledger for supported recovery features; do not introduce it without a clear operational contract and retention policy.

Test subtree stop versus concurrent settlement and restart. Shutdown must not persist a permanent child tombstone. Deletion must not leave reports capable of resurrecting removed identities.

**Acceptance:** recovery matches documented semantics at every tested boundary. No accepted work disappears without an explicit terminal/uncertain disposition. No claim of exactly-once effects exceeds what the tool protocol can guarantee.

## 5. Treat resource control as a product feature

The inspected defaults include 1,024 sessions, 100 active descendants globally and per root, depth three, 64 mailbox messages, 1,024 replay events, and 8 MiB replay bytes. These are finite, but count limits alone do not establish an affordable machine.

### 5.1 Add aggregate accounting

Measure and bound:

- accepted input bytes, including attachments and multimodal content;
- all retained replay data, including duplicate ownership paths;
- accumulated in-flight assistant content/reasoning;
- topology/transcript snapshots and clone traffic;
- live subscriptions and tree-observation goroutines;
- persistence journals, undelivered reports, and retention tables;
- provider connections and simultaneous requests;
- rendering caches and image memory.

The 8 MiB replay setting can represent roughly 8 GiB across 1,024 fully occupied session hubs before other memory, if all are resident and reach the configured cap. This is a planning bound, not an observed allocation. Inspect actual residency, eviction, sharing, and global accounting before choosing defaults.

Provide conservative desktop defaults and opt-in high-throughput profiles. Unlimited sentinels should be conspicuous and documented.

### 5.2 Measure scheduling fairness and contention

The registry scheduler rotates sorted driver IDs and scans drivers. Descendant admission also scans resident drivers and walks ancestry. That is reasonable at small scale; it deserves measurement at the advertised limits.

Profile admission latency, pending-input age, completion retries, and report delivery across one busy root and many competing roots. Investigate whether synchronous store operations in the shared retry pass let one slow database operation delay all other sessions. Inspect `observeTreeWith` scaling and descendant observation teardown; it attaches observations per session and uses forwarding goroutines.

Optimize with ready queues, ancestry indexes, or per-root quotas only if measurements justify them. Any index must be a rebuildable projection of canonical ownership, not another capacity authority.

**Acceptance:** bounded memory under saturation, explicit capacity errors, documented fairness, and no starvation in sustained mixed-root tests. Report p50/p95/p99 latency and queue age, not just maximum concurrency reached.

## 6. Close the safety gap between intent and enforcement

Async agents amplify permission mistakes because several sessions can act while the human is looking elsewhere.

Build a threat model spanning manifest loading, child spawn grants, working-directory inheritance, read-only hints, code-mode inner tools, deferred activation, MCP callbacks/authentication, remote session attachment, generated media, filesystem symlinks, shell execution, and terminal output.

Important distinctions:

- `ReadOnlyHint` filtering trusts annotations; it is not an OS sandbox.
- Prompt guidance and notification wrappers identify provenance but do not neutralize malicious model/tool content.
- Cancellation does not undo an external effect.
- Parent/child topology does not itself grant permission to attach or mutate a session.

Verify authorization at the final dispatch boundary after implicit-tool composition. Test child attempts to reach siblings, unrelated roots, undeclared agents, or broader filesystem scopes. Correlate approvals with session, generation, interaction, and immutable tool arguments. An edited or replaced request must not inherit approval accidentally.

If the HTTP server is exposed beyond a trusted local boundary, define authentication, origin/CSRF policy as applicable, maximum payloads, path disclosure rules, and per-client limits. Do not silently expand the deployment threat model.

Audit terminal escape sanitization separately from normal ANSI rendering; model output is untrusted, and image/link protocols have their own boundary.

**Acceptance:** an adversarial capability matrix proves denials at dispatch and transport boundaries, with clear distinctions between trusted-local operation and supported remote deployment.

## 7. Make diagnosis possible without reading the whole source tree

### 7.1 Structured lifecycle observability

Every important transition should expose session ID, root ID, child ID where relevant, turn/request ID, generation, interaction ID, report/write identity, phase, duration, and reason. Use one correlation vocabulary across runtime, storage, HTTP, and UI.

Record useful bounded metrics:

- submit/admission and settlement latency;
- pending age and mailbox bytes;
- scheduler lag and capacity denials;
- provider active connections and cancellation time;
- journal bytes, storage retry count, and barrier age;
- SQLite lock-acquisition latency and transaction duration;
- replay gaps and observer disconnects;
- restore/reclaim duration and retained resources;
- root/child spend and unpriced usage.

Do not put session IDs in unbounded metric labels. Use traces/logs for high-cardinality identities. Redact prompts, credentials, model payloads, and sensitive filesystem paths by default.

### 7.2 A safe diagnostic snapshot

Provide a non-mutating support snapshot with version, schema, resource policy, resident session states, queue depths, oldest blocked operation, and sanitized ownership relations. It should not hydrate sessions or trigger execution while diagnosing them.

Expose persistence recovery explicitly: a generic spinner is insufficient when a turn is waiting for durable settlement. Show accepted versus executing versus recovering versus settled.

**Acceptance:** a stalled session can be explained from a diagnostic snapshot and a correlated trace without reproducing it interactively or dumping private conversation content.

## 8. Harden the frontends without turning them into another kernel

Use one cross-entrypoint contract suite for submit, steer, cancel, retry, pause, approval, reconnect, queue edit, and detach/restore. Run it through local handles, remote handles, HTTP v2, legacy adapters, and supported frontends. Compare semantic events and final state, not only screenshots.

Keep input editing, selection, hover, and render caches local. Keep canonical execution state outside widget models. Require monotonic revision/identity checks for asynchronous completions and queued edits.

The owned text core creates a maintenance obligation: grapheme segmentation, combining marks, emoji/ZWJ, wide characters, word boundaries, CRLF, paste, selection, and viewport geometry need durable fixtures and fuzzing. Native IME/preedit support should be explicitly supported or explicitly outside the contract.

Add real PTY tests for modes, title, cursor, paste, focus, mouse, Unicode, hyperlinks, image protocols, resize, detach, and shutdown. `WithoutRenderer` tests and semantic styled-output comparators are useful but do not prove terminal protocol behavior.

Expand performance scenarios to long histories, growing streams, many active children, multiple panes, images, observer reconnects, and slow storage. Existing benchmark improvements are relative to earlier fork checkpoints, not proof of superiority to live upstream.

**Acceptance:** cached and fresh semantic rendering match after perturbations; real terminal lifecycle tests pass; memory stabilizes in long-lived sessions; performance claims state fixture, baseline, hardware, and limitations.

## 9. Operationalize testing and releases

### A practical test pyramid

- **Per change:** relevant contract tests, static checks, deterministic race-sensitive tests.
- **Per candidate:** full supported unit/integration suite, production builds, platform matrix, selected full-package races.
- **Scheduled:** broader races, property/fuzz tests, crash matrix, saturation, multi-hour soak, PTY/provider-compatibility tests.
- **Before promotion:** clean-source reproducibility, database upgrade/recovery, package/install/sandbox smoke tests, retained validation manifest.

Use synthetic time and controllable providers for lifecycle timing where appropriate. Use wall-clock/PTY tests only for behavior that actually depends on real scheduling or terminal transport. Never hide correctness failures with generous timing thresholds; separate throughput budgets from state invariants.

Track flake rate. Quarantine is a temporary operational state with an issue and owner, not a permanent greenwashing mechanism.

### Preserve creative freedom with explicit promotion

Keep `subagents-reborn` active and experimental. Periodically select a tested immutable checkpoint for a candidate/release line created elsewhere. This provides users a reproducible artifact without imposing upstream-sized commits on the workshop.

For an independent product, establish distinct branding/versioning, data directories, update channels, support boundaries, schema policy, and credential handling. Separate experimental and stable settings where needed. Provide export/backup tools so trying a candidate does not trap users' histories.

**Acceptance:** a release artifact corresponds to a known source and validation manifest; rollback behavior and data compatibility are explicit; users can distinguish experimental builds from supported ones.

## 10. Prioritized delivery program

| Priority | Parcel | Completion evidence |
|---|---|---|
| P0 | Upstream concurrency/accounting/security applicability ledger and selected repairs | Regression tests, verified adapted fixes, explicit remaining dispositions |
| P0 | Current frozen-candidate validation | Full result matrix with retained commands/artifacts, no hidden red checks |
| P0 | Durable guarantees and database compatibility policy | Contract matrix, migration fixtures, promotion-crash disposition |
| P1 | Crash/fault harness and external-effect uncertainty | Fresh-process recovery at named commit boundaries |
| P1 | Global byte/resource accounting and conservative profiles | Saturation tests, bounded resident memory, fair queue-age results |
| P1 | Correlated diagnostics and recovery UX | Sanitized snapshot explains blocked work; traces span all authorities |
| P1 | Capability/approval boundary tests | Adversarial dispatch/transport matrix |
| P2 | Behavior-preserving driver/API simplification | Easier transition tracing, parity tests, no new execution authorities |
| P2 | Frontend semantic contract and PTY coverage | Cross-entrypoint parity and terminal lifecycle passes |
| P2 | Reproducible candidate packaging and upgrade/export tooling | Clean builds, platform smoke tests, documented recovery |
| P3 | Scheduler/rendering/storage performance refinements | Measured realistic-load improvement with parity and bounded retention |

Dependencies matter more than calendar promises. Do not start a kernel redesign, backend replacement, or new database architecture merely because it would look cleaner. Require a concrete failure mode or measurable bottleneck first.

## Final recommendation

Protect the creative branch, but stop making release confidence depend on the creator's intuition alone. Preserve the strongest work—single ownership, honest durability, stable identities, and useful multi-session UX—and surround it with reproducible failure tests, resource budgets, current upstream triage, and operational visibility.

A hardcore machine is not one that never encounters uncertainty. It is one that knows exactly what it accepted, what it attempted, what it committed, what it cannot know, and how to recover without lying to its operator.
