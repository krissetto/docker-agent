# Session ownership

## Authorities

* The `sessionDriver` owner goroutine owns authoritative session mutation and
  `sessionLifecycle` for one live generation: phase (idle,
  starting, running, cancelling, settling), request identity, cancellation,
  retirement, generation output and the last settled generation. Retirement is
  process-local execution fencing, **not** a durable child stop. Interaction,
  view dormancy, maintenance reservations and accepted mailbox entries are
  orthogonal facts, not alternate running flags.
* `sessionDriverRegistry` owns residency, deletion admission fences, capacity
  accounting and scheduling. Capacity reads immutable driver lifecycle
  projections, not topology. Unknown-session delivery is rejected; there is no
  runtime-global orphan inbox.
* The committed `session.ChildRecord` owns child identity, last durable turn
  outcome/result and terminal stop. `childRecord` retains that committed value
  plus ownership/session handles; it has no independently writable lifecycle,
  result or error fields. The durable error lives only in `ChildRecord.Node.Error`.
  Driver results are uncommitted generation output until settlement commits them.
* The session store owns transcripts and accepted input. The persistence
  observer's journal owns pending writes and retry identities; it does not own
  execution or child lifecycle. The coordination outbox owns report delivery
  until atomic acceptance into the parent's inbox.

## Execution and storage boundaries

A driver has one bounded owner mailbox. State transitions run on that owner;
`d.mu` synchronizes detached snapshots and legacy internal readers, not a second
mutation authority. Execution is a long-running worker with a private mutable
session clone and its own loop state. Provider/tool work and hooks run outside
the owner. This is not a phase-by-phase execution state machine on the owner.
Worker events apply typed, generation-checked effects to the authoritative
session; they never replace it with the worker's whole clone. Active-agent,
approval, instruction and compaction changes use reserved durable effects.
Parallel tool results still require synchronized access to their execution-local
scratch; detached does not mean that scratch is immutable.

Each session's I/O lane allows one reserved write through its owner
acknowledgement. Reservation materializes a detached payload after the preceding
acknowledgement; storage runs off-owner, and completion commits the applicable
memory delta on-owner. Caller cancellation does not release an in-flight lane or
skip acknowledgement. The transcript journal retains its separate FIFO and
idempotent retry identities, bounded to 256 effects and 8 MiB. Backpressure waits
outside the owner, so blocked storage does not hold the session mutation lock.
The lane bounds active writes, not arbitrary caller goroutines waiting for it.
Owner commands share one 64-command mailbox; there is no separate priority
control queue. A canceled command that has not begun can be withdrawn; once
state-only work starts, its caller joins completion rather than returning while
closure outputs are still being written.
Stop/status/response transitions do not wait for provider or storage I/O, but
queued state-only work, including transcript snapshot copying, can delay them.
This is not a hard cancellation-latency guarantee. Providers, tools, hooks and
observers must honor cancellation for a bounded drain.

## Projections

`childRead`, session status, summaries and the swarm tree are read/event/UI
projections. A read combines the committed child outcome with the live driver
phase; terminal stop takes precedence. Tree callbacks and metrics publish
snapshots, but tree state never grants execution or consumes capacity. Legacy
saved trees are migration input; canonical child records win during restore.
Summary paging reads metadata only, ordered by creation descending then ID
ascending, retaining at most the requested page plus a lookahead row.

## Transition and failure invariants

1. Turn settlement flushes transcript writes before committing child outcome and
   outbox; only then are `TurnSettled` and waiters released. A failed barrier
   retains the generation and journal for retry, including retired drivers.
2. Explicit child-subtree stop serializes with generation admission (`runMu`)
   and root-scoped child settlement/admission transitions. It commits all child
   tombstones atomically, updates the committed cache/projection, then retires
   and drains execution.
   Neither manager nor transition mutex is held while waiting for goroutines;
   settlement releases its transition lock before invoking arbitrary hooks.
   A failed child-tombstone acknowledgement does not publish a stopped state or
   cancel that child work. Root stop instead fences and cancels root execution,
   withdraws durable pending/steering input and records canceled turn outcomes,
   then stops children. These root effects are not one atomic tombstone
   transaction; failure can leave partial progress and must be retried. The root
   admission fence remains process-local.
3. `CommitChildren` validates revision/identity/report constraints in one
   transaction (or one in-memory critical section). Stopped records cannot be
   revived or publish new reports. Unsupported custom stores fail before child
   tombstone mutation; there is no partial per-child fallback. Root cancellation
   may already have occurred when a custom store rejects pending withdrawal.
4. Repeated stop is idempotent. Ambiguous acknowledgement is reconciled using
   stable identities and revisions under a fresh bounded read context; an
   unsuccessful reconciliation leaves the same operation retriable.
5. Restore commits configuration/ancestry terminalization rather than keeping a
   separate restore-only stop flag. Shutdown retires drivers without inventing
   durable child stops. Release/delete retry pending settlement before removing
   a driver's journal, and deletion surfaces descendant persistence failures.

The existing legacy tree import and topology checkpoint formats remain for
compatibility. They are not an execution authority or a substitute for the
coordination transaction.

## Legacy HTTP input translation

`SessionLegacyInput` translates old user-message batches into the same driver's
transcript and pending FIFO. A `legacy_run` accepted control item starts one
canonical generation without adding model-visible text, including for an empty
batch. `legacy_followup` pending input remains dormant while headless/idle; an
explicit run or an existing generation's successor handoff consumes that FIFO.
There is no legacy queue or streaming flag. Queue status is derived from the
driver's pending/steering entries and phase.

`session_input_batches` stores immutable append receipts, not execution state.
Both stores commit the complete batch and receipt atomically; an optional model
override shares that transaction. The session I/O reservation serializes the
batch/binding commit; a run reservation prevents competing starts while the batch
is in flight. Generation admission remains separate from durable batch
acknowledgement. Retry probes receipts before capacity checks, and ambiguous
acknowledgements reconcile stored rows rather than replay caller payloads.
Explicit adoption stamps an existing unbound session's durable agent binding
before publishing its driver.

`SessionLegacyCommandInput` resolves switch commands against the pre-batch active
agent through `LookupCommand`/`ResolveCommand`. Only user-role inputs trigger
command interpretation; all legacy contents still become user messages. Targets
are validated before mutation. Active-agent execution settings, the pre-switch
agent's optional model override, user batch and request receipt commit together;
the initial `SessionAgentAttribute` remains unchanged. Receipts fingerprint the
original request so retry after a switch neither reinterprets nor reevaluates
commands. Active identity is stored in `sessions.execution_settings`, not in a
compatibility owner or an additional attribute.
