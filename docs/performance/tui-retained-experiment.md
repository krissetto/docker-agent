# Bounded retained editor/pane experiment

## Scope and status

This is the authorized bounded explicit-input editor/pane experiment following
[the historical performance decision](../architecture/tui-performance-decision.md).
It does not authorize a blanket toolkit migration or terminal-backend replacement.
Baseline is `301d2e290897657f67e4cb230064f75aaf49e09a` (`subagents-reborn`).
**Final maintained candidate measurements and 32-stream cross-revision parity
passed.** Integration validation results and limits are recorded below.

## Implemented extraction boundary

`pkg/tui/rendering/retained.Slot[I comparable]` is the only framework-independent
core: a single retained exact input/output value, unconditional fresh rendering,
and draw counts. It imports **nothing**. It is neither a UI state store nor a
scheduler, dependency graph, editable-document engine, or terminal renderer.
Each slot assumes a stable drawing function and single-owner mutation.

Editor `internal/widget` adapters privately own Bubbles textarea/textinput models;
no mutable model/cursor escapes. Mutation invalidates their retained ANSI artifact.
Viewport normalization still belongs to the widget owner because Bubbles View
mutates viewport state. The explicit editor composition input is two strings
(textarea and optional history-search artifacts). Its replaceable paint adapter
still uses Lipgloss for ANSI-cell-aware vertical joining. Retention does not make
that adapter framework-free or make the ANSI input an editable text model.

The complete authoritative editor Frame style is applied on **every editor View**,
including transforms, borders, and future style properties. No partial style
snapshot is reconstructed. `Body` diagnostics therefore increase on these paints;
`Composition`, `Textarea`, `Search`, and `Banner` can remain unchanged on a spinner
update. Banner moves geometry/hit-region work from baseline View into owner
mutation/reflow, then publishes a cached immutable artifact. This is a new owner
materialization boundary plus diagnostics/fresh access, not a replacement banner
paint engine.

Pane identity and title caption are materialized using the full existing identity
and Muted styles on each capture. Exact immutable ANSI identity/activity/title
strings plus width drive retained layout. Exact layout bytes, foreground/background
colors (preserving default/basic/indexed/RGB distinctions), dimming, and fade input
drive retained paint. The surface itself has an explicit color-only contract;
this does not approximate the source identity/caption styles. Identity/caption
materialization is **not** skipped. `agentidentity` has no final production diff.
Transcript `p.TranscriptView()` remains consulted before pane preparation retention;
exact authoritative transcript bytes and dimensions drive that preparation. There
is no new transcript validity authority or hidden registration of dependencies.

This makes the narrow retention utility independently extractable and keeps paint
adapters replaceable through plain values. Extracting a whole editor still requires
the existing widget semantics, styling/ANSI adapter, and owner update/effect model.
Bubble Tea, Lipgloss, and Bubbles remain production dependencies; no backend was
replaced and no whole-TUI framework-independence claim is made.

### Dependency audit

These exact commands passed; `dependency-audit.txt` preserves output:

```sh
go list -deps ./pkg/tui/rendering/retained
go list -f '{{join .Imports "\n"}}' ./pkg/tui/rendering/retained
go list -f '{{.ImportPath}}:{{join .Imports ","}}' ./pkg/tui/components/editor ./pkg/tui/components/editor/internal/widget ./pkg/tui
```

The first lists only `github.com/docker/docker-agent/pkg/tui/rendering/retained`;
the second is empty. The last explicitly confirms Charm imports in widget/editor
and root adapters. This dependency audit is separate from output parity and speed.

## Authoritative fixtures

`pkg/tui/shell_residual_bench_test.go` extends `BenchmarkShellResidual`, not the
older recording-wrapper `BenchmarkShellPresentation`. Both replay wrapper types
are removed before measurement, so per-tick bookkeeping cannot grow without bound.
The new `Matrix/panes=1|3/draft=empty|long/clean|changed|real/toast=false|true`
cases preserve production `Update`/`View`, 156×48 geometry, three session tabs,
and 24 mixed history messages per session. Only the profile session is working;
three panes does not mean three active providers. Long draft is 2080 runes
(2240 UTF-8 bytes). The owner scheduler accepts genuine pending leases; no lease,
registration, or output is fabricated to force progress.

After 240 warm ticks and a 5ms preparation step, clean advances 1ns and checks
the bounded sub-frame budget and unchanged Chat/Card frame indices. Changed
advances 120ms; real advances 16,666,667ns. None sleeps. The fixture validation
checks actual `View.Content`, outside benchmark timers: **clean 0/24 changed,
changed 24/24 changed, real 4/24 (one pane) or 6/24 (three panes)** across both
drafts and toast settings. Each ends with one pending owner lease. These are
output-change observations, not cross-revision full-View parity proofs.

`BenchmarkShellResidualInputs/panes=1|3/typing|scroll|stream` requires
`-benchtime=1x`: each sample is one finite 32-event replay. Its ns/op and
allocations are **per replay**, not per event. Typing alternates insertion and
backspace from an empty draft and verifies exact restoration; scrolling alternates
PageUp/PageDown with content focus; streaming routes 32 real `AgentChoice` chunks
through root Update. All three produce 32/32 changed outputs in baseline validation.
Stream replay is not a throughput or unbounded-history scaling test. It sends no
provider requests. The long draft remains covered by the cadence matrix; it is
not a reversible typing fixture. An initial long wrapped trailing-space typing
probe returned equal byte length but a changed suffix on baseline; that discarded
probe is retained in `fixtures-initial.txt`, not treated as candidate evidence.

Other than the owner tick, commands are not executed: no provider, terminal IO,
editor blink callback, or notification expiry callback. Toast remains stable by
construction. For corrected parity and final timings, fixed spinner text (`Thinking`) is
supplied by the **same test-only source overlay**, with canonicalized source and
replacement paths, in both builds to remove random input-label noise. Production
randomness and clocks are unchanged; output is not normalized.

## Final authoritative same-fixture comparison

Apple M3 Max, macOS arm64, Go1.27.1, default GOMAXPROCS16. CPU-heavy runs were
serialized, including builds and validation outside the timers. Baseline is the
immutable `301d2e290` archive; candidate is the maintained full-style artifact
implementation, not the discarded partial-style snapshot prototype. The common
fixture and test-only spinner overlay are byte-identical with canonical source
and replacement paths. Cadence/legacy cases: three 200ms samples. Inputs: five
one-replay samples. Times below are **microseconds**; input rows measure an entire
32-event replay, not an event. Allocation columns show sample medians; JSON retains
every sample and min/median/max for each metric. Percentages compare medians and
are descriptive, not significance estimates from three samples.

Long-draft no-toast real cadence improves 26.7%/27.7% for one/three panes;
actual changed-frame cadence improves 28.6%/26.0%. Long stable-toast real cadence
improves 19.6%/15.6%. Empty-draft gains are smaller: one-pane real is +0.3%,
three-pane real −8.3%. The experiment mainly avoids rematerializing unchanged
editor widget content when another region changes; it does not accelerate all
inputs or eliminate whole-frame assembly.

Regressions are retained, not omitted: legacy NotificationClean +10.1%
(4.244→4.671µs); three-pane long-toast matrix clean +3.7% (6.949→7.204µs).
The equivalent one-pane long-toast matrix clean is 4.233→4.235µs (+0.0% rounded).
An alternating five-sample 200ms repeat measured legacy 4.359 (4.215–4.488)
→4.380 (4.199–4.532)µs (+0.48%) and equivalent matrix 4.395 (4.312–4.599)
→4.333 (4.213–4.521)µs (−1.41%), all 772B/22 allocations. The original +10.1%
is preserved in the table; the paired repeat did not reproduce that magnitude.
This does not establish its cause or justify a new optimization. Typing replay medians are
+1.9%/+0.1% and allocations increase 179626→179706 / 200844→203986 per replay.
There is **no typing performance improvement claim**. Clean allocations remain
772B/22 for one pane and 2308B/58 for three panes. Synthetic unrelated Update+View
is +1.8%; it is not a measured idle message rate or idle wall-clock CPU cost.

| Case | Baseline µs median (range) | Candidate µs median (range) | time Δ | B/op | alloc/op |
|---|---:|---:|---:|---:|---:|
| Matrix/panes=1/draft=empty/clean/toast=false | 1.340 (1.328–1.342) | 1.359 (1.301–1.374) | +1.4% | 772 → 772 | 22 → 22 |
| Matrix/panes=1/draft=empty/clean/toast=true | 3.713 (3.691–3.715) | 3.785 (3.709–3.788) | +1.9% | 772 → 772 | 22 → 22 |
| Matrix/panes=1/draft=empty/changed/toast=false | 1230.036 (1208.042–1244.150) | 1182.127 (1176.206–1183.395) | -3.9% | 569307 → 555169 | 14570 → 13780 |
| Matrix/panes=1/draft=empty/changed/toast=true | 1745.894 (1741.459–1764.701) | 1697.895 (1650.680–1737.936) | -2.7% | 748702 → 734744 | 16072 → 15282 |
| Matrix/panes=1/draft=empty/real/toast=false | 197.831 (197.651–207.004) | 198.382 (197.127–205.739) | +0.3% | 95631 → 93204 | 2456 → 2322 |
| Matrix/panes=1/draft=empty/real/toast=true | 297.304 (290.740–300.855) | 285.960 (277.777–291.825) | -3.8% | 125560 → 123551 | 2697 → 2574 |
| Matrix/panes=1/draft=long/clean/toast=false | 1.298 (1.289–1.301) | 1.289 (1.284–1.295) | -0.7% | 772 → 772 | 22 → 22 |
| Matrix/panes=1/draft=long/clean/toast=true | 4.233 (4.228–4.243) | 4.235 (4.219–4.448) | +0.0% | 772 → 772 | 22 → 22 |
| Matrix/panes=1/draft=long/changed/toast=false | 1733.173 (1707.006–1772.431) | 1237.532 (1221.436–1241.316) | -28.6% | 932059 → 761150 | 21364 → 16742 |
| Matrix/panes=1/draft=long/changed/toast=true | 2377.436 (2375.960–2406.217) | 1878.995 (1849.508–1883.088) | -21.0% | 1.20006e+06 → 1.02962e+06 | 23219 → 18596 |
| Matrix/panes=1/draft=long/real/toast=false | 289.738 (289.219–298.527) | 212.386 (211.553–216.995) | -26.7% | 156840 → 128331 | 3596 → 2819 |
| Matrix/panes=1/draft=long/real/toast=true | 410.105 (409.886–417.306) | 329.574 (324.029–331.536) | -19.6% | 201660 → 173312 | 3909 → 3135 |
| Matrix/panes=3/draft=empty/clean/toast=false | 3.826 (3.775–3.943) | 3.741 (3.716–3.785) | -2.2% | 2308 → 2308 | 58 → 58 |
| Matrix/panes=3/draft=empty/clean/toast=true | 6.029 (5.972–6.276) | 6.248 (6.084–6.253) | +3.6% | 2308 → 2308 | 58 → 58 |
| Matrix/panes=3/draft=empty/changed/toast=false | 1270.203 (1230.460–1304.781) | 1191.655 (1184.142–1195.747) | -6.2% | 695902 → 684154 | 12863 → 12141 |
| Matrix/panes=3/draft=empty/changed/toast=true | 2021.780 (2007.149–2065.605) | 1851.884 (1843.186–1876.663) | -8.4% | 943480 → 933100 | 14832 → 14110 |
| Matrix/panes=3/draft=empty/real/toast=false | 294.978 (290.180–301.057) | 270.628 (269.582–273.020) | -8.3% | 164559 → 160390 | 2754 → 2560 |
| Matrix/panes=3/draft=empty/real/toast=true | 495.327 (487.992–502.484) | 457.420 (456.890–467.731) | -7.7% | 231816 → 226826 | 3280 → 3081 |
| Matrix/panes=3/draft=long/clean/toast=false | 3.718 (3.679–3.797) | 3.541 (3.520–3.658) | -4.8% | 2308 → 2308 | 58 → 58 |
| Matrix/panes=3/draft=long/clean/toast=true | 6.949 (6.933–6.976) | 7.204 (6.837–7.257) | +3.7% | 2308 → 2308 | 58 → 58 |
| Matrix/panes=3/draft=long/changed/toast=false | 1753.291 (1736.100–1760.349) | 1296.749 (1286.137–1298.742) | -26.0% | 1.04326e+06 → 877582 | 20198 → 15643 |
| Matrix/panes=3/draft=long/changed/toast=true | 2470.553 (2462.096–2483.476) | 2045.277 (2042.343–2063.575) | -17.2% | 1.32097e+06 → 1.15823e+06 | 22368 → 17814 |
| Matrix/panes=3/draft=long/real/toast=false | 440.876 (429.780–454.323) | 318.858 (318.335–318.970) | -27.7% | 261782 → 216169 | 4942 → 3739 |
| Matrix/panes=3/draft=long/real/toast=true | 634.502 (633.655–659.603) | 535.331 (524.182–541.090) | -15.6% | 334063 → 289461 | 5515 → 4306 |
| CleanCombined | 1.299 (1.298–1.302) | 1.342 (1.301–1.348) | +3.3% | 772 → 772 | 22 → 22 |
| CleanUpdate | 1.246 (1.245–1.266) | 1.268 (1.248–1.270) | +1.8% | 708 → 708 | 18 → 18 |
| CachedView | 0.074 (0.074–0.075) | 0.074 (0.073–0.074) | -0.6% | 64 → 64 | 4 → 4 |
| RealCombined | 290.899 (288.694–296.397) | 206.269 (205.263–207.377) | -29.1% | 156327 → 127787 | 3587 → 2816 |
| ChangedCombined | 1719.238 (1715.371–1731.540) | 1229.572 (1227.722–1238.852) | -28.5% | 932889 → 762570 | 21364 → 16742 |
| RealUpdate | 5.471 (5.447–5.544) | 5.139 (5.137–5.428) | -6.1% | 5085 → 5093 | 47 → 47 |
| ForcedCompose | 1315.174 (1307.586–1317.648) | 887.008 (881.559–903.843) | -32.6% | 747599 → 577846 | 14868 → 10245 |
| IdleView | 0.074 (0.073–0.074) | 0.076 (0.075–0.076) | +3.1% | 64 → 64 | 4 → 4 |
| IdleUnrelatedUpdateView | 2819.417 (2815.245–2900.029) | 2870.414 (2857.156–2988.867) | +1.8% | 1.0791e+06 → 1.07715e+06 | 18292 → 18292 |
| NotificationClean | 4.244 (4.226–4.277) | 4.671 (4.515–4.687) | +10.1% | 772 → 772 | 22 → 22 |
| NotificationReal | 402.579 (398.013–410.008) | 313.711 (313.224–324.790) | -22.1% | 201644 → 172755 | 3907 → 3117 |
| HiddenView | 0.074 (0.074–0.076) | 0.074 (0.073–0.074) | -0.5% | 64 → 64 | 4 → 4 |
| LoadingClean | 1338.692 (1333.668–1391.943) | 834.118 (829.846–838.332) | -37.7% | 699166 → 530474 | 13223 → 8606 |
| LoadingReal | 1342.398 (1310.485–1350.071) | 837.150 (835.585–839.238) | -37.6% | 697968 → 531175 | 13237 → 8621 |
| Tabs1Clean | 1.312 (1.312–1.319) | 1.292 (1.289–1.297) | -1.5% | 772 → 772 | 22 → 22 |
| Tabs12Clean | 1.332 (1.317–1.347) | 1.331 (1.310–1.336) | -0.1% | 772 → 772 | 22 → 22 |
| Inputs/panes=1/typing | 24618.291 (24389.041–25658.334) | 25074.333 (24627.708–25178.541) | +1.9% | 1.45649e+07 → 1.4566e+07 | 179626 → 179706 |
| Inputs/panes=1/scroll | 93500.292 (92770.124–94375.292) | 78654.125 (78057.291–80778.459) | -15.9% | 3.54202e+07 → 3.00035e+07 | 772035 → 624119 |
| Inputs/panes=1/stream | 138256.708 (136576.249–140638.083) | 121013.333 (118700.708–123332.292) | -12.5% | 4.47944e+07 → 3.93741e+07 | 956530 → 808621 |
| Inputs/panes=3/typing | 30900.583 (30889.042–31821.334) | 30923.166 (30730.625–32195.542) | +0.1% | 1.99403e+07 → 2.00156e+07 | 200844 → 203986 |
| Inputs/panes=3/scroll | 95582.667 (94864.250–96600.917) | 78969.126 (77647.125–80161.001) | -17.4% | 3.80537e+07 → 3.2709e+07 | 733251 → 588411 |
| Inputs/panes=3/stream | 137895.792 (137202.625–138182.167) | 119279.333 (118714.500–119623.333) | -13.5% | 4.72936e+07 → 4.18174e+07 | 926589 → 781731 |

## Artifacts and exact reproduction

Artifacts: `/tmp/docker-agent-retained-baseline-301d2e290/` (canonical
`/private/tmp/...` on this machine). Authoritative measurements are
`fixed-baseline{,-inputs}.txt` versus `maintained-candidate{,-inputs}.txt`;
`*-summary.json` preserves samples and `maintained-candidate-comparison.json`
preserves paired metrics. `fixed-baseline.test` and `maintained-candidate.test`
are measured binaries; final source patch/status snapshots are also retained.
`final-baseline-frames-vs-maintained-candidate-frames.json` records final parity.
Local temporary files are evidence, not a requirement to reproduce this work.

### Fresh reproduction without prior temporary files

Run from the repository root, with no concurrent builds, tests, race runs, or
other CPU-heavy activity:

```sh
bash scripts/tui-retained-evidence.sh /tmp/tui-retained-new-evidence
```

The durable script creates a fresh `git archive` of baseline `301d2e290`, copies
the current durable benchmark/parity fixture sources identically, canonicalizes
**both** source and replacement paths, generates a test-only fixed-label overlay,
builds baseline and current-working-tree binaries, verifies all raw full-View
streams before timing, then executes:

```sh
binary -test.run '^TestResidualMatrixFixtures$' -test.count=1 -test.v
binary -test.run '^$' -test.bench '^BenchmarkShellResidual$' -test.benchmem -test.benchtime=200ms -test.count=3
binary -test.run '^$' -test.bench '^BenchmarkShellResidualInputs$' -test.benchmem -test.benchtime=1x -test.count=5
```

It preserves source fixtures, overlays, environment, binaries, raw JSONL views,
raw benchmark samples, full-view hashes, and machine-readable distributions.
The output directory must be new. Python3, Git, and Go are required. The script
fails closed if candidate spinner source differs from baseline, fixture parity
fails, or benchmark validation fails. Production random behavior is untouched.
Optional `BENCHTIME`, `COUNT`, and `INPUT_COUNT` override the defaults above for
smoke validation; `BENCHTIME=1x COUNT=1 INPUT_COUNT=1` runs every case once and is
not a substitute for repeated acceptance timings. Syntax and embedded Python checks passed. The full wrapper also passed an
end-to-end fresh-directory smoke with these 1x overrides, including all fixtures,
all benchmark cases, and exact 32-stream/890-View parity. Smoke timings are not
used in the acceptance table (`durable-script-smoke/`, `durable-script-smoke.log`).

## Validation and limits

Baseline `TestResidualMatrixFixtures`: PASS, all 30 cases; original and expanded
benchmark runs: PASS. No full tests or race suites were run concurrently with
timed measurements. Final maintained candidate fixture checks and all benchmarks: PASS. Differential
full-View parity: PASS (32 streams/890 Views). Core dependency audit: PASS.
Integration validation: `go test ./pkg/tui/...` PASS (root 82.650s);
`go vet ./pkg/tui/...` PASS. Full `go test -race ./pkg/tui/...` attempt was
terminated at 240 seconds with no package output, data-race report, or behavior
assertion; **inconclusive, not green and not established as a regression**.
No lingering processes were observed afterward. The following bounded component
and root integrated races **PASS**, root 37.70s including the historical timing
and queue-removal cases:

```sh
go test -race ./pkg/tui/rendering/retained ./pkg/tui/components/editor/... ./pkg/tui/components/agentidentity -count=1
go test -race ./pkg/tui -run 'TestRetained|TestPane|TestActualRootTransitionLifecycle|TestActualProgram(ContextUsageLifecycleAndIdleCleanup|EditorDirectResizeAndPostSendCollapse|TabbarDragDropSettlesAndRejectsHiddenHold|QueueRemoveUsesExactCanonicalIDAndEvent)' -count=1 -timeout=120s
```

These passes do not turn the timed-out full race run green. Installed
`golangci-lint` 2.12.2 exited 3 because its Go1.26.2 build cannot target Go1.27
(runtime Go1.27.1); lint validation is blocked, not passed. Logs:
`validation-{test,vet,lint,race-all,race-components,race-root,audit}.log` in the
evidence directory. Final `git diff --check` and script syntax checks: PASS.

No PTY/backend render/flush, actual terminal paint, wall-clock utilization,
provider scheduling, token-arrival throughput, or end-to-end latency is measured.
A cached root View, an accepted glyph-stable tick, a changed frame, a synthetic
unrelated update, and settled idle are different paths. Bounded extraction does
not establish pure editable-document rendering or a complete retained dependency
architecture. The bounded experiment is ready to test, not a broad toolkit migration or live
terminal performance guarantee.

## Corrected complete-View cross-revision parity

`pkg/tui/shell_residual_parity_test.go` adds a bounded stream fingerprint harness,
independent of candidate retained-versus-fresh checks. The corrected immutable
baseline and candidate streams match **32/32 sequences, 890 complete tea.Views**.
A second immutable-baseline process also matches all 32 streams byte-for-byte;
its raw frame visibly contains the controlled `Thinking` label. The first
noncanonical-overlay parity attempts were not valid cross-revision evidence:
baseline versus itself differed in random pending-response labels. They are
retained separately, not attributed to a production regression.

The 32 sequences comprise 24 cadence/pane/draft/toast scenarios, six 32-event
input sequences, and two 46-frame transition sequences (paste, resize, theme,
blur/focus, draft clear, notification hide/show, resize restoration; four genuine
owner ticks follow each transition). Each frame serializes all exported View
fields: Content, Cursor, WindowTitle, ForegroundColor, BackgroundColor, AltScreen,
MouseMode, DisableBracketedPasteMode, KeyboardEnhancements, ReportFocus,
ProgressBar, and OnMouse. Callback fields must be nil and are encoded explicitly
as null. Unknown/unserializable fields fail closed. ANSI bytes and metadata are
not normalized. Default tests retain only a digest and the previous frame;
`TUI_RESIDUAL_PARITY_DIR` optionally stores the bounded JSONL frame streams.

Artifacts `baseline-fixed-frames/`, `baseline-fixed-repeat-frames/`,
`candidate-fixed-frames/` (earlier), `final-baseline-frames/`,
`maintained-candidate-frames/` (final), corresponding `*-parity.txt` logs, `compare-parity.py`,
and `baseline-fixed-frames-vs-*.json` hold the raw comparison evidence. This is
bounded same-input output equality, not proof for every event or future API.

```sh
# Baseline cwd: /private/tmp/docker-agent-retained-baseline-301d2e290/source
D=/private/tmp/docker-agent-retained-baseline-301d2e290
TUI_RESIDUAL_PARITY_DIR="$D/baseline-fixed-frames" go test -overlay="$D/baseline-overlay.json" ./pkg/tui -run '^TestResidualViewStreams$' -count=1 -v
# Candidate cwd: /Users/krissetto/dev/ai-dev/docker-agent
TUI_RESIDUAL_PARITY_DIR="$D/candidate-fixed-frames" go test -overlay="$D/candidate-overlay.json" ./pkg/tui -run '^TestResidualViewStreams$' -count=1 -v
python3 "$D/compare-parity.py" baseline-fixed-frames candidate-fixed-frames
```

## Superseded evidence appendix

The original noncanonical `/tmp` overlay keys missed baseline source under
`/private/tmp`. Preliminary `baseline*.txt` timings and first parity captures
therefore retained random spinner labels and are **not** the final controlled
comparison. A baseline-versus-itself repeat exposed this; canonical paths fixed
it, and two corrected baseline processes matched all 32 raw streams. Those raw
artifacts are preserved, not erased. `fixed-candidate*` is the subsequently
superseded partial-style snapshot prototype; it is attribution only. Only
`fixed-baseline` versus `maintained-candidate` supplies the table above.

## Notification repeat reproduction

After all integration tests/races finished, five alternating baseline/candidate
samples repeated these two cases with the preserved final binaries:

```sh
binary -test.run '^$' -test.bench '^BenchmarkShellResidual$/^NotificationClean$' -test.benchmem -test.benchtime=200ms -test.count=1
binary -test.run '^$' -test.bench '^BenchmarkShellResidual$/^Matrix$/^panes=1$/^draft=long$/^clean$/^toast=true$' -test.benchmem -test.benchtime=200ms -test.count=1
```

`repeat-notification.zsh`, `repeat-{fixed-baseline,maintained-candidate}-{legacy,matrix}-{1..5}.txt`,
and `notification-repeat-summary.json` preserve exact ordering, all measurements,
and distributions. No production changes followed this repeat.
