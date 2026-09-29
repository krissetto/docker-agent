# Main TUI Bubbles migration evidence

## Scope and baseline

The immutable baseline is `b685f5d3abdf29d23e32be1fe12d049f1ad99de0`,
not the older `301d2e290` retained-rendering experiment. This report concerns
removing Bubbles from the **main TUI**. The board still uses Bubbles; neither
whole-binary dependency removal nor terminal-backend replacement is claimed.
Bubble Tea and Lipgloss are outside this dependency-removal scope.

## Reproducible evidence

Run from the repository root with no concurrent builds, tests, races, or other
CPU-heavy work. Git, Go, and Python3 are required; benchstat is not required.

```sh
# Capture immutable baseline before editing production code.
bash scripts/tui-migration-evidence.sh baseline /tmp/tui-migration-evidence
# After integration, diagnose parity before requesting a timing window.
CANDIDATE=candidate-preflight bash scripts/tui-migration-evidence.sh candidate-validate /tmp/tui-migration-evidence
# If no source changes followed, measure that exact preserved binary later.
CANDIDATE=candidate-preflight bash scripts/tui-migration-evidence.sh candidate-measure /tmp/tui-migration-evidence
# Or build, validate, and measure a final candidate in one exclusive window.
bash scripts/tui-migration-evidence.sh candidate /tmp/tui-migration-evidence
# Or perform both phases in a fresh directory:
bash scripts/tui-migration-evidence.sh all /tmp/tui-migration-fresh
```

The script archives the actual baseline commit, preserves identical fixture
sources and test binaries, and canonicalizes both keys and replacement targets
in Go source overlays (including macOS `/tmp` versus `/private/tmp`). Each build's
spinner constructor receives the fixed **input** label `Thinking`; production
clocks/randomness are unchanged and no output bytes or metadata are masked.
The input substitution is applied surgically to each revision's own spinner
source, never by substituting baseline production logic into the candidate.
The script fails closed if that constructor expression changes.

Baseline is compared with a second independent baseline process before timing.
Raw exact complete-View equality is always checked and reported independently.
A mismatch leaves raw JSONL, hashes, first-different-frame indices, complete
unmasked differing field values, and ANSI line context in `*-mismatches.json`.
It remains **exact parity FAIL**, even if terminal semantics match.

A separate strict semantic comparator (`scripts/tui-migration-evidence.go`) gates
candidate timing: every styled grapheme/occupied wide-cell column, color kind and
value (including default versus RGB/indexed), background, attributes, underline,
hyperlink, row geometry, all other exported View metadata, and ending SGR/link
state must match. No spaces, colors, attributes, or terminal metadata are ignored.
Only audited SGR, OSC8, and newline sequences are supported; arbitrary escapes,
controls, unknown/malformed SGR, and incomplete sequences fail closed. Printable
runs use the existing uniseg grapheme decoder; standalone zero-width graphemes or
formatting inserted inside a grapheme are unsupported and rejected, not dropped.
This is bounded styled-output equivalence, not a general terminal emulator or
proof of backend flush equivalence. Raw bytes are neither rewritten nor replaced.

The helper selftests exercise segmentation equality, combining/emoji-ZWJ
clusters, differing backgrounds/attributes/color kinds, hyperlinks and closure,
trailing resets, wide-cell geometry and continuation attributes, and rejection
of unsupported control sequences. Its source is saved with each candidate build.

Candidate evidence fails rather than overwriting a captured binary.
`CANDIDATE=candidate-<label>` preserves separate attempts after fixes without
recapturing or overwriting baseline.
`candidate-validate` builds and checks parity/fixtures without timing;
`candidate-measure` times that exact saved binary, not later working-tree changes.
Use a new label to capture changed candidate code.

### Authoritative bounded workloads

The unchanged `pkg/tui/shell_residual_bench_test.go` and
`pkg/tui/shell_residual_parity_test.go` are copied once and overlaid identically
into both builds. `paneReplayPage` and `splitRecordingPage` are unwrapped before
measurement, avoiding accumulated event recordings. Fixtures exercise production
root Update/View at 156×48 with three session tabs, 24 mixed history messages per
session, and only the profile session working. Visible panes vary independently
between one and three. Drafts are empty or 2080 runes (2240 UTF-8 bytes); stable
toast is independently absent/present. Warmup is 240 owner ticks plus a 5ms
preparation step. Only genuine pending owner leases execute.

* **Clean active tick:** virtual 1ns, glyph-stable and bounded below 1ms total;
  not an idle CPU or wall-clock measurement.
* **Changed frame:** virtual 120ms with actual output changes checked.
* **Realistic cadence:** virtual 16,666,667ns, mixing changed and equal frames;
  no sleeps and no actual terminal flush.
* **Inputs:** one finite 32-event typing, scroll, or stream replay. Typing
  alternates insertion/backspace from empty and must restore the exact draft;
  scroll alternates PageUp/PageDown; stream sends 32 routed chunks, no provider.
  **ns/op, B/op, and allocs/op are per 32-event replay, not per event.**

No commands execute except the virtual owner tick: no provider, terminal IO,
editor blink callback, or toast expiry callback. Bounded streaming is not an
unbounded-history or token-throughput benchmark.

The matrix has 24 combinations and inputs have six cases. Full-View differential
parity covers those plus two 46-frame transition streams: paste, resize, theme,
blur/focus, draft clear, toast hide/show, and resize restoration, each followed by
four owner ticks. Total: **32 streams, 890 complete tea.Views**. Every exported
View field is serialized, including ANSI Content and cursor/terminal metadata;
callbacks must be nil and unknown/unserializable fields fail closed. Default
tests retain only a digest and previous frame; optional evidence capture writes
bounded JSONL.

### Commands and distributions

```sh
binary -test.run '^TestResidualMatrixFixtures$' -test.count=1 -test.v
binary -test.run '^$' -test.bench '^BenchmarkShellResidual$' -test.benchmem -test.benchtime=200ms -test.count=3
binary -test.run '^$' -test.bench '^BenchmarkShellResidualInputs$' -test.benchmem -test.benchtime=1x -test.count=5
```

Raw samples and `baseline-summary.json` / `candidate-summary.json` preserve
min/median/max and all samples for every metric. `BENCHTIME`, `COUNT`, and
`INPUT_COUNT` can override defaults. `BENCHTIME=1x COUNT=1 INPUT_COUNT=1` is a
smoke check, not acceptance timing. A fresh evidence directory is required.

## Captured baseline

Artifacts: `/private/tmp/docker-agent-migration-b685f5d3a` (also reachable under
`/tmp` on macOS). Apple M3 Max, macOS arm64, Go1.27.1, default GOMAXPROCS16.
Baseline binary was built from the immutable archive before production changes;
timing was exclusive of other agent builds/tests. Full script baseline phase
passed, including two independent exact **32-stream/890-View** captures, all 30
fixture validations, three 200ms samples for each of 40 cadence/legacy cases, and
five one-replay samples for each of six input cases. Shell syntax, all embedded
Python compilation, and `git diff --check` passed.

Selected baseline long-draft/no-toast values below are microseconds per accepted
tick+View (not input-replay units). Full distributions remain in the summary JSON.
These are this migration's measured baseline, not numbers copied from the older
report.

| Panes | Cadence | Median µs | Min–max µs |
|---:|---|---:|---:|
| 1 | clean | 1.396 | 1.395–1.403 |
| 1 | changed | 1284.267 | 1279.049–1300.312 |
| 1 | realistic | 232.037 | 222.703–232.753 |
| 3 | clean | 3.610 | 3.588–3.649 |
| 3 | changed | 1352.246 | 1324.029–1352.524 |
| 3 | realistic | 334.031 | 319.618–343.173 |

## Final source-frozen validation

Final production source: `6f71d908262ab070af5d34c627d4b4f1578d73e0`.
`candidate-release.test` was built at that revision with only evidence/documentation
changes outside production. The captured baseline fixtures are unchanged.

**Strict semantic complete-View parity PASS: 32 streams/890 Views.** All styled
cells, color kinds, attributes, links, wide-cell continuation columns, geometry,
exported View metadata, and ending pen/link state match. All **30 fixture
validations pass**. **Raw exact parity FAIL: all 890 Content strings differ**.
This is a documented ANSI serialization change, not byte-identical output.
Total raw Content bytes: **27,073,643 → 25,694,987 (−5.09%)**; this excludes JSON
overhead and is not terminal write/flush/paint timing.

Final dependency checks were rerun on `6f71d9082` and pass; complete output is
preserved in `release-dependency-audit.txt`:

```sh
go list -deps ./pkg/tui
go list -deps -test ./pkg/tui/...
go list -f '{{if not .Standard}}{{.ImportPath}}{{end}}' -deps ./pkg/tui/text
```

Neither dependency graph contains Bubbles. The pure editable-document core's only
non-standard dependency is uniseg. The remaining seven Bubbles import lines under
`pkg`/`cmd` are exclusively in four board files. Thus **main TUI removal** is the
claim, not removal from the whole application/module. Final timing and normal-suite
results are recorded below.

## Diagnostic attempts (not final timing candidates)

Early preflight intentionally failed strict semantic comparison: 374/890 frames
had placeholder-padding foreground `#808080` instead of baseline `#c0c0c0`.
A first grouping repair changed actual placeholder glyphs to regular foreground;
that was also rejected (374/890 frames). Neither plain-text equality nor invisible
space foreground was treated as acceptance. All original frames/diagnostics are
preserved. Early per-rune output grew from 27,073,643 to 29,988,635 Content bytes;
this is not the final implementation.

`candidate-fix2` passes strict semantic comparison for **32 streams/890 complete
Views**, including all styled cells, geometry, View metadata, and ending pen/link
state. All 30 fixture validations pass. **Raw exact parity remains FAIL: all 890
Content byte strings differ** due to ANSI serialization. Total Content bytes are
27,073,643 baseline versus 24,549,899 candidate (−9.32%). This comparison excludes
JSON encoding overhead and does not measure terminal IO or paint latency. The
fix2 binary recorded `d8ddacd60f3901edd15f13c5a164cbf840fa624d` plus its saved
working-tree patch (subsequently committed as `e89a6bafa`); it is preliminary,
not the final source-frozen timing candidate.

## Targeted race validation

The bounded real-program race selection passed in 18.172s with exit 0; command,
revision and working-tree patch are preserved as `race-root-*` artifacts. This
run included the following selection plus the historical
`TabbarDragDropSettlesAndRejectsHiddenHold` and
`QueueRemoveUsesExactCanonicalIDAndEvent` cases:

```sh
go test -race ./pkg/tui -run '^TestActualProgram(EditorDirectResizeAndPostSendCollapse|DialogFadeInputAndIdleQuiescence|HelpCompletionSnapshotAndSafeReturn|ModalHelpPreservesInputAndSelectedAction|LocalEditsKeepDangerousTabKeysAndPasteLocal|EnhancedNewlineAndConfiguredShiftEnterOwnership|HistoryHelpReturnsToSearchWithoutSubmitting|QueuedEditRetainsIdentityOrderAndFailureDraft|ToolApprovalAndReasonHelpPreserveDecisionCorrelation|ToolReasonCancelReturnsUnansweredThenRejectsOnce)$' -count=1 -timeout=120s
```

This exercises actual Bubble Tea programs and event-loop snapshot channels for
composer resize/collapse, completion/help, inline/title paste isolation,
configured newline ownership, history cancellation, queued-edit failure draft
retention, and correlated tool decisions. It is a targeted race selection, not a
claim that the entire race suite passed.

## Integration checks and limitations

Integration-owner validation at both frozen `5755fd6db` and maintained
`6f71d9082` passed fresh
`go test -count=1 ./pkg/tui/...`, board and `cmd/root` tests, relevant `go vet`,
`go build ./cmd/... ./pkg/...`, and
`go build -o /tmp/docker-agent-consumer-validation .`.
Core and adapter-focused race tests passed. The 12 selected actual-program root
races were rerun at maintained `6f71d9082` after the final paint, Word-boundary,
and getter repairs: **PASS 17.967s, exit 0**, preserved as
`release-race-root-{command,revision,log,exit}`. The earlier 18.172s run remains
separately recorded. These are bounded race passes, not a
claim that the full repository race suite ran or passed.

**Unrestricted `go build ./...` is blocked**, not green: preserved ignored file
`dist/kit/v2.cVNprC/evidence/validate-spec.go:2:35` imports missing module
`github.com/docker/sandboxes/sandboxlib/kit`. That untracked artifact is ignored by
`.gitignore:2` (`dist`); its reported creation/modification timestamp
2026-09-22 14:57:29 predates this task. It was left untouched, as were existing
`.worktrees/` and `idk.debug.*`. Explicit production builds above passed without
removing user artifacts.

The owned core still performs whole replacement-string/split/reindex/reuse-map
work on edits. Unchanged-line segmentation/wrapping is cached, but Layout visits
all visual rows and returns detached runs; normalization/scrolling scans row
metadata. Core-only short samples have no equivalent Bubbles baseline and do not
establish a comparative speedup. Root page measurements below are the comparative
evidence. No order-of-magnitude, terminal flush latency, live provider throughput,
wall-clock CPU, or blanket framework-independence claim is made.

## Intermediate comparison at `5755fd6db` (superseded)

The following complete table preserves the initially frozen candidate, including
its reproduced regression. It is **not** the maintained final candidate. The
release comparison at `6f71d9082` follows separately below.

Exclusive timing, baseline and candidate separately, three 200ms samples for
cadence/legacy cases and five finite one-replay input samples. Values below are
**microseconds**; input rows are **per entire 32-event replay**, never per event.
Ranges are min–max; percentages describe medians, not significance estimates.
All samples and metrics remain in `baseline-summary.json`,
`candidate-final-summary.json`, and `candidate-final-comparison.json`.

Long no-toast realistic cadence improves 16.4%/10.9% for one/three panes; actual
changed frames improve 4.4%/10.7%. Finite typing improves 12.7%/11.5%, scroll
52.3%/50.3%, and stream 65.8%/64.3%. Five finite replays are small samples, not
provider throughput or unbounded-history evidence. Clean no-toast remains
microsecond-scale with unchanged allocations. **Long-toast clean regresses**
and is discussed below; it is not omitted from the acceptance evidence.


### Cadence matrix

| Case | Baseline µs median (range) | Candidate µs median (range) | Time Δ | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| Matrix/panes=1/draft=empty/clean/toast=false | 1.310 (1.288–1.361) | 1.306 (1.292–1.313) | -0.3% | 772 → 772 | 22 → 22 |
| Matrix/panes=1/draft=empty/clean/toast=true | 3.788 (3.702–3.794) | 3.807 (3.792–3.832) | +0.5% | 772 → 772 | 22 → 22 |
| Matrix/panes=1/draft=empty/changed/toast=false | 1156.626 (1138.364–1157.771) | 1165.832 (1120.557–1167.143) | +0.8% | 553244 → 546667 | 13780 → 13530 |
| Matrix/panes=1/draft=empty/changed/toast=true | 1696.767 (1677.980–1736.307) | 1663.388 (1656.795–1686.835) | -2.0% | 732379 → 746435 | 15282 → 14995 |
| Matrix/panes=1/draft=empty/real/toast=false | 205.939 (205.295–206.039) | 192.578 (191.909–200.878) | -6.5% | 93239 → 92173 | 2321 → 2280 |
| Matrix/panes=1/draft=empty/real/toast=true | 304.336 (303.940–311.693) | 288.039 (283.599–288.967) | -5.4% | 123288 → 126060 | 2575 → 2529 |
| Matrix/panes=1/draft=long/clean/toast=false | 1.396 (1.395–1.403) | 1.328 (1.321–1.346) | -4.9% | 772 → 772 | 22 → 22 |
| Matrix/panes=1/draft=long/clean/toast=true | 4.565 (4.482–4.580) | 5.450 (5.408–5.455) | +19.4% | 772 → 772 | 22 → 22 |
| Matrix/panes=1/draft=long/changed/toast=false | 1284.267 (1279.049–1300.312) | 1227.395 (1188.632–1245.284) | -4.4% | 761201 → 703733 | 16742 → 14880 |
| Matrix/panes=1/draft=long/changed/toast=true | 2075.885 (1933.601–2108.606) | 1795.508 (1771.587–1813.450) | -13.5% | 1030263 → 909573 | 18596 → 16629 |
| Matrix/panes=1/draft=long/real/toast=false | 232.037 (222.703–232.753) | 193.904 (193.338–196.966) | -16.4% | 128172 → 118063 | 2819 → 2502 |
| Matrix/panes=1/draft=long/real/toast=true | 349.651 (344.413–351.330) | 302.702 (297.924–312.955) | -13.4% | 173483 → 152857 | 3136 → 2800 |
| Matrix/panes=3/draft=empty/clean/toast=false | 3.757 (3.702–4.112) | 3.563 (3.550–3.619) | -5.2% | 2308 → 2308 | 58 → 58 |
| Matrix/panes=3/draft=empty/clean/toast=true | 6.250 (6.187–6.522) | 6.470 (6.244–6.516) | +3.5% | 2308 → 2308 | 58 → 58 |
| Matrix/panes=3/draft=empty/changed/toast=false | 1199.332 (1197.642–1235.043) | 1186.227 (1185.586–1195.688) | -1.1% | 682820 → 677464 | 12141 → 11891 |
| Matrix/panes=3/draft=empty/changed/toast=true | 1879.086 (1858.693–1891.263) | 1871.798 (1857.679–1929.481) | -0.4% | 932200 → 922387 | 14110 → 13822 |
| Matrix/panes=3/draft=empty/real/toast=false | 275.636 (275.494–290.926) | 280.973 (277.382–283.086) | +1.9% | 161325 → 159997 | 2566 → 2496 |
| Matrix/panes=3/draft=empty/real/toast=true | 464.650 (461.944–484.349) | 466.707 (464.329–466.802) | +0.4% | 227263 → 224321 | 3088 → 3002 |
| Matrix/panes=3/draft=long/clean/toast=false | 3.610 (3.588–3.649) | 3.626 (3.572–3.662) | +0.4% | 2308 → 2308 | 58 → 58 |
| Matrix/panes=3/draft=long/clean/toast=true | 7.007 (6.892–7.047) | 7.633 (7.543–7.655) | +8.9% | 2308 → 2308 | 58 → 58 |
| Matrix/panes=3/draft=long/changed/toast=false | 1352.246 (1324.029–1352.524) | 1207.066 (1190.261–1207.692) | -10.7% | 877069 → 861539 | 15643 → 13783 |
| Matrix/panes=3/draft=long/changed/toast=true | 2154.815 (2085.235–2170.622) | 1938.374 (1917.912–1944.441) | -10.0% | 1156124 → 1080693 | 17813 → 15848 |
| Matrix/panes=3/draft=long/real/toast=false | 334.031 (319.618–343.173) | 297.636 (296.345–304.417) | -10.9% | 216436 → 211677 | 3732 → 3240 |
| Matrix/panes=3/draft=long/real/toast=true | 538.086 (530.969–539.047) | 506.422 (505.971–507.065) | -5.9% | 289719 → 270907 | 4307 → 3797 |

### Finite input replays (32 events/op)

| Case | Baseline µs median (range) | Candidate µs median (range) | Time Δ | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| panes=1/typing | 25075.458 (24252.667–25827.417) | 21878.916 (21318.125–22802.750) | -12.7% | 14706960 → 9962760 | 179720 → 141593 |
| panes=1/scroll | 79121.584 (78336.375–81553.958) | 37756.500 (37522.875–37869.291) | -52.3% | 30073992 → 21981432 | 624125 → 481561 |
| panes=1/stream | 121216.250 (120576.916–121674.791) | 41402.291 (41162.876–41763.833) | -65.8% | 39373504 → 24967912 | 808617 → 582854 |
| panes=3/typing | 30644.166 (30236.667–32144.541) | 27128.584 (26977.458–28485.041) | -11.5% | 20020552 → 15276240 | 203997 → 165865 |
| panes=3/scroll | 79952.875 (78813.750–84314.501) | 39742.916 (39485.084–40343.750) | -50.3% | 32639384 → 25967392 | 588406 → 445877 |
| panes=3/stream | 123442.458 (123110.208–125364.084) | 44069.624 (43822.583–44090.167) | -64.3% | 41732568 → 27933536 | 781721 → 555993 |

### Legacy diagnostic cases

| Case | Baseline µs median (range) | Candidate µs median (range) | Time Δ | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| CleanCombined | 1.327 (1.320–1.352) | 1.343 (1.332–1.370) | +1.2% | 772 → 772 | 22 → 22 |
| CleanUpdate | 1.234 (1.229–1.245) | 1.255 (1.251–1.288) | +1.7% | 708 → 708 | 18 → 18 |
| CachedView | 0.076 (0.074–0.077) | 0.075 (0.075–0.076) | -1.2% | 64 → 64 | 4 → 4 |
| RealCombined | 217.901 (217.347–218.776) | 198.040 (194.955–200.631) | -9.1% | 127670 → 118046 | 2808 → 2498 |
| ChangedCombined | 1300.945 (1279.404–1365.288) | 1166.924 (1163.730–1169.487) | -10.3% | 760733 → 701477 | 16742 → 14880 |
| RealUpdate | 5.530 (5.507–5.694) | 5.172 (5.133–5.318) | -6.5% | 5080 → 5093 | 47 → 47 |
| ForcedCompose | 899.979 (876.408–903.761) | 795.384 (793.629–811.939) | -11.6% | 576384 → 518011 | 10245 → 8383 |
| IdleView | 0.074 (0.073–0.074) | 0.074 (0.073–0.074) | +0.2% | 64 → 64 | 4 → 4 |
| IdleUnrelatedUpdateView | 2877.190 (2864.060–2895.602) | 978.311 (973.984–979.919) | -66.0% | 1077161 → 697222 | 18293 → 8798 |
| NotificationClean | 4.342 (4.341–4.402) | 5.284 (5.275–5.435) | +21.7% | 772 → 772 | 22 → 22 |
| NotificationReal | 322.065 (318.850–327.245) | 313.441 (312.681–313.614) | -2.7% | 172483 → 152953 | 3126 → 2790 |
| HiddenView | 0.077 (0.076–0.077) | 0.075 (0.075–0.076) | -2.0% | 64 → 64 | 4 → 4 |
| LoadingClean | 893.153 (886.701–907.770) | 784.526 (778.616–786.090) | -12.2% | 531628 → 425679 | 8606 → 6554 |
| LoadingReal | 876.555 (867.693–890.924) | 779.186 (764.568–784.934) | -11.1% | 531455 → 427276 | 8621 → 6569 |
| Tabs1Clean | 1.364 (1.358–1.398) | 1.297 (1.291–1.309) | -4.9% | 772 → 772 | 22 → 22 |
| Tabs12Clean | 1.324 (1.315–1.394) | 1.326 (1.294–1.353) | +0.2% | 772 → 772 | 22 → 22 |

### Reproduced clean-toast regression

Five alternating baseline/candidate 200ms repeats confirmed the clean-toast
regression; all samples remain **772 B/op / 22 allocs/op**. NotificationClean
median 4.397µs (4.286–4.527) → 5.273µs (5.137–5.585), **+19.9%**. Equivalent
one-pane long-toast matrix 4.397µs (4.244–4.473) → 5.510µs (5.280–5.892),
**+25.3%**. Samples and exact ordering are preserved as
`repeat-{baseline,candidate-final}-{notification,matrix}-{1..5}.txt` and
`repeat-clean-toast-summary.json`. The original matrix/legacy numbers above
remain intact rather than being replaced by the repeats.

A focused 500ms NotificationClean CPU profile for each preserved binary was
collected after timing. Candidate samples include notification avoidance, editor
occupancy access, and Lipgloss frame geometry; this coarse profile is not proof
of one root cause. Profiled ns/op are not substituted into timing results.
No production tuning was performed by the evidence parcel. The integration
owner subsequently traced occupancy accessors to repeated approximately 14KB
model copies: the ARM64 `occupiedTextCells` stack frame was 28,448 bytes.
Read-only pointer receivers remove those copies without changing geometry,
revisions, or output; the repaired frame is 648 bytes. This focused production
repair is included in the maintained release candidate, not the table above.
The late word-completion boundary repair is also included in the final source.


Repeat reproduction (exclusive CPU window; order is baseline then candidate for
each of five rounds, notification then matrix within each revision):

```sh
D=/private/tmp/docker-agent-migration-b685f5d3a
for round in 1 2 3 4 5; do
  for revision in baseline candidate-final; do
    "$D/$revision.test" -test.run '^$' -test.bench '^BenchmarkShellResidual$/^NotificationClean$' -test.benchmem -test.benchtime=200ms -test.count=1
    "$D/$revision.test" -test.run '^$' -test.bench '^BenchmarkShellResidual$/^Matrix$/^panes=1$/^draft=long$/^clean$/^toast=true$' -test.benchmem -test.benchtime=200ms -test.count=1
  done
done
```

## Maintained final comparison at `6f71d9082`

This is the authoritative release table: the same immutable `b685f5d3a` baseline
and captured fixtures against `candidate-release.test`, not the superseded
`5755fd6db` binary above. Full suite: three 200ms samples per cadence/legacy case;
five one-replay samples per input case, all PASS. Runs were serialized with no
concurrent builds/tests/races. Baseline was measured earlier in this session; the
alternating clean-toast repeats below explicitly recalibrate the repaired path.

Long no-toast changed frames improve **5.4%/11.5%**, realistic cadence
**15.9%/11.4%** for one/three panes. Clean is **1.375/3.770µs**, unchanged
772/2308 B and 22/58 allocations. Finite32 typing improves **10.8%/12.2%**, scroll
**51.1%/51.7%**, stream **65.3%/65.2%**. These are small-sample descriptive
medians, not significance estimates or provider throughput. No case regresses
more than 10%; the largest positive time delta is **+4.4%** for three-pane
long-draft/no-toast clean (3.610→3.770µs). **A whole-TUI 10× speedup was not
achieved or established.** Bubble Tea and Lipgloss remain unchanged dependencies.

All timing values are µs; input rows measure the whole32-event replay.
`candidate-release-summary.json` and `candidate-release-comparison.json` preserve
all samples, min/median/max, and allocation metrics.


### Final cadence matrix

| Case | Baseline µs median (range) | Candidate µs median (range) | Time Δ | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| Matrix/panes=1/draft=empty/clean/toast=false | 1.310 (1.288–1.361) | 1.328 (1.325–1.331) | +1.4% | 772 → 772 | 22 → 22 |
| Matrix/panes=1/draft=empty/clean/toast=true | 3.788 (3.702–3.794) | 3.802 (3.690–3.810) | +0.4% | 772 → 772 | 22 → 22 |
| Matrix/panes=1/draft=empty/changed/toast=false | 1156.626 (1138.364–1157.771) | 1132.896 (1116.370–1161.879) | -2.1% | 553244 → 546324 | 13780 → 13530 |
| Matrix/panes=1/draft=empty/changed/toast=true | 1696.767 (1677.980–1736.307) | 1684.744 (1668.340–1723.561) | -0.7% | 732379 → 745069 | 15282 → 14995 |
| Matrix/panes=1/draft=empty/real/toast=false | 205.939 (205.295–206.039) | 196.911 (193.652–202.354) | -4.4% | 93239 → 91987 | 2321 → 2282 |
| Matrix/panes=1/draft=empty/real/toast=true | 304.336 (303.940–311.693) | 289.302 (284.529–293.141) | -4.9% | 123288 → 125063 | 2575 → 2526 |
| Matrix/panes=1/draft=long/clean/toast=false | 1.396 (1.395–1.403) | 1.375 (1.357–1.376) | -1.5% | 772 → 772 | 22 → 22 |
| Matrix/panes=1/draft=long/clean/toast=true | 4.565 (4.482–4.580) | 4.486 (4.402–4.500) | -1.7% | 772 → 772 | 22 → 22 |
| Matrix/panes=1/draft=long/changed/toast=false | 1284.267 (1279.049–1300.312) | 1215.202 (1177.157–1224.527) | -5.4% | 761201 → 704273 | 16742 → 14880 |
| Matrix/panes=1/draft=long/changed/toast=true | 2075.885 (1933.601–2108.606) | 1800.160 (1774.170–1843.656) | -13.3% | 1030263 → 914580 | 18596 → 16630 |
| Matrix/panes=1/draft=long/real/toast=false | 232.037 (222.703–232.753) | 195.161 (193.403–196.028) | -15.9% | 128172 → 118055 | 2819 → 2506 |
| Matrix/panes=1/draft=long/real/toast=true | 349.651 (344.413–351.330) | 300.009 (298.923–302.375) | -14.2% | 173483 → 152215 | 3136 → 2789 |
| Matrix/panes=3/draft=empty/clean/toast=false | 3.757 (3.702–4.112) | 3.593 (3.578–3.610) | -4.4% | 2308 → 2308 | 58 → 58 |
| Matrix/panes=3/draft=empty/clean/toast=true | 6.250 (6.187–6.522) | 6.002 (5.952–6.434) | -4.0% | 2308 → 2308 | 58 → 58 |
| Matrix/panes=3/draft=empty/changed/toast=false | 1199.332 (1197.642–1235.043) | 1148.944 (1137.712–1158.236) | -4.2% | 682820 → 677527 | 12141 → 11891 |
| Matrix/panes=3/draft=empty/changed/toast=true | 1879.086 (1858.693–1891.263) | 1881.020 (1854.740–1883.377) | +0.1% | 932200 → 924701 | 14110 → 13823 |
| Matrix/panes=3/draft=empty/real/toast=false | 275.636 (275.494–290.926) | 278.062 (275.825–278.506) | +0.9% | 161325 → 159148 | 2566 → 2497 |
| Matrix/panes=3/draft=empty/real/toast=true | 464.650 (461.944–484.349) | 466.729 (462.600–468.196) | +0.4% | 227263 → 224187 | 3088 → 3006 |
| Matrix/panes=3/draft=long/clean/toast=false | 3.610 (3.588–3.649) | 3.770 (3.757–3.811) | +4.4% | 2308 → 2308 | 58 → 58 |
| Matrix/panes=3/draft=long/clean/toast=true | 7.007 (6.892–7.047) | 6.943 (6.906–7.027) | -0.9% | 2308 → 2308 | 58 → 58 |
| Matrix/panes=3/draft=long/changed/toast=false | 1352.246 (1324.029–1352.524) | 1196.239 (1193.083–1198.965) | -11.5% | 877069 → 859207 | 15643 → 13782 |
| Matrix/panes=3/draft=long/changed/toast=true | 2154.815 (2085.235–2170.622) | 1932.360 (1931.769–1936.674) | -10.3% | 1156124 → 1081409 | 17813 → 15848 |
| Matrix/panes=3/draft=long/real/toast=false | 334.031 (319.618–343.173) | 296.082 (293.445–309.376) | -11.4% | 216436 → 211104 | 3732 → 3231 |
| Matrix/panes=3/draft=long/real/toast=true | 538.086 (530.969–539.047) | 490.134 (488.907–492.087) | -8.9% | 289719 → 270458 | 4307 → 3770 |

### Final finite input replays (32 events/op)

| Case | Baseline µs median (range) | Candidate µs median (range) | Time Δ | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| panes=1/typing | 25075.458 (24252.667–25827.417) | 22362.375 (22160.708–22745.458) | -10.8% | 14706960 → 10035688 | 179720 → 141585 |
| panes=1/scroll | 79121.584 (78336.375–81553.958) | 38715.291 (38004.500–39050.416) | -51.1% | 30073992 → 21981672 | 624125 → 481563 |
| panes=1/stream | 121216.250 (120576.916–121674.791) | 42068.333 (41570.208–43508.417) | -65.3% | 39373504 → 25109192 | 808617 → 582871 |
| panes=3/typing | 30644.166 (30236.667–32144.541) | 26918.958 (26806.082–27927.417) | -12.2% | 20020552 → 15346624 | 203997 → 165855 |
| panes=3/scroll | 79952.875 (78813.750–84314.501) | 38619.791 (38404.668–39363.792) | -51.7% | 32639384 → 26042232 | 588406 → 445882 |
| panes=3/stream | 123442.458 (123110.208–125364.084) | 42930.500 (42546.208–43156.875) | -65.2% | 41732568 → 28003744 | 781721 → 555997 |

### Final legacy diagnostic cases

| Case | Baseline µs median (range) | Candidate µs median (range) | Time Δ | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| CleanCombined | 1.327 (1.320–1.352) | 1.317 (1.308–1.322) | -0.8% | 772 → 772 | 22 → 22 |
| CleanUpdate | 1.234 (1.229–1.245) | 1.260 (1.250–1.272) | +2.1% | 708 → 708 | 18 → 18 |
| CachedView | 0.076 (0.074–0.077) | 0.076 (0.075–0.080) | +0.1% | 64 → 64 | 4 → 4 |
| RealCombined | 217.901 (217.347–218.776) | 200.638 (199.809–201.475) | -7.9% | 127670 → 117895 | 2808 → 2502 |
| ChangedCombined | 1300.945 (1279.404–1365.288) | 1194.071 (1184.933–1220.039) | -8.2% | 760733 → 705186 | 16742 → 14880 |
| RealUpdate | 5.530 (5.507–5.694) | 5.583 (5.556–5.699) | +1.0% | 5080 → 5122 | 47 → 47 |
| ForcedCompose | 899.979 (876.408–903.761) | 803.946 (799.018–821.885) | -10.7% | 576384 → 517557 | 10245 → 8383 |
| IdleView | 0.074 (0.073–0.074) | 0.074 (0.073–0.074) | +0.3% | 64 → 64 | 4 → 4 |
| IdleUnrelatedUpdateView | 2877.190 (2864.060–2895.602) | 961.966 (954.195–1001.263) | -66.6% | 1077161 → 697850 | 18293 → 8798 |
| NotificationClean | 4.342 (4.341–4.402) | 4.337 (4.333–4.363) | -0.1% | 772 → 772 | 22 → 22 |
| NotificationReal | 322.065 (318.850–327.245) | 301.751 (301.714–303.629) | -6.3% | 172483 → 152906 | 3126 → 2800 |
| HiddenView | 0.077 (0.076–0.077) | 0.075 (0.074–0.076) | -2.6% | 64 → 64 | 4 → 4 |
| LoadingClean | 893.153 (886.701–907.770) | 775.030 (774.947–777.619) | -13.2% | 531628 → 426221 | 8606 → 6554 |
| LoadingReal | 876.555 (867.693–890.924) | 783.892 (777.506–785.367) | -10.6% | 531455 → 427045 | 8621 → 6569 |
| Tabs1Clean | 1.364 (1.358–1.398) | 1.357 (1.354–1.369) | -0.5% | 772 → 772 | 22 → 22 |
| Tabs12Clean | 1.324 (1.315–1.394) | 1.361 (1.351–1.362) | +2.8% | 772 → 772 | 22 → 22 |

### Final alternating clean-toast repeat

Five alternating baseline/release 200ms samples confirm the earlier roughly20%
regression is resolved, not hidden by an earlier baseline measurement.
NotificationClean **4.465µs (4.291–4.556) → 4.488µs (4.421–4.586), +0.52%**;
one-pane long-toast matrix **4.512µs (4.222–4.616) → 4.386µs (4.250–4.656),
−2.79%**. Every sample remains **772 B/op /22 allocs/op**. Original full-suite
numbers remain above, not replaced by repeat results.

Raw files: `release-repeat-{baseline,candidate-release}-{notification,matrix}-{1..5}.txt`
and `release-repeat-clean-toast-summary.json`. Reproduce with the earlier repeat
command using `candidate-release` instead of `candidate-final`. Repaired assembly
and preserved earlier CPU profile establish removal of model-copy work, not a
claim that every notification cost has been eliminated.
