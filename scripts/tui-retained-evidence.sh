#!/usr/bin/env bash
# Rebuild identical bounded residual fixtures against an immutable baseline and
# the current working tree. Run alone: builds/tests/races must not overlap timing.
set -euo pipefail

ROOT=$(git rev-parse --show-toplevel)
BASELINE=${BASELINE:-301d2e290897657f67e4cb230064f75aaf49e09a}
BENCHTIME=${BENCHTIME:-200ms}
COUNT=${COUNT:-3}
INPUT_COUNT=${INPUT_COUNT:-5}
ARTIFACTS=${1:-$(mktemp -d "${TMPDIR:-/tmp}/tui-retained.XXXXXX")}
mkdir -p "$ARTIFACTS"
ARTIFACTS=$(python3 -c 'import pathlib,sys; print(pathlib.Path(sys.argv[1]).resolve())' "$ARTIFACTS")
if [[ -e "$ARTIFACTS/source" ]]; then
  echo "Refusing to overwrite existing evidence: $ARTIFACTS/source" >&2
  exit 1
fi
mkdir "$ARTIFACTS/source"
git -C "$ROOT" archive "$BASELINE" | tar -x -C "$ARTIFACTS/source"
for file in shell_residual_bench_test.go shell_residual_parity_test.go; do
  cp "$ROOT/pkg/tui/$file" "$ARTIFACTS/$file"
  cp "$ARTIFACTS/$file" "$ARTIFACTS/source/pkg/tui/$file"
done
python3 - "$ROOT" "$ARTIFACTS" <<'PY'
import json, pathlib, sys
root, artifacts = (pathlib.Path(p).resolve() for p in sys.argv[1:])
spinner = pathlib.Path('pkg/tui/components/spinner/spinner.go')
source = (artifacts / 'source' / spinner).read_text()
needle = 'defaultMessages[rand.IntN(len(defaultMessages))]'
assert source.count(needle) == 1, 'spinner random-input fixture changed; review overlay'
fixed = source.replace('\t"math/rand/v2"\n', '').replace(needle, '"Thinking"')
(artifacts / 'spinner-fixed.go').write_text(fixed)
# Never silently replace changed production spinner logic in the candidate.
assert (root / spinner).read_text() == source, 'candidate spinner differs; review common overlay'
for name, tree in [('baseline', artifacts / 'source'), ('candidate', root)]:
    replacements = {str(tree / spinner): str(artifacts / 'spinner-fixed.go')}
    for filename in ['shell_residual_bench_test.go', 'shell_residual_parity_test.go']:
        replacements[str(tree / 'pkg/tui' / filename)] = str(artifacts / filename)
    (artifacts / (name + '-overlay.json')).write_text(json.dumps({'Replace': replacements}, indent=2))
PY
{
  git -C "$ROOT" rev-parse "$BASELINE"
  git -C "$ROOT" rev-parse HEAD
  go version
  uname -a
  git -C "$ROOT" status --short
} > "$ARTIFACTS/environment.txt"
for revision in baseline candidate; do
  source="$ROOT"
  if [[ "$revision" == baseline ]]; then source="$ARTIFACTS/source"; fi
  (cd "$source" && go test -overlay="$ARTIFACTS/$revision-overlay.json" -c -o "$ARTIFACTS/$revision.test" ./pkg/tui)
  TUI_RESIDUAL_PARITY_DIR="$ARTIFACTS/$revision-frames" "$ARTIFACTS/$revision.test" -test.run '^TestResidualViewStreams$' -test.count=1 -test.v > "$ARTIFACTS/$revision-parity.txt"
done
python3 - "$ARTIFACTS" <<'PY'
import hashlib, json, pathlib, sys
artifacts = pathlib.Path(sys.argv[1])
left = artifacts / 'baseline-frames'
right = artifacts / 'candidate-frames'
assert {p.name for p in left.glob('*.jsonl')} == {p.name for p in right.glob('*.jsonl')}
results = {}
for p in sorted(left.glob('*.jsonl')):
    a, b = p.read_bytes(), (right / p.name).read_bytes()
    results[p.name] = {'equal': a == b, 'frames': len(a.splitlines()), 'baseline_sha256': hashlib.sha256(a).hexdigest(), 'candidate_sha256': hashlib.sha256(b).hexdigest()}
(artifacts / 'parity.json').write_text(json.dumps(results, indent=2) + '\n')
assert results and all(row['equal'] for row in results.values()), 'full-View parity failed; inspect raw JSONL before timing'
print('Exact full-View parity:', len(results), 'streams,', sum(row['frames'] for row in results.values()), 'frames')
PY
for revision in baseline candidate; do
  "$ARTIFACTS/$revision.test" -test.run '^TestResidualMatrixFixtures$' -test.count=1 -test.v > "$ARTIFACTS/$revision-fixtures.txt"
  "$ARTIFACTS/$revision.test" -test.run '^$' -test.bench '^BenchmarkShellResidual$' -test.benchmem -test.benchtime="$BENCHTIME" -test.count="$COUNT" > "$ARTIFACTS/$revision.txt"
  "$ARTIFACTS/$revision.test" -test.run '^$' -test.bench '^BenchmarkShellResidualInputs$' -test.benchmem -test.benchtime=1x -test.count="$INPUT_COUNT" > "$ARTIFACTS/$revision-inputs.txt"
done
python3 - "$ARTIFACTS" <<'PY'
import json, pathlib, re, statistics, sys
artifacts = pathlib.Path(sys.argv[1])
summary = {}
for revision in ['baseline', 'candidate']:
    rows = {}
    for suffix in ['', '-inputs']:
        for line in (artifacts / (revision + suffix + '.txt')).read_text().splitlines():
            if not line.startswith('Benchmark'):
                continue
            fields = line.split()
            name = re.sub(r'-\d+$', '', fields[0])
            rows.setdefault(name, []).append(dict(zip(fields[3::2], map(float, fields[2::2]))))
    summary[revision] = {name: {unit: {'samples': [s[unit] for s in samples], 'median': statistics.median(s[unit] for s in samples), 'min': min(s[unit] for s in samples), 'max': max(s[unit] for s in samples)} for unit in samples[0]} for name, samples in rows.items()}
(artifacts / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
PY
printf 'Evidence: %s\n' "$ARTIFACTS"
