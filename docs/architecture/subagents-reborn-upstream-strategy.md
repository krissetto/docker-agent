# Extracting upstream contributions without dismantling subagents-reborn

> Historical strategy: the six upstream-native runtime proposal branches have since been prepared and signed locally, numbered `01-observe-background-agents` through `06-retain-messaging-subagents`, on upstream base `7a69c316f03c635d8d1951bc4677c59b47beedc7`. They remain separate from this fork, unpushed, with no PRs opened. The recommendations below retain their original assessment context.

## Executive recommendation

Treat `subagents-reborn` as a product and research workspace. Treat upstream contributions as **separately authored, upstream-native derivatives of proven ideas**. Do not rewrite this branch, split its historical commits, rebase its ongoing work onto contribution stacks, or constrain experimentation to upstream-sized parcels.

The source branch is the workshop. A contribution branch is a carefully packed shipment. They have different jobs.

A successful outcome may be an independent product, a set of upstream improvements, or both. Do not make the fork wait for upstream agreement. Conversely, do not ask upstream to accept the entire fork's architecture merely because individual improvements are valuable.

This document is a strategy, not an instruction to create worktrees, branches, commits, or pull requests now. Only this report and its companion [hardening report](subagents-reborn-hardening-report.md) were requested.

## Evidence boundary

The assessment began at local `302f95633`; report preparation observed `38af2c2fb` and concurrent working-tree edits. Preserve that ongoing work. Relevant initial history included three large foundation commits: `44426fa55` touched 402 files, `f792af568` 229, and `3c444d84d` 179. Those commits are useful provenance, not suitable upstream review units.

Cached upstream was `aec69870b`, while the read-only live query found `83fca5b2577d5548c59bde40d5d0b067d532a71a`. The live comparison reported 416 additional commits. Upstream has already advanced overlapping areas including ACP, sandbox Kit support, lean settings, rendering/component boundaries, provider concurrency, and deterministic tests. Candidate contribution boundaries therefore require fresh upstream reconnaissance before implementation.

No upstream acceptance is assumed. No current upstream maintainer preferences beyond observable repository conventions were verified. Numerical review-size suggestions below are planning heuristics, not project rules.

## 1. Principles that protect the creative workspace

### 1.1 Extract ideas, not history

For most parcels, port the behavior into current upstream rather than cherry-picking a checkpoint and then undoing unrelated changes. Large commits contain implicit dependencies, renamed APIs, test helpers, migrations, and assumptions about the fork's ownership model.

Use the fork as design evidence and a reference implementation. Re-express the change using upstream's current contracts and vocabulary. Preserve attribution and applicable license notices. Where the actual patch is independently isolated and still matches upstream, cherry-picking can be appropriate—but prove that before using it as the default mechanism.

### 1.2 Keep product direction independent

Upstream rejection can mean “not a fit,” “not now,” “too much compatibility burden,” or “different architectural direction.” It need not invalidate the fork's product decision.

Keep a contribution ledger separate from the product roadmap. A pending PR must not block improvements to the same idea in the fork. Record semantic divergence and decide later whether to incorporate upstream's version.

### 1.3 One contribution, one problem statement

A parcel needs an upstream-observable problem, not “this file exists in my fork.” It should provide:

- a concrete bug, limitation, or measured workload;
- a coherent implementation that fits the current baseline;
- an independent correctness/performance oracle;
- an explicit compatibility and operational cost;
- and a useful state if no later parcel is merged.

Not every feature can be independent, but dependency should be visible and intentional.

### 1.4 Avoid laundering a rewrite through tiny PRs

A sequence of individually small PRs can still force an unapproved architecture on maintainers. If multiple prerequisites only make sense for persistent async sessions, discuss that destination first.

Foundations should have independent utility or explicit architectural approval. Do not merge dead scaffolding in the hope that it makes the later feature unavoidable.

## 2. A safe extraction environment

When contribution work is authorized, use a separate clone or dedicated external worktree rooted at a pinned current upstream SHA. Prefer a location outside the active source tree to avoid package discovery and generated-artifact interference.

A separate clone offers the clearest isolation: independent refs, index, worktree, hooks, and build artifacts. A Git worktree is lighter but shares repository metadata; creating it does not switch the creative branch, yet it is still an operation to perform deliberately. Neither is to be created as part of merely writing this strategy.

Keep credentials, provider recordings, test databases, images, and release output in contribution-specific directories. Never share the fork's live session database with an upstream candidate without a proven schema contract.

### Suggested lanes

1. **Creative source:** `subagents-reborn`, untouched by contribution bookkeeping.
2. **Contribution baseline:** immutable upstream SHA plus reconnaissance notes.
3. **Independent bug/performance branches:** one per parcel.
4. **Architecture prototype:** an explicitly experimental stack for persistent async sessions, not a disguised ready-to-merge PR.
5. **Accepted integration notes:** a ledger of upstream commits, equivalent fork behavior, and any remaining divergence.

Keep a frozen fork checkpoint for each extracted idea. That is a tag/reference or source archive in the contribution environment, not a demand to stop work on the creative branch.

## 3. Classify candidates before writing code

| Category | What belongs here | Default treatment |
|---|---|---|
| Independent correctness fixes | Reproducible upstream bugs without the fork runtime | Small upstream-native PR |
| Measured local performance fixes | Bounded render/cache/allocation changes with parity | Separate benchmark-backed PR |
| Useful additive capabilities | Session todos, metadata paging, improved approval visibility where appropriate | Design discussion then scoped feature |
| Architectural foundations | Driver ownership, durable accepted input, coordination | Explicit RFC/prototype before a stack |
| Product-specific composition | Tiled workspace UX, makerspace team, opinionated Kit | Keep in fork unless requested upstream |
| Already upstream/superseded | Existing or newer equivalent implementations | No duplicate PR; consider fork adaptation |
| Insufficiently proven | Suspected races, attractive abstractions, unsupported performance claims | Reproduce or measure first |

Use two independent scores: **upstream fit** and **fork value**. An idea can be excellent for the product and poor as an upstream contribution. This prevents reviewability from becoming the product's design compass.

## 4. First wave: high-value, low-coupling parcels

### 4.1 Regression fixes that reproduce on current upstream

Candidate areas from the fork history include stale queued-message editing, input identity preservation, session/page routing, settings lifecycle, image-placement cleanup, Unicode hit testing, and correlated tool decisions.

These are leads, not a verified bug list. Upstream has independently addressed several adjacent issues. For each lead:

1. reproduce it on current upstream;
2. build the smallest failing test using upstream fixtures;
3. determine whether the fork's fix depends on canonical session handles or redesigned widgets;
4. port only the minimal upstream-valid behavior;
5. discard the parcel if the latest upstream already fixes it.

**Typical scope:** a few production files and their tests. A queued-edit identity fix should not import the fork's entire queue editor and session protocol.

### 4.2 Bounded rendering improvements

The fork contains useful mechanisms: tabbar unchanged-tick gates, local occupancy caching, pane/header visibility-aware invalidation, retained Markdown blocks, and bounded prepared-render reuse.

Port one mechanism at a time. The upstream baseline must have the same measured bottleneck. Existing fork reports compare earlier fork revisions, not current upstream; their percentages cannot be recycled as upstream claims.

A performance parcel should include:

- a warmed production-path benchmark;
- representative changed and unchanged cases;
- fixed deterministic inputs in both variants;
- semantic output and metadata parity;
- theme, resize, focus, pointer/hit-test and settling tests;
- allocation/retention measurements;
- and a short evidence report with raw distributions.

Separate application `Update`/`View` costs from terminal renderer/PTY behavior. Do not claim zero framework wakeups, terminal speedup, or provider throughput from synthetic owner-tick tests.

**First-choice candidate:** a small invalidation or allocation fix with an obvious current bottleneck. **Not first choice:** the wholesale Bubbles replacement.

### 4.3 SQLite writer admission and contention

`pkg/session/sqlite_write.go` retries lock acquisition within a bounded cancellation-aware window rather than replaying transaction bodies or ambiguous commits. That distinction has independent potential value.

Inspect upstream's current transaction mode, busy timeout, connection pool, error classification, and writer admission first. A proposed helper must not alter semantics accidentally or create nested retries that exceed caller deadlines.

Tests should exercise two independent database handles, lock release, deadline cancellation, permanent SQL errors, transaction-body failure, and uncertain commit behavior. Avoid adding runtime coordination interfaces to a storage-only fix.

### 4.4 Model/session isolation and policy fixes

Context-scoped model selection and read-only filtering across implicit/deferred/code-mode tools may offer isolated contributions. Upstream has related session model-state and request-scoped changes; check overlap carefully.

Potential parcels:

- a demonstrated concurrent model-selection bleed;
- missing final dispatch filtering for implicit tools;
- deferred discovery that exposes prohibited tools;
- override policy inheritance that mutates shared manifest defaults.

Preserve existing public interfaces unless a breaking change is explicitly agreed. If context is needed, consider an additive path or a coordinated API migration rather than casually replacing a widely used method signature.

### 4.5 Metadata-only session paging

If upstream session browsing hydrates complete transcripts unnecessarily, a metadata-only paging API can stand alone. Preserve root-only defaults, ordering/tie-breakers, supported filtering, and stable pagination semantics. Define whether pagination is snapshot-consistent or eventually consistent while new sessions arrive.

Keep child attachment/restore grants out of simple metadata enumeration. A browser row is not authorization to execute or inspect a session.

## 5. Second wave: useful capabilities with moderate coupling

### 5.1 Session-scoped todos

Separate three concerns:

1. storage ownership and atomic mutation;
2. tool behavior and errors;
3. UI presentation/editing.

Do not combine persistent storage, a breaking `todo.Storage` interface, compact sidebar rendering, dialogs, and optimistic editing into one PR.

A plausible sequence is:

- additive context-aware/session-scoped storage adapters with compatibility policy;
- atomic store operations and deterministic conflict/error tests;
- runtime/tool binding using execution context;
- independently useful UI integration.

If current upstream prefers the existing tool contract, keep the fork's richer public interface local. Prove concurrent updates do not lose writes and child sessions do not unexpectedly share one global list.

### 5.2 Confirmation and dialog usability

The fork's visible decisions while arguments scroll, coherent choice blocks, and retained interaction correlation are useful UX ideas. Upstream has reusable confirmation-component work, so target those boundaries rather than porting the full dialog framework.

Separate correctness from visual preference:

- correctness: a decision responds to the right session/generation/interaction;
- accessibility/usability: focus, scrolling, labels, key behavior;
- styling: spacing, colors, animation.

Visual changes need small before/after captures and explicit keyboard behavior. They should not drag in unrelated themes or notification redesign.

### 5.3 Safe image previews

A useful parcel may share image source uploads across preview resizes or correctly remove stale placements. Keep terminal protocol ownership, cleanup, and capability negotiation explicit.

Do not bundle attachment deduplication, a new preview dialog, all image hit testing, and backend changes unless they are truly inseparable. Include real terminal tests where protocol correctness is the point.

## 6. The architectural parcel: persistent async sessions

This is the fork's most valuable and most difficult upstream contribution. It should start as a design discussion and executable prototype, not a thousand-file pull request.

### 6.1 Present the user outcome before the implementation

Describe the desired experience:

- a declared child starts without blocking the parent;
- parent and child can continue across turns;
- child completion is delivered without polling;
- cancellation, permanent stop, and shutdown have distinct meanings;
- accepted inputs and reports survive supported restarts;
- multiple frontends observe the same execution owner;
- slow viewers do not control execution lifetime;
- resources and permission inheritance are bounded.

Compare this with upstream's current background-agent, transfer-task, handoff, session, and join semantics. Explain what remains unchanged. Do not propose replacing every delegation mechanism just because the fork unified them differently.

### 6.2 Questions requiring agreement

Before splitting the feature, seek decisions on:

- whether persistent bidirectional children belong in upstream's product;
- whether one stable session execution owner is the desired foundation;
- minimum Go SDK/API compatibility guarantees;
- durable versus volatile backends and capability negotiation;
- recovery semantics after promotion and process death;
- report delivery, parent wake rules, and descendant quiescence;
- data migration, permanent-stop, and deletion policy;
- permission and approval inheritance;
- resource defaults and supported deployment modes;
- whether HTTP v2 is required now or can be additive later;
- and which frontend receives first-class support initially.

These are product and ownership choices, not just implementation details. If agreement fails, preserve the implementation in the fork and contribute independent repairs instead.

### 6.3 A possible dependency graph

The following is conditional on agreement and current-source reconnaissance:

```text
A. Behavior contracts + executable failure scenarios
                     |
B. Accepted-input identities / atomic store primitives
                     |
C. Single-session execution ownership + settlement barrier
                     |
D. Child identity/outcome + transactional report delivery
                     |
E. Async child tools and bounded scheduling
                     |
F. Minimal CLI/headless integration + observation
                   /   \
G. Additive HTTP surface H. Minimal TUI child visibility
                   \   /
I. Wider adapter compatibility and restart/soak qualification
```

Cross-cutting policy/model isolation and resource controls must land before features that rely on them. Do not defer capability enforcement to the final polish stage.

#### A. Contracts and executable scenarios

Write tests/specification for single-owner execution, duplicate requests, ambiguous acknowledgements, cancellation, stop, restart, and slow observers. Tests can initially support the prototype branch rather than land as disabled production scaffolding.

#### B. Store primitives

Introduce only the persistence capabilities genuinely needed: stable append identities, atomic accepted-input batches, and explicit durability negotiation. Keep schema changes coherent and tested independently. Avoid introducing every future coordination table at once.

#### C. Execution ownership

This is the riskiest parcel. Demonstrate one authority without changing every UI at the same time. Keep existing entrypoint contracts through thin adapters if possible. Do not let transitional adapters become second queues or lifecycle owners.

#### D. Child records and delivery

Add revisioned child identity/outcome and an atomic report outbox/inbox acceptance path. Prove report deduplication and stop finality. Treat topology as a projection.

#### E. Tools and scheduling

Add spawn/send/read/stop only after ownership and delivery are sound. Include direct-child authorization, resource limits, fairness, and no-polling guidance. Keep tool vocabulary aligned with upstream decisions.

#### F–I. Integrations

Start with the smallest useful entrypoint. Then add transport and UI capabilities as separately reviewable consumers of the same owner. Reuse upstream's existing app/TUI contracts where possible. A large panel redesign is not a prerequisite for showing a child's state and transcript.

### 6.4 Stack discipline

Every merged stage should leave supported behavior correct, without dormant half-features. If that cannot be achieved for a dependency boundary, keep the stack in a prototype until it can be reviewed as a coherent unit.

A reviewable unit is defined by an invariant, not an arbitrary 300-line cap. Store migrations, transition logic, and their tests can legitimately be substantial. The reviewer should still be able to state exactly what changed and why no other authority changed.

## 7. What I would deliberately keep in the fork

### Tiled pane/workspace composition

This is a product differentiator and a large interaction surface: layout, drag transactions, routing, hydration, focus, persistence, sidebars, and compositor caching. Do not force it through the persistent-subagent proposal. Offer it later if upstream wants this experience.

### Wholesale owned-widget migration

The evidence shows useful bounded gains, but replacing an input toolkit also assumes responsibility for Unicode, editing semantics, selection, accessibility, and maintenance. Offer small fixes or the pure text core as a separate discussion only when there is a clear demand. Dependency removal alone is not a compelling upstream user benefit.

### Opinionated Kit and agent-team packaging

Upstream now has related sandbox/Kit work. The fork's team prompts and packaging can remain a distinct distribution or example product. Extract only independently valuable compatibility fixes, provider configuration corrections, or build support that fit current upstream.

### Theme catalog and micro-polish

Keep these decoupled from runtime proposals. Theme changes are cheap to maintain locally and can be offered independently if there is user interest.

### The full API/runtime replacement

An additive capability may be welcome; a wholesale replacement of current SDK and HTTP surfaces is a much larger decision. The fork's legacy compatibility layer is evidence of seriousness, but it also demonstrates the cost. Do not presume API v2 must accompany every upstream improvement.

## 8. A contribution dossier template

For every candidate, maintain a short dossier in the contribution workspace:

```text
Title / upstream problem
Current upstream baseline SHA
Frozen fork reference SHA and relevant source paths
Reproducer on current upstream
User-visible outcome and non-goals
Existing upstream implementation / overlapping work
Smallest proposed implementation boundary
Dependencies and public API/schema changes
Correctness invariants and failure semantics
Security/resource implications
Validation commands and result manifest
Performance baseline/candidate distributions, if relevant
Rejected alternatives and why
Attribution/licensing notes
Status: investigate / prototype / discuss / PR / accepted / declined
Fork disposition after upstream decision
```

The dossier prevents a reviewer from needing to understand the entire fork to judge a small change.

## 9. Validation and presentation rules

1. Test against a pinned current upstream baseline, not the stale cached ref.
2. Use fresh contribution-specific source/build/data directories.
3. Make the reproducer fail before the fix where feasible.
4. Include supported build/lint/test commands and report blockers plainly.
5. For concurrency, use deterministic barriers/synthetic time plus meaningful race checks; avoid sleep-driven evidence when a precise signal is available.
6. For storage, test independent handles/processes and ambiguous commit acknowledgements; never “fix” them with transaction-body replay.
7. For UI, cover semantic identity, effects, metadata, and hit zones—not just screenshot similarity.
8. For performance, compare identical fixtures with repeats and disclose retention/backend limits.
9. For migrations/public APIs, document backward compatibility and unsupported combinations explicitly.
10. Keep the PR description about the upstream problem. Link to fork evidence as supplementary provenance, not as a substitute for a current reproducer.

## 10. Managing the relationship over time

### Start with evidence, not a sales pitch

A small independently useful contribution builds credibility. It should not be a bargaining chip for the big feature. Maintainers need to see that the author understands upstream's constraints as well as the fork's vision.

For architecture, lead with a concise RFC and an optional prototype. Ask focused questions about ownership and product semantics. Avoid asking maintainers to review a giant diff merely to discover the proposal.

### Track accepted ideas without forcing convergence

After an upstream merge, record:

- upstream SHA and behavior;
- which fork code is equivalent;
- whether to adopt upstream's implementation, retain a deliberate fork variant, or remove duplication later;
- tests demonstrating equivalence/divergence.

Do not churn the creative branch just to mirror upstream style. Conversely, do not preserve an inferior fork implementation out of attachment to its origin.

### Define a maintenance budget

Every extracted public capability creates future obligations. Limit parallel PRs to what can be kept current, answered, and tested. A small number of well-supported parcels is better than a stack of abandoned feature proposals.

Review-size heuristics: independent fixes often fit in one to five production files; performance fixes should generally isolate one ownership/caching mechanism; architecture stages should have one crisp invariant. These are heuristics, not hard limits or excuses to omit tests.

## 11. Recommended sequence

### Phase 1: reconnaissance and independent wins

- Refresh contribution knowledge of live upstream in an isolated environment when authorized.
- Identify already-merged equivalents and remove duplicate candidates from the queue.
- Choose one reproducible correctness fix and one measured low-coupling performance fix.
- Prepare standalone tests/dossiers and contribute them without the async architecture baggage.

### Phase 2: useful additive capabilities

- Evaluate session-scoped todo storage, metadata paging, or policy isolation against current upstream needs.
- Discuss public interfaces before porting breaking fork APIs.
- Keep storage/runtime/UI parcels separated where each remains useful and correct.

### Phase 3: persistent async session proposal

- Produce the guarantee matrix and executable prototype.
- Resolve product/ownership questions with maintainers.
- If there is agreement, build an upstream-native dependency stack elsewhere.
- If not, keep advancing the independent product and contribute smaller improvements opportunistically.

### Phase 4: optional product UX contributions

- Offer pane/workspace or widget work only on its own merits and after demand is established.
- Keep opinionated distribution/team packaging separate from runtime library proposals.

## Final judgment

The fork should not be chopped up on the branch where it is alive. Its history is an experimental record, not a defective pull request that must be repaired.

For upstream, extract the strongest behaviors into fresh, purpose-built contributions with current reproducers, narrow authority changes, and explicit compatibility costs. Start with independent wins; discuss the execution architecture honestly; leave product-specific composition local unless upstream asks for it.

This preserves both forms of value: a place where true creation can continue, and a path for good engineering to travel beyond it.
