# TUI performance architecture decision

## Status and decision

**GO: bounded option A on `subagents-reborn`.** Preserve Bubble Tea, Lipgloss,
Bubbles, the existing event/effect model, and retained application caches.
Implement only the measured tabbar gate and separately justified local residual
fixes. **NO-GO: blanket option B migration solely for performance. NO-GO: a
complete from-scratch option C.** These are current investment decisions, not
claims that either alternative can never be useful.

Evaluation checkpoint: `b683f49bd`; tracked files were clean at evaluation start.
This document authorizes bounded implementation, not production acceptance.
The evaluation itself adds no implementation. Another implementation owner must
complete the acceptance gates below. Earlier inventory language recommending
“B first” is superseded by this decision.

## Fair comparison

| Criterion | A: bounded retained-cache fixes | B: systematic retained dependency architecture | C: replacement toolkit and terminal stack |
|---|---|---|---|
| Scope | Sound local tabbar, toast, and proven root-invalidation gates | Complete typed data/layout/paint/metadata revisions, owner-scoped dependencies, retained regions, next-deadline scheduling | New/adapted components, effects, styling, layout, event loop, input, renderer, and terminal lifecycle |
| Measured support | Isolated prototype removes the dominant unchanged-tick tabbar work | Plausible way to avoid remaining component rebuilds; incremental gain over optimized A not measured | No measured real-workload advantage over optimized A/B |
| Strength | Small, reversible changes using existing caches and tests | Explicit dependency coverage and more predictable clean-event cost across owners | Maximum control over scheduler and terminal rendering contracts |
| Limitation | Local gates require continuing dependency discipline | Real migration: current counters omit media, selection, banners, private editor blink, and suggestions | Broad compatibility surface and replacement maintenance burden |
| Backend benefit | None promised | None promised without a separate backend change | Possible, but must be demonstrated with live terminal evidence |
| Risk/cost | Lowest; stale geometry or metadata still possible | Medium/high; missing revisions can silently retain stale output | Highest; input, effects, terminal protocols, extension API, and lifecycle all move |
| Decision | GO, subject to production parity and performance gates | NO-GO as a blanket performance migration now; retain staged fallback | NO-GO now; require funded contracts and compelling comparative evidence |

B must preserve single-threaded `Update` mutation, effects returning messages,
session identity, and authoritative application snapshots. It is not a second
reactive state store and not a semantic relabeling of A. A future B can migrate
one owner at a time while preserving the existing backend.

## Evidence behind A

The root already caches a complete `tea.View`; messages and panes already retain
bounded output caches. A root cache hit does not prevent work done earlier in
`Update`. The identified cost is tabbar styled geometry/paint used to discover
that a frame has not changed, not absence of all retained rendering.

Fresh external profiling on Apple M3 Max attributed 78.3% of sampled CPU to
`Update`, 76.4% to `TabBar.Tick`, 67.5% to `renderTab`, and 54.7% to
`Style.Render` (overlapping cumulative percentages). `renderTab` accounted for
90.5% of allocated objects. Final fair medians from three samples, with both
baseline and integrated fixtures unwrapped, are:

| Fixture | Clean baseline | Integrated bounded A |
|---|---:|---:|
| Accepted owner tick + `View`, stable glyph, 3 tabs | 208.393 µs, 53,856 B, 1,539 allocations | 1.292 µs, 772 B, 22 allocations |
| 1 tab, stable glyph | 70.559 µs | 1.321 µs |
| 12 tabs, stable glyph | 860.282 µs | 1.302 µs |
| Stable toast | 227.787 µs, 104,751 B, 1,551 allocations | 4.361 µs, 772 B, 22 allocations |
| 16.666667 ms cadence, combined | 635.288 µs | 289.945 µs (54.4% lower) |
| Toast at 16.666667 ms cadence | 807.792 µs | 400.469 µs |
| 120 ms changed-frame cadence | 1,902.427 µs | 1,734.613 µs (8.8% lower) |

All these fixtures use **one visible pane and a long Unicode draft**; tab count
is not pane count. They do not establish the full three-pane/empty-draft matrix.
Cached `View` is about 73 ns (64 B, four allocations). A 1 ns clock advance
means active-but-glyph-stable, not idle. The 120 ms changed-frame fixture and
16.666667 ms cadence fixture answer different questions. Stable-toast timing
deliberately did not execute its expiry command and cannot establish expiry
correctness. Original recording-wrapper and partial-unwrap timings are invalid
as final acceptance data; this fair table supersedes them.

Across 600 baseline cadence ticks, 500 complete views were equal (83.3%);
160 ticks invalidated root (26.7%), while only 100 changed output (16.7%).
The corrected integrated comparison reduces root invalidations from 160 to 100
with the same 100 output changes. This supports the local gate, not indiscriminate
message suppression. True idle had zero active animation owners and pending
application leases. This is not a measurement of zero framework wakeups.

Validation corrections and current results:

- The tick-recording benchmark wrapper can retain records at high iteration
  counts. Final fair benchmarks and parity fixtures unwrap it in **both**
  variants; retain earlier flawed measurements only as diagnostic history.
- Initial active/toast full-view fingerprints differed despite matching counts.
  Investigation identified randomly chosen pending-response spinner text
  (`defaultMessages[rand.IntN]`). A test-only overlay now supplies the same fixed
  label in both variants, without changing production randomness or phase.
  Against `b683f49bd`, the integrated implementation matches all ten SHA256
  serialized complete `tea.View` streams across 600 ticks, including title,
  cursor, colors, modes, and progress; callback fields were verified nil.
  **Deterministic sequence parity passed.** Only the random fixture input was
  controlled: no output content or metadata was normalized away. Retain the
  original failed artifacts as invalid cross-process comparisons.

Targeted tabbar/root prototype tests and corrected sequence parity passed.
Fair results meet the unchanged, toast, cached-view, real-cadence improvement,
and changed-frame budgets for the measured one-pane fixtures. Full normal
`go test ./pkg/tui/...` and `go vet ./pkg/tui/...` passed, as did post-edge
comparison of all ten complete-view streams, component race tests, and focused
race tests for touched root paths. Broader performance-matrix coverage remains
unproven; this is not blanket production readiness or a live terminal CPU claim.

The full race suite is **not green**. Its initial run reported an unsynchronized
lifecycle-test read. A test-only event-loop probe repair passed five repeats;
baseline attempts of one and five repeats did not reproduce that race, so there
is no empirical baseline reproduction claim. The bounded full-suite rerun
reported no data race but failed two behavior/timing assertions:
`ActualProgramTabbarDragDrop` measured 21.75 Hz against a >25 Hz overlay threshold
under instrumentation, and `ActualProgramQueueRemove` retained an active-hint
reminder. Both tests passed in a single combined isolated rerun (6.518 s), but
that does not make the failed full-suite run green. These findings do not
authorize further scope.

Indefinite synthetic loading still costs about 1.305 ms, and an unknown non-tick
`struct{}{}` message plus root `View` costs about 2.822 ms. Neither is a genuinely
unchanged accepted owner tick; neither authorizes blanket message suppression.

## Bounded implementation endpoint

1. Gate tabbar work only when cache validity, theme/color, and the last rendered
   Card frame are valid and unchanged; finite motion, dragging, settling, and
   `scrollPending` must still progress. Geometry and hit-test state must remain
   authoritative during `Update`, not become current only after `View`.
2. Address the measured stable-toast residual separately. External residual
   attribution identifies `syncNotificationAvoidance` as 97.3% of allocations,
   including translated rectangle growth (62.5%), cloning (26%), and textarea
   `Value` (8.8%). Preserve avoidance geometry, commands, and expiry semantics.
3. Gate only proven phantom root invalidations. The reported 60/600 Card cases
   arise from a 125 ms check using `hasRunningPane` without pane-header
   visibility: a single pane has no header. Prove each local visibility rule.
   Unknown/unrelated messages remain conservatively dirty unless absence of
   visible, metadata, and effect changes is demonstrated.
4. Keep durable production-path benchmarks, complete cached-versus-fresh
   `tea.View` parity tests, and lifecycle/owner regression coverage.

No blanket component migration, custom command suppression, protocol semantic
change, or terminal replacement belongs to this endpoint.

## Proposed acceptance criteria

These are decision-policy budgets, not measurements or guarantees. Use warmed
production `Update`/`View`, the same deterministic fixtures, machine, build,
and clock in both variants, repeated samples, and reported spread. Separate
`Update`, cached `View`, composition, and combined costs.

| Gate | Required result |
|---|---|
| Genuinely unchanged, no toast | Representative 1/3-pane and 1/12-tab cases: median target ≤10 µs, cap ≤20 µs; ≤2 KiB and ≤40 allocations per accepted owner tick + `View` |
| Stable toast | ≤20 µs combined and material allocation reduction; measured one-pane result is 772 B versus 104,751 B baseline, with executed expiry still a separate correctness gate |
| Cached `View` | ≤1 µs, negligible allocation (report bytes/count; do not call four allocations zero) |
| Real 60 Hz cadence | Mean combined cost at least 25% lower than clean baseline in the same fixture |
| Changed frames | No >5% regression; repeat noisy comparisons before concluding pass/fail |
| Correctness | Identical complete `tea.View`, including metadata, against fresh rendering and baseline with identical deterministic fixture inputs; do not normalize away content/metadata differences; identical required side-effect schedule |
| Settlement | Zero application animation leases/timers after true settlement; no claim of zero total framework wakes |

The acceptance matrix covers 1 ns, 16.666667 ms, 120 ms, and true idle;
1/3 panes; empty/long drafts; 1/12 tabs; active toast and executed expiry;
theme/color, resize, focus, drag, settling, scrolling, Unicode widths,
indicators, and hit zones. Preserve hidden-pane/owner behavior and bounded cache
retention. Run targeted regressions and races, plus supported build/lint checks.
Report any unsupported check explicitly rather than silently weakening a gate.

## Compatibility and backend boundary

A and B retain toolkit-shaped contracts: `tui.New` returns `tea.Model`,
`core.Resolve` consumes it, and custom tool `Builder` returns `layout.Model`
with an animation runtime. C spans 119 production Tea imports, 72 Lipgloss
imports, and 42 Bubbles imports (overlapping counts), plus the private sequence
adapter, image output writer, coalescer/filter, and terminal lifecycle.

The inspected dependency renderer is unexported and consumes a string-based
view. Its 60 Hz ticker persists during idle. Unchanged alternate-screen flushes
acquire a lock, perform a trivial `NewStyledString` construction, and return at
the equality guard: no ANSI parsing or cell rebuild occurs on that clean path.
Changed flushes can clear and draw the whole buffer. Lean/non-alternate-screen
mode counts newlines before equality. Consequently neither A nor B promises
zero framework wakes or backend parsing strictly proportional to changed
regions. No live backend CPU profile establishes its current cost.

An isolated backend replay of a captured production ANSI frame at 156×48,
writing to a counting discard sink with manual flushes, confirms this boundary.
The retained `frame-a.ansi` contains an active UI, long Unicode draft, 24 history
messages, and test-fixed spinner; replay uses alternate screen, all-motion
mouse, fixed title, truecolor, and the selected production dependency graph.
Unchanged flushes cost 0.051–0.076 µs, 112 B, one allocation, and zero output
bytes. An equal-width `word` → `Word` one-glyph replacement costs 483.3 µs,
about 13.7 KB, 1,164 allocations, and 40 output bytes; title-only changes cost
474.1 µs, about 13.3 KB, 1,140 allocations, and 12 output bytes.
This excludes live TTY, ticker, and provider costs. Small output does not imply
cheap changed-frame drawing; this isolated replay does not establish live UI
CPU share or justify replacing the backend.

Existing E2E use of `WithoutRenderer` and stripped frames cannot validate
terminal protocols. Any backend replacement needs PTY coverage for keyboard,
focus, mouse, paste, Unicode/ANSI, hyperlinks, media, cursor, title, modes,
colors, progress, shutdown, and detach. Native IME/preedit has no established
contract; rune-based selection has only partial grapheme coverage. Treat these
as explicit contract/test gaps, not functionality a rewrite automatically fixes.

## Milestones and stop conditions

1. **Preserve checkpoint and evidence.** Archive baseline, overlays, failed and
   corrected fingerprints, fixture definitions, command lines, and hardware.
   Preserve the corrected unwrapped, fixed-label parity fixtures and use them
   for durable benchmarks. The deterministic sequence parity oracle now passes.
2. **Production-safe tabbar gate.** One reviewable change with explicit geometry
   and cache dependencies. Exit: motion/hit-test/theme parity and durable
   unchanged/changed benchmarks. Stop and fix correctness before broadening.
3. **Separate profile-backed residual fixes.** Treat toast allocation and proven
   phantom invalidations as separate changes. Reprofile after each; retain only
   measured wins with complete output/effect parity. Do not expand to B by drift.
4. **Acceptance and handoff.** Run the full matrix, races, supported build/lint,
   and corrected comparisons. Record remaining backend and terminal-test limits.
   Claim completion only as **bounded A**, not a completed reactive architecture
   or validated terminal-stack optimization.

Each stage is independently reviewable/revertible. Failed gates justify repair,
a narrower scope, or a new decision—not an automatic toolkit rewrite.

## Reconsideration thresholds

- **B:** reconsider if bounded fixes cannot meet the ≤20 µs unchanged cap, or
  application cost demonstrably scales with unchanged history/panes. Also
  reconsider when component rebuilding exceeds 25% of real-60-Hz application
  CPU and a parity-preserving B prototype yields at least 20% additional gain
  over optimized A. Validate revision coverage before trusting counters.
- **Selective backend work:** require a live PTY profile showing renderer work
  at least 25% of total UI CPU after relevant A/B stages, plus a measured
  prototype win. An explicit zero-wake requirement can justify a targeted
  renderer-scheduler patch without justifying complete C.
- **C:** reconsider only if selective application/backend stages cannot meet an
  explicit user budget, a prototype materially improves real workloads (for
  example ≥2× versus optimized A/B), and the migration contract inventory and
  PTY/test program are funded and completed as release gates. No such gain is
  currently measured.

## Evidence provenance

See the durable [empirical performance report](../performance/tui-residual-performance.md)
for measurements, fixture details, and validation results.

Measurements and profile attribution come from the external fresh profiling
team, not runs performed to write this decision. Local evaluation artifacts:
`/tmp/docker-agent-residual-fresh/{isolation,gate,baseline-equality,gate-equality,gate-tests,clean-cpu-cum,clean-alloc}.txt`.
Final fair benchmarks: `/tmp/docker-agent-residual-fresh/fair-{baseline,final}.txt`.
Fixture definition: `pkg/tui/shell_residual_bench_test.go`.
Corrected parity: `/tmp/docker-agent-residual-fresh/{baseline-normalized-parity,final-normalized-parity}.txt`.
Despite those filenames, only the random test input label is controlled; outputs
are not normalized. Source inventory:
`/tmp/docker-agent-architecture-fresh/INVENTORY.md`.
Temporary artifacts are evaluation provenance, not portable release assets;
archive the corrected empirical report with the implementation handoff.

## Subsequent bounded editor/pane experiment

A later authorized, scoped explicit-input retained editor/pane experiment is
tracked in [its separate evidence note](../performance/tui-retained-experiment.md).
That experiment preserves the existing toolkit/backend and does not erase this
historical decision or authorize a blanket option B/C migration. Its same-fixture
baseline is `301d2e290`; candidate acceptance is recorded separately in that note.
