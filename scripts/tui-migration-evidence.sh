#!/usr/bin/env bash
# Run alone: builds, tests, and races must not overlap timing. Baseline can be
# captured before production edits; candidate reuses its immutable fixtures.
set -euo pipefail
ROOT=$(git rev-parse --show-toplevel)
BASELINE=${BASELINE:-b685f5d3abdf29d23e32be1fe12d049f1ad99de0}
BENCHTIME=${BENCHTIME:-200ms}
COUNT=${COUNT:-3}
INPUT_COUNT=${INPUT_COUNT:-5}
PHASE=${1:-all}
CANDIDATE=${CANDIDATE:-candidate}
[[ "$CANDIDATE" =~ ^candidate(-[a-zA-Z0-9_-]+)?$ ]] || { echo 'CANDIDATE must be candidate or candidate-<label>' >&2; exit 2; }
ARTIFACTS=${2:-$(mktemp -d "${TMPDIR:-/tmp}/tui-migration.XXXXXX")}
case "$PHASE" in baseline|candidate|candidate-validate|candidate-measure|all) ;; *) echo "Usage: $0 [baseline|candidate|candidate-validate|candidate-measure|all] [artifacts]" >&2; exit 2 ;; esac
mkdir -p "$ARTIFACTS"
ROOT=$(python3 -c 'import pathlib,sys; print(pathlib.Path(sys.argv[1]).resolve())' "$ROOT")
ARTIFACTS=$(python3 -c 'import pathlib,sys; print(pathlib.Path(sys.argv[1]).resolve())' "$ARTIFACTS")

prepare_baseline() {
  if [[ -e "$ARTIFACTS/source" ]]; then
    echo "Refusing to overwrite baseline: $ARTIFACTS/source" >&2
    exit 1
  fi
  mkdir "$ARTIFACTS/source"
  git -C "$ROOT" archive "$BASELINE" | tar -x -C "$ARTIFACTS/source"
  for file in shell_residual_bench_test.go shell_residual_parity_test.go; do
    cp "$ROOT/pkg/tui/$file" "$ARTIFACTS/$file"
  done
  cp "$ROOT/scripts/tui-migration-evidence.go" "$ARTIFACTS/tui-migration-evidence.go"
  git -C "$ROOT" rev-parse "$BASELINE" > "$ARTIFACTS/baseline-revision.txt"
}

build() {
  local revision=$1 source=$2
  [[ ! -e "$ARTIFACTS/$revision.test" ]] || { echo "Refusing to overwrite $revision binary" >&2; exit 1; }
  cp "$ROOT/scripts/tui-migration-evidence.go" "$ARTIFACTS/$revision-semantic.go"
  python3 - "$source" "$ARTIFACTS" "$revision" <<'PY'
import json, pathlib, sys
source, artifacts = (pathlib.Path(p).resolve() for p in sys.argv[1:3])
revision = sys.argv[3]
spinner = source / 'pkg/tui/components/spinner/spinner.go'
text = spinner.read_text()
needle = 'defaultMessages[rand.IntN(len(defaultMessages))]'
assert text.count(needle) == 1, 'spinner input changed; review overlay, never mask output'
fixed = text.replace('\t"math/rand/v2"\n', '').replace(needle, '"Thinking"')
target = artifacts / (revision + '-spinner-fixed.go')
target.write_text(fixed)
# Resolve BOTH sides: /tmp and /private/tmp differ on macOS.
replacements = {str(spinner.resolve()): str(target.resolve())}
for filename in ['shell_residual_bench_test.go', 'shell_residual_parity_test.go']:
    replacements[str((source / 'pkg/tui' / filename).resolve())] = str((artifacts / filename).resolve())
(artifacts / (revision + '-overlay.json')).write_text(json.dumps({'Replace': replacements}, indent=2) + '\n')
PY
  {
    git -C "$ROOT" rev-parse HEAD
    go version
    uname -a
    printf 'BENCHTIME=%s COUNT=%s INPUT_COUNT=%s\n' "$BENCHTIME" "$COUNT" "$INPUT_COUNT"
    git -C "$ROOT" status --short
  } > "$ARTIFACTS/$revision-environment.txt"
  if [[ "$revision" == candidate* ]]; then
    git -C "$ROOT" diff HEAD > "$ARTIFACTS/$revision.patch"
  fi
  (cd "$source" && go test -overlay="$ARTIFACTS/$revision-overlay.json" -c -o "$ARTIFACTS/$revision.test" ./pkg/tui)
}

capture() {
  local revision=$1 label=$2
  TUI_RESIDUAL_PARITY_DIR="$ARTIFACTS/$label-frames" "$ARTIFACTS/$revision.test" -test.run '^TestResidualViewStreams$' -test.count=1 -test.v > "$ARTIFACTS/$label-parity.txt"
}

compare() {
  python3 - "$ARTIFACTS" "$1" "$2" <<'PY'
import hashlib, json, pathlib, sys
artifacts = pathlib.Path(sys.argv[1])
left, right = (artifacts / (name + '-frames') for name in sys.argv[2:])
assert {p.name for p in left.glob('*.jsonl')} == {p.name for p in right.glob('*.jsonl')}, 'stream names differ'
results = {}
diagnostics = []
for p in sorted(left.glob('*.jsonl')):
    a, b = p.read_bytes(), (right / p.name).read_bytes()
    row = {'equal': a == b, 'frames': len(a.splitlines()), 'baseline_sha256': hashlib.sha256(a).hexdigest(), 'other_sha256': hashlib.sha256(b).hexdigest()}
    if a != b:
        aa, bb = a.splitlines(), b.splitlines()
        index = next((i for i, (x, y) in enumerate(zip(aa, bb)) if x != y), min(len(aa), len(bb)))
        row['first_different_frame'] = index
        detail = {'stream': p.name, 'frame': index, 'baseline_frames': len(aa), 'other_frames': len(bb)}
        if index < min(len(aa), len(bb)):
            av, bv = json.loads(aa[index]), json.loads(bb[index])
            detail['different_fields'] = {key: {'baseline': av.get(key), 'other': bv.get(key)} for key in sorted(av.keys() | bv.keys()) if av.get(key) != bv.get(key) or (key in av) != (key in bv)}
            # Complete raw field values stay above; line context only aids diagnosis.
            if av.get('Content') != bv.get('Content'):
                al, bl = av['Content'].splitlines(), bv['Content'].splitlines()
                line = next((i for i, (x, y) in enumerate(zip(al, bl)) if x != y), min(len(al), len(bl)))
                detail['content_context'] = {'first_line': line, 'baseline': al[max(0, line-1):line+2], 'other': bl[max(0, line-1):line+2]}
        diagnostics.append(detail)
    results[p.name] = row
stem = '-vs-'.join(sys.argv[2:])
(artifacts / (stem + '-parity.json')).write_text(json.dumps(results, indent=2) + '\n')
(artifacts / (stem + '-mismatches.json')).write_text(json.dumps(diagnostics, indent=2, ensure_ascii=False) + '\n')
if diagnostics:
    first = diagnostics[0]
    print('First mismatch:', first['stream'], 'frame', first['frame'], 'fields', list(first.get('different_fields', {})))
    print('Raw field values and ANSI line context:', artifacts / (stem + '-mismatches.json'))
assert results and all(row['equal'] for row in results.values()), 'full-View parity failed; inspect raw JSONL before timing'
print('Exact full-View parity:', len(results), 'streams,', sum(row['frames'] for row in results.values()), 'frames')
PY
}

measure() {
  local revision=$1
  "$ARTIFACTS/$revision.test" -test.run '^TestResidualMatrixFixtures$' -test.count=1 -test.v > "$ARTIFACTS/$revision-fixtures.txt"
  "$ARTIFACTS/$revision.test" -test.run '^$' -test.bench '^BenchmarkShellResidual$' -test.benchmem -test.benchtime="$BENCHTIME" -test.count="$COUNT" > "$ARTIFACTS/$revision.txt"
  # Each operation is a finite 32-event replay, NOT a single event.
  "$ARTIFACTS/$revision.test" -test.run '^$' -test.bench '^BenchmarkShellResidualInputs$' -test.benchmem -test.benchtime=1x -test.count="$INPUT_COUNT" > "$ARTIFACTS/$revision-inputs.txt"
  python3 - "$ARTIFACTS" "$revision" <<'PY'
import json, pathlib, re, statistics, sys
artifacts, revision = pathlib.Path(sys.argv[1]), sys.argv[2]
rows = {}
for suffix in ['', '-inputs']:
    for line in (artifacts / (revision + suffix + '.txt')).read_text().splitlines():
        if not line.startswith('Benchmark'):
            continue
        fields = line.split()
        name = re.sub(r'-\d+$', '', fields[0])
        rows.setdefault(name, []).append(dict(zip(fields[3::2], map(float, fields[2::2]))))
summary = {name: {unit: {'samples': [s[unit] for s in samples], 'median': statistics.median(s[unit] for s in samples), 'min': min(s[unit] for s in samples), 'max': max(s[unit] for s in samples)} for unit in samples[0]} for name, samples in rows.items()}
(artifacts / (revision + '-summary.json')).write_text(json.dumps(summary, indent=2) + '\n')
PY
}

if [[ "$PHASE" == baseline || "$PHASE" == all ]]; then
  prepare_baseline
  build baseline "$ARTIFACTS/source"
  capture baseline baseline
  capture baseline baseline-repeat
  compare baseline baseline-repeat
  measure baseline
fi
if [[ "$PHASE" != baseline ]]; then
  [[ -s "$ARTIFACTS/baseline-summary.json" ]] || { echo 'Capture baseline first' >&2; exit 1; }
  if [[ "$PHASE" != candidate-measure ]]; then
    build "$CANDIDATE" "$ROOT"
    capture "$CANDIDATE" "$CANDIDATE"
  fi
  # Preserve exact raw FAIL truthfully. A separately checked strict semantic
  # comparison permits different ANSI serialization, never changed styled cells.
  if ! compare baseline "$CANDIDATE"; then
    echo 'Raw exact full-View parity: FAIL (retained); checking strict semantics separately'
  fi
  (cd "$ARTIFACTS/source" && go run "$ARTIFACTS/$CANDIDATE-semantic.go" "$ARTIFACTS" "$CANDIDATE")
  if [[ "$PHASE" == candidate-validate ]]; then
    "$ARTIFACTS/$CANDIDATE.test" -test.run '^TestResidualMatrixFixtures$' -test.count=1 -test.v > "$ARTIFACTS/$CANDIDATE-fixtures.txt"
  else
    # Measures the preserved binary, not subsequent working-tree changes.
    [[ ! -e "$ARTIFACTS/$CANDIDATE.txt" ]] || { echo 'Refusing to overwrite candidate timings' >&2; exit 1; }
    measure "$CANDIDATE"
  fi
fi
printf 'Evidence: %s\n' "$ARTIFACTS"
