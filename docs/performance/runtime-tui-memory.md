# Runtime and TUI memory optimization

## Scope

This pass reduces allocation churn in streaming, event delivery and history replay,
and removes a redundant private committed-view snapshot. It does **not** establish
an order-of-magnitude reduction in whole-application RAM. The observed live process
had an approximately 1.9 GiB footprint; it was neither restarted nor used as an
interchangeable before/after benchmark.

Measurements used the dirty-worktree snapshot taken before implementation, not HEAD
alone, on macOS arm64 / Apple M3 Max / Go 1.27.1. Go workloads were serialized, CGO
remained enabled, and fixtures used fake providers and temporary stores. MB below
means 1,000,000 bytes. Retained heap is measured after forced GC in synthetic tests;
allocation volume is not retained heap or RSS.

## Changes

- Streamed assistant text uses an append-only builder in the model stream,
  harness, event hub and persistence presentation. Previously emitted strings
  remain immutable; tool boundaries and reasoning/content order are unchanged.
- Event publication reuses one JSON encoding for byte accounting and independent
  observer decodes. Process-local metadata and snapshot isolation are preserved.
- App fanout avoids per-event subscriber-slice copies, checks removed subscribers
  safely, and clears flushed throttle references. Backpressure is unchanged.
- Compact history steps preserve valid rendered-item caches. Plain history
  prepends use amortized front capacity, released on completion or invalidation.
  Canonical history and final rendered rows are not truncated.
- A prepared view shares one **private** committed Session/Attach snapshot;
  every public Info/Commit result still deep-copies each field independently.
  Terminal preparations release publication-only graphs. Restoring an already
  privately cloned child no longer clones it a second time.
- Typed schema maps and slices are deeply isolated. A broader schema-sharing
  experiment was rejected because it changed independent public-clone mutation
  behavior; it is not part of these changes.

## Same-workload measurements

| Workload | Before | After | Meaning |
|---|---:|---:|---|
| Compact replay, 1,000 fixture records / 1,200 items | 1,728–1,733 MB allocated | 88.1–88.7 MB | About 19.6× fewer allocated bytes |
| Same compact replay | 2.37–2.49 s | 97–104 ms | About 24× faster in these two process runs |
| Same compact replay | 46.35 million allocations | 1.37 million | About 34× fewer allocations |
| Same compact replay retained view | 3.234 MB | 3.225–3.230 MB | Essentially unchanged; source fixture excluded |
| Plain replay, 5,000 records / 6,000 items | 827.9–829.5 MB allocated | 522.9–524.2 MB | About 37% fewer allocated bytes; retained view stays ~24.75 MB |
| Runtime response, 1,000 chunks | 31.21 MB allocated | 15.29 MB | Fresh runtime per turn; setup and Close excluded |
| Runtime response, 5,000 chunks | 771.94 MB allocated | 322.56 MB | 58% fewer allocated bytes; 257→221 ms in the bounded run |
| Hub, 2,000 accumulated chunks | 261,679 B/chunk | 1,957 B/chunk | Prefix-copy cost removed from this path |
| Publish, 32 observers | 7,206 B / 67 allocations per event | 3,503 B / 34 allocations | Independent observer payloads remain |
| App fanout, 64 observers / 20,000 events | ~22.1 MB allocated | ~1.60 MB | No significant latency improvement claimed |
| Retained committed preparation, 1,000 messages / 16-property schemas | 30.30–30.32 MB | 26.16–26.18 MB | About 4.14 MB / 13.6% lower while the prepared object remains held |

TUI scale cases and retained-preparation cases use two independent processes.
Runtime turns use fixed iteration counts (20 at 100/1,000 chunks; five at 5,000)
in one process per side; timings are not precision estimates. The preparation
comparison isolates its changes against the otherwise current source, not a
whole-application before/after. Its benefit disappears once the preparation is
dropped, as expected. It applies to retained hosted preparations, not ordinary
OpenSessionView callers that immediately release that object.

The hub retained-heap fixture remains approximately 1.24–1.26 MB despite much lower
allocation volume. Small-process RSS improved in some fixtures, but there is no
same-session end-to-end RAM measurement establishing a corresponding live-app gain.

## Behavior and remaining work

Designer-defined parity checks cover compact/plain layouts at widths 60/100/160,
heights 24/40, partial/settled/scrolled/streaming/resized states, fixed-clock reply
handoffs and incremental replay equivalence. Optimizations must preserve child
expansion/scroll state, selection and link geometry, theme changes, and shell wheel
consumption at both output boundaries. Existing compact bottom-follow and selection
drift are characterized baseline defects, not silently included fixes. Final designer
sign-off includes the mid-body shell reader surviving a theme switch and all history
steps, independent child disclosure, and wheel consumption at both bounds.

Remaining structural costs are significant:

- Per-message public mutable schema graphs are still copied independently.
- Cold child restoration still hydrates the owning root history.
- Persistence still marshals accumulated response prefixes for exact journal
  accounting; no writes were batched or dropped to obtain these gains.
- Full rendered transcript strings remain retained; this is not viewport-only
  rendering. Compact replay still has growing projection/assembly work, so the
  measured gains do not prove fully linear scaling.
- No realistic multi-session soak or whole-process 10× result is claimed.

## Validation

- Build passed with `CI=true GOFLAGS='-p=2' task build`. Changed-line lint reported
  zero issues; attributable formatting, whitespace and empty-staging checks passed.
- Ten full TUI/chat package suites passed, alongside focused session clone/snapshot,
  runtime streaming/persistence/observer/restore, app fanout/cancellation, root UI,
  and gated synthetic investigation tests. Full-repository success is not claimed:
  the broader affected-package attempt exposed 13 failures reproduced on the exact
  pre-change snapshot, including panic-shortened runs.
- The final baseline/candidate comparison matched 180 matrix artifacts, 42 coverage
  artifacts, and four replay traces byte for byte. Eight rebuild oracles matched,
  and all three backing-release regressions passed. Artifacts include ANSI, stripped
  text and metadata; these counts are not counts of independent screenshots.
- Designer sign-off covers the tested phase-1 parity matrix, not viewport rendering,
  remote resets, scrollbar dragging, keyboard navigation, or every Visible replies
  setting during paging. Known baseline rendering quirks remain documented.

Detailed local evidence is in `/tmp/docker-agent-memory-fixes.sYX2uR/`: `results.md`,
`baseline-failures.md`, `design-contract.md` (§9 FINAL),
`parent-final-verification-run.log`, `parent-final-parity-run.log`, and
`parent-preservation.log`. The live process and real sessions were not modified;
no staging, commits or deployment were performed.

## Reproduction

Run one command at a time from the repository root. Source `.env.test`, set
`SDKROOT="$(xcrun --sdk macosx --show-sdk-path)"`, and keep CGO enabled.

```sh
DA_INVESTIGATION_MEMORY=1 DA_INVESTIGATION_MODE=compact DA_INVESTIGATION_MESSAGES=1000 \
  go test -p=2 ./pkg/tui/components/messages \
  -run '^TestInvestigationReplayScaleAndCleanup$' -v -count=1

go test -p=2 ./pkg/runtime -run '^$' \
  -bench '^BenchmarkInvestigationStreamingTurn$/chunks=1000$' -benchmem -benchtime=20x

INVESTIGATION_PREPARED=1 go test -p=2 ./pkg/runtime \
  -run '^TestInvestigationPreparedViewRetention$' -v -count=1

INVMEM_HEAVY=1 go test -p=2 ./pkg/runtime -run '^$' \
  -bench '^BenchmarkInvMemInflightChunks$' -benchmem -benchtime=2000x
```

Do not start with the earlier 24k compact fixture: its baseline took about 983
seconds. Scale bounded workloads first. Full-repository test/lint success is not
claimed; confirmed baseline failures are separate from the passing changed-path checks.
