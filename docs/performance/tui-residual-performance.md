# Residual TUI empirical diagnosis and targeted implementation

## Provenance and scope

Baseline `b683f49bda9f55819d688fc35610ed5b864fa9ba`, branch `subagents-reborn`; the candidate is the targeted implementation accompanying this report. Apple M3 Max, macOS arm64, Go1.27.1, benchmark default GOMAXPROCS16 (`-16`). All CPU-heavy measurements serialized. Samples: three500ms benchmark repetitions unless specified. Numbers are **microseconds**, not milliseconds, except explicitly labeled loading/changed paths. No live provider, terminal capture, terminal emulator latency, wall-clock scheduler utilization, or token-arrival throughput was measured.

The implementation covers tabbar invalidation, editor occupancy/root notification allocation, and visible pane-header animation. It adds durable benchmarks and regression tests, and repairs test-only synchronization in `session_lifecycle_acceptance_test.go` after a full race run identified a concurrent test read. Local worktrees and debug logs were left untouched.

## Attribution and decision

Exact original clean no-toast one-pane/long profile:219.808µs,53,931B,1539alloc/op. CPU cumulative percentages: Update78.31%; TabBar.Tick76.36%; computeLayouts/renderTab67.46%; lipgloss.Style.Render54.66%. Allocation objects: renderTab90.50%,computeLayouts90.94%. These nested cumulative figures are not additive. `clean.cpu`, `clean.mem`, `clean-cpu-cum.txt`, `clean-alloc.txt` contain evidence. Owner Update invokes six full tab-list layout/style passes on a stable busy tick: hasAnimatedIndicator twice, reconcileScroll once, syncIndicatorSub twice, render once. Root View was already cached:~0.078µs,64B,4alloc; a new root output gate cannot remove Update work.

Prototype tabbar-only gate brought clean221.2→1.583µs,1539→23alloc; toast232.8→15.964µs; real16.7ms637.9→429.8µs. Those early numbers used recording wrappers and random spinner labels: useful attribution, **not the authoritative final comparison**. Prototype toast allocation profile attributes97.27% bytes cumulatively to syncNotificationAvoidance: translated-slice append62.48% direct, cloned rectangles25.97%,textarea.Value8.84% (nested percentages overlap). Manager already compared cells before cloning.

The final tabbar gate checks cached render validity, theme/agent colors, no finite animation, no drag pending/active, no settling drop or pending scroll, and either no active visible indicator subscription OR the same last-rendered Card frame. It does not skip authoritative setters, pointer geometry or subscription reconciliation. Last-rendered frame is recorded only on actual render, not every tick. The inactive-sub alternative prevents repeated layout on another component's clock when all busy tabs are offscreen. No toolkit replacement or generic signals layer is supported by these results.

## Authoritative bounded same-fixture comparison

`fair-baseline.txt` and `fair-final.txt`: EXACT same durable `BenchmarkShellResidual` source, baseline production files supplied via `pristine-production-overlay.json`. One visible pane,24 bounded mixed history messages per session,156x48,long2080-rune draft,focused editor;1/3/12 means **tab-strip count**, not pane count. Twelve tabs include DTO clones, not twelve active providers. StreamStarted keeps spinner work alive; it is not token streaming.240warm ticks; clean prep advances5ms, then1ns accepted owner ticks with elapsed<1ms and unchanged Chat/Card indices asserted. Real cadence16666667ns advances virtual time, does not sleep. No non-owner command execution, so no real editor blink callback or toast expiry. Real samples mix changed and equal outputs.

Important fixture repair: paneReplayRoot warmup can replace `paneReplayPage` with `splitRecordingPage`; both must be unwrapped to avoid unbounded recorded ticks. Corrected baseline AND final were rerun. `DISCARDED-*` logs are not final evidence. `final-clean-cpu.txt` confirms no recording wrapper in measured dominant paths.
| Case | baseline µs (range) | final µs (range) | B/op baseline → final | allocs/op baseline → final |
|---|---:|---:|---:|---:|
| CleanCombined | 208.393 (207.332–214.732) | 1.292 (1.288–1.298) | 53856 → 772 | 1539 → 22 |
| CleanUpdate | 214.442 (210.528–215.196) | 1.218 (1.214–1.230) | 53792 → 708 | 1535 → 18 |
| CachedView | 0.074 (0.073–0.074) | 0.073 (0.073–0.074) | 64 → 64 | 4 → 4 |
| RealCombined | 635.288 (634.360–654.105) | 289.945 (288.212–289.948) | 285552 → 156656 | 6597 → 3651 |
| ChangedCombined | 1902.427 (1888.613–1927.911) | 1734.613 (1726.817–1743.932) | 990338 → 933524 | 23290 → 21774 |
| RealUpdate | 220.814 (213.066–223.194) | 7.059 (5.879–7.381) | 59059 → 5530 | 1582 → 65 |
| ForcedCompose | 1330.743 (1298.582–1340.489) | 1319.097 (1317.074–1331.830) | 746085 → 746605 | 14882 → 14880 |
| IdleView | 0.074 (0.073–0.074) | 0.073 (0.073–0.074) | 64 → 64 | 4 → 4 |
| IdleUnrelatedUpdateView | 2825.889 (2820.206–2826.721) | 2822.140 (2816.661–2823.369) | 1.07368e+06 → 1.07575e+06 | 18282 → 18282 |
| NotificationClean | 227.787 (224.288–228.354) | 4.361 (4.348–4.475) | 104751 → 772 | 1551 → 22 |
| NotificationReal | 807.792 (805.872–820.772) | 400.469 (392.261–407.399) | 408032 → 197040 | 7144 → 3987 |
| HiddenView | 0.074 (0.073–0.074) | 0.073 (0.073–0.074) | 64 → 64 | 4 → 4 |
| LoadingClean | 1539.094 (1532.655–1570.124) | 1305.267 (1298.281–1318.078) | 748261 → 701772 | 15071 → 13351 |
| LoadingReal | 1534.530 (1501.187–1539.015) | 1321.673 (1312.211–1334.474) | 746724 → 702107 | 14986 → 13347 |
| Tabs1Clean | 70.559 (70.328–72.330) | 1.321 (1.310–1.342) | 16373 → 772 | 562 → 22 |
| Tabs12Clean | 860.282 (837.744–863.188) | 1.302 (1.288–1.310) | 300649 → 772 | 5746 → 22 |
### Original durable benchmark reproduction

Original `BenchmarkShellPresentation` retained byte-for-byte; baseline.txt versus original-final.txt below. It has three session tabs even when panes=1. It retains test wrapper bookkeeping; do not compare its absolute clean timing against the corrected fixture. Random pending-spinner text affects changed rendering/allocation noise. Notification is stable because expiry command is not executed.
| Case | baseline µs (range) | final µs (range) | B/op baseline → final | allocs/op baseline → final |
|---|---:|---:|---:|---:|
| panes=1/draft=empty/changed=false | 218.974 (209.411–220.134) | 1.317 (1.298–1.330) | 53910 → 855 | 1539 → 22 |
| panes=1/draft=empty/changed=true | 1466.861 (1422.407–1467.937) | 1215.024 (1213.715–1244.304) | 629326 → 572239 | 16667 → 14708 |
| panes=1/draft=long/changed=false | 212.930 (212.569–223.935) | 1.328 (1.326–1.390) | 53911 → 864 | 1539 → 22 |
| panes=1/draft=long/changed=true | 1967.278 (1946.926–1994.235) | 1753.836 (1753.721–1770.096) | 986265 → 934110 | 23119 → 21502 |
| panes=3/draft=empty/changed=false | 225.589 (214.778–226.060) | 3.749 (3.748–3.957) | 55629 → 2558 | 1576 → 57 |
| panes=3/draft=empty/changed=true | 1440.363 (1438.765–1467.053) | 1211.665 (1211.531–1249.402) | 750002 → 698505 | 14448 → 12932 |
| panes=3/draft=long/changed=false | 217.077 (216.150–217.121) | 3.711 (3.696–3.768) | 55640 → 2551 | 1576 → 57 |
| panes=3/draft=long/changed=true | 1990.513 (1988.062–2016.612) | 1771.169 (1745.830–1775.433) | 1.10286e+06 → 1.04532e+06 | 22190 → 20266 |
| panes=3/draft=long/changed=false/notification | 234.494 (225.220–237.095) | 7.178 (7.111–7.364) | 106537 → 2552 | 1588 → 57 |
| panes=3/draft=long/changed=true/notification | 2743.111 (2738.883–2751.266) | 2525.599 (2507.962–2582.872) | 1.43287e+06 → 1.33466e+06 | 24318 → 22860 |
## Correctness and actual no-change semantics

`baseline-normalized-parity.txt` vs `final-normalized-parity.txt`: all10 hashes match after final inactive-sub gate; full serialized tea.View fields checked (Content,title,cursor,colors,mouse/focus/bracketed paste/alternate screen/progress etc). Nil OnMouse verified; functions cannot be serialized.600ticks/scenario for active,toast,loading at1ns and16.666667ms; idle/hidden fixtures have no pending lease and therefore execute0ticks. This is same-phase full-output equality, not just matching frame counts.

Initial raw hashes differed because spinner.New selects `defaultMessages[rand.IntN(...)]`: captured baseline-frame/gate-frame differ in exactly the pending-response label row (e.g.Charging the crystals vs Thinking). Corrected parity uses identical `/tmp/spinner-fixed.go` overlays in BOTH revisions, replacing only random label selection with Thinking. Production random behavior preserved. Original unnormalized hashes were NOT parity evidence; premature matching claim was corrected.

Active/toast600real ticks:500whole views equal(83.3%),100changed(16.7%). Baseline160root invalidations versus final100: all60false invalidations coincided with Card boundaries, no Chat boundary/tab visual-generation change. Root specialist restricted pane Card dirtiness to visibly rendered animated headers. At1ns600/600views equal,0invalidations. Loading waiting fixture invalidates600/600even when500views equal; loading acquisition is never completed, so this does not quantify normal finite acquisition latency. Idle and hidden fixture end at0active registrations,0pending leases,settled=true: **no owner Update work is scheduled at true idle**. Hidden fixture is a specific switch-away lifecycle, not proof every background workload sleeps.

The600tick replay has no perturbations. Separate durable tests cover glyph cycle equivalence, changing glyph geometry, Unicode close/hit zones, static/offscreen busy ticks, resize/reveal/hide, drag/scroll/settle lifecycle, theme/agent colors, disposal; root tests cover forced-fresh full View equality across notification edits/paste/focus/clear/resize/theme/show/hide and single/split header semantics. Do not describe those as a600tick perturbation replay.

Synthetic IdleUnrelatedUpdateView sends `struct{}{}` to Update each operation. Production non-tick invalidation path is exercised but no actual source/rate of such messages was captured. Its~2.82ms is **not idle wall-clock cost** and no no-op-input performance claim is made. ForcedCompose deliberately bypasses root view cache,~1.29ms, and is not the true clean path.

## Backend separation

Captured real156x48 ANSI production frame from the normalized fixture (`frame-a.ansi`), truecolor,AltScreen,MouseModeAllMotion,fixed title. Bounded renderer fixture manually calls actual BubbleTea v2.0.9 newCursedRenderer/start/render/flush with counting discard writer. Production dependency graph preserved via copied module and temp modfile: Go1.27 rejects overlay replacements under GOMODCACHE. `backend_bench_test.go`, `backend.mod`, copied `bubbletea/`, `backend.txt` preserve reproduction.

| render + flush | median µs (range) | B/op | alloc/op | output bytes/op |
|---|---:|---:|---:|---:|
| identical tea.View |0.066(0.051–0.076)|112|1|0|
| one equal-width content glyph (`word`→`Word`) |483.295(482.419–490.733)|~13,744|1164|40|
| title-only change |474.067(471.147–476.792)|~13,339|1140|12|

Backend clean equality exits before full ANSI parse/draw. Changed View still clears/draws full frame then retained terminal diff; title-only change also rebuilds frame. This cost is separate from app Update/View and cannot be added mechanically to all owner ticks because renderer flush scheduling/coalescing differs. No real tty, ticker wakeups,OS timer costs,provider,PTY transport,terminal paint or end-to-end latency was measured.

## Residual profile and acceptance

`final-clean.cpu/mem` bounded500000iterations profile: final1.29µs clean is ordinary Update/fanout/lease/bookkeeping, not tab layout. CPU cumulative messages.Update24.36%,reasoningblock.Update/time.Now16.67%,Update52.56%; setup still contributes~14% so percentages are approximate. Allocation-space paths include root command batches,splitLayout.Sessions,tickVisiblePanes,timer-message closures,MapCommand and test scheduler.22alloc/772B is not zero. Toast4.36µs also compares occupancy rectangles; avoids rebuilding text/slices, but not free. Stable root cached View remains~0.08µs.

Hardware-specific acceptance suggested for this fixture: clean owner Update+View<5µs and<1KiB/<30alloc across1/3/12tabs; stable-toast<10µs and<1KiB/<30alloc; real-cadence average<350µs plain,<500µs toast; changed120ms frame no material regression; zero stale-output hashes/hit-zone regressions; no active owner lease at settled idle. Achieved here, not portable CI wall-clock thresholds. Structural gate Tick0alloc tests are more robust. At60accepted ticks/s measured app average289.945µs corresponds to~17.4ms CPU work/s versus~38.1ms/s baseline, excluding backend/scheduler. Do not infer end-to-end utilization.

Remaining limits: loading unchanged path~1.305ms; real changed-frame assembly and backend~0.48ms remain. No history scaling/token-arrival throughput captured; tabs scaling only. Future direct textarea content-mutating APIs MUST invalidate editor-owned occupancyValueValid; current Update/direct paths audited/tested, not a promise of automatic invalidation for future APIs.

## Reproducible commands

Run from `/Users/krissetto/dev/ai-dev/docker-agent`; artifacts root `D=/tmp/docker-agent-residual-fresh`.

```
go test ./pkg/tui -run '^$' -bench '^BenchmarkShellPresentation$' -benchmem -benchtime=500ms -count=3
go test -overlay=$D/pristine-production-overlay.json ./pkg/tui -run '^$' -bench '^BenchmarkShellResidual$' -benchmem -benchtime=500ms -count=3
go test ./pkg/tui -run '^$' -bench '^BenchmarkShellResidual$' -benchmem -benchtime=500ms -count=3
for revision in baseline final; do
 go test -overlay=$D/$revision-parity-overlay.json ./pkg/tui -run '^TestResidualEquality$' -count=1 -v
done
go test ./pkg/tui -run '^$' -bench '^BenchmarkShellResidual$/^CleanCombined$' -benchtime=500000x -cpuprofile=$D/final-clean.cpu -memprofile=$D/final-clean.mem -o $D/final.test
go tool pprof -top -cum $D/final-clean.cpu
go test -mod=readonly -modfile=$D/backend.mod charm.land/bubbletea/v2 -run '^$' -bench '^BenchmarkResidualBackend$' -benchmem -benchtime=400ms -count=3
```

Pristine overlay stores git-show b683f49bd copies of changed production files plus original notification test to avoid compiling final-only private fields against baseline. New common benchmark file is unchanged between both. parity.go and spinner-fixed.go are complete fixture sources. Source refs: tui.go Update/updateWithLifecycle/tick case/View; tabbar.go Tick/unchangedTick/rendered/computeLayoutsForTabs/hasAnimatedIndicator; editor/occupancy.go; notification_avoidance.go; split_panes.go visible pane-header predicate; animation/coordinator.go Accept/tickLocked; BubbleTea cursed_renderer.go290–345.

## Validation and unresolved limitations

- `go test ./pkg/tui/...`: PASS, rerun after final edits.
- `go test ./pkg/tui/components/tabbar`: PASS including0allocation/cycle/offscreen regressions.
- `go vet ./pkg/tui/...`: PASS. gofmt and `git diff --check`: PASS.
- Full selected-package race: component tabbar/editor/notification/animation PASS. Root full race first found existing test direct-read race: Eventually read root.application concurrent with handleSwitchTab. Pristine focused count1+5 did not reproduce timing-sensitive race; source/trace identifies unsynchronized test read, not proven baseline runtime reproduction.
- Authorized minimal test-only repair uses event-loop probe for observation/cleanup; Driver.Send waits sync sentinel after Update+capture, establishing happens-before. Async Eventually check remains, followups use captured app pointer. Repaired test `go test -race ./pkg/tui -run '^TestColdRestoreWorkingDirReplacementRetainsSharedRuntime$' -count=5`: PASS.
- Focused touched root/component race regex `Test(SettledTick|TickGate|OccupiedTextCells|AppendOccupiedTextCells|RootNotificationAvoidance|RunningPaneRequires|PaneActivityCard|Pane|Split|Animation)`: PASS. Components also passed full race.
- **Final full root race NOT GREEN**: no DATA RACE detected after repair, but TestActualProgramTabbarDragDropSettlesAndRejectsHiddenHold observed21.75Hz overlay versus>25Hz threshold, and TestActualProgramQueueRemoveUsesExactCanonicalIDAndEvent/active-hint retained reminder. Both PASS in ONE isolated combined race rerun,6.518s. No thresholds or unrelated production semantics changed. Full-suite interference/instrumentation sensitivity is plausible, not established; preserve these failures as limitations (`race-final.txt`).
- Initial cold race compilation timed out240s; subsequent full run completed. This timeout is separate from test failures. No claim of entire repository tests/races or all-green full root race.
