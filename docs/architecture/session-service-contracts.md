# Session service contracts

This describes implemented runtime guarantees, not a distributed service design.
The [ownership model](session-ownership.md) remains the authority for driver,
registry, child-record, transcript journal and coordination-outbox transitions.

## Authority and lifetimes

`runtime.SessionService` routes stable session IDs across its registered local
runtimes. Claims are exclusive across services **within one process**; they
include reserved/resident identities and obey the service's aggregate session
limit. Other resource limits remain per runtime. Conflicting owners and ambiguous
cold-load routes fail explicitly rather than choosing a runtime arbitrarily.
Cold routing checks the stored agent binding and workspace; child attachment
uses prepared ancestry validation. Runtime shutdown unregisters it and releases
its claims after draining.

`SessionService.Runtime()` and `SessionRuntimeSupervisor.Runtime()` return
borrowed routing interfaces, not shutdown authority. An observation or UI view
owns its subscription and projection, not the session. Detaching closes those
resources without cancelling execution. The explicit owner retains the
supervisor and calls `Shutdown`; stores remain caller-owned. A supplied team,
backend or transport also retains its own lifecycle owner: service shutdown is
not a universal resource destructor.

## Persistence and turn evidence

Metadata writes serialize with `Session.LockMetadata`. The persistence observer's
run-start journal takes a fresh authoritative metadata snapshot **on each retry**,
not the stale snapshot captured when a write was queued. Handle edits commit to
the store before changing live metadata. `LocalRuntime.UpdateSessionTitle` routes
resident sessions through the driver; cold writes update only the title under
the metadata lock. A stale title caller cannot overwrite safety or permissions.

Settlement flushes transcript effects and commits terminal turn evidence before
publishing `TurnSettled` or releasing settlement waiters. Storage failure retains
accepted work and settlement/journal state for retry; cancellation does not turn
failed persistence into successful completion. Request identities and supported
store append receipts reconcile retries rather than blindly duplicating input.

`Session.TurnOutcomes` retains at most **1024** terminal identities (completed,
failed or canceled), including consumed steering identities. `AwaitTurn` waits
for the requested identity, not session-wide idleness. Its successful return
normally means settlement is known, not that the model succeeded: inspect the
outcome. Consumed steering is the exception: success can acknowledge safe-boundary
delivery before its owning turn settles. Unknown
identities return `not_found`. An accepted, promoted transcript identity without
terminal evidence returns `interrupted` once it is no longer pending/active;
it must not be automatically replayed. `SessionStatus.InterruptedTurns`, also
projected through HTTP snapshots/status, distinguishes this uncertainty from
scheduler idleness. Legacy sessions and evidence evicted from the bounded window
can therefore be conservatively uncertain even if they actually completed.

## Observation and interaction identities

Snapshots and event envelopes carry an **epoch plus sequence cursor**. Epochs
belong to the process-local event hub, not durable history. Reconnecting with a
missing or different epoch starts a fresh snapshot/live baseline; a same-epoch
cursor resumes bounded replay. Retention eviction and slow subscribers produce
a gap/resnapshot boundary rather than an unbounded queue. Snapshots include
outstanding interactions and pending input; transcript positions disambiguate
snapshot/replay overlap. Turn and interaction IDs must remain exact when sending
cancellation or responses.

The ordinary single-session client helper resets on epoch change, retries
recoverable transport failures with bounded backoff, and resnapshots on gaps;
repeated gaps without progress terminate explicitly. It rejects tree observations
(including a descendant-addition channel) rather than dropping children. Tree
observation has its own contract and does not support cursor-based reconnect.
Remote subtree watching carries descendant events, so bandwidth scales with the
subtree; a temporarily missing topology is rebaselined at most eight times and
remote child visibility can briefly lag. An epoch/cursor is not an exactly-once
execution token or a durable event log.

## Invocation-scoped tools and communication

Shared toolsets use stable callback dispatchers that resolve elicitation,
sampling and OAuth ownership from the operation's `tools.HandlerScope`.
Missing scopes fail closed. Scoped MCP callbacks without an unambiguous causal
call identity, including unsolicited callbacks, are rejected rather than routed
to whichever runtime most recently installed a handler. Legacy direct setters
remain a single-owner integration boundary. Subscription APIs are required for
custom notifiers/RAG integrations; unsupported opaque todo composites or extra
toolsets without binding fail explicitly. Memory accepts an injected backend
factory; selecting one does not create a native directory as a side effect.

`send_message` addresses only the sender's parent or direct children. Typed
`DeliveryReceipt` fields distinguish acceptance, idempotent retry, durable
admission, queued state, delivery disposition and rejection. Acceptance is an
**inbox commit**, not proof of waking or model consumption. `guidance` joins a
busy recipient at a safe boundary; `new_turn` stays in its ordinary FIFO. Stable
explicit request IDs, or IDs derived from sender session plus tool-call ID,
allow exact retries; changed payload/mode under the same identity conflicts.
An unavailable live inbox cannot acknowledge durable acceptance. The internal
lifecycle orphan buffer remains volatile and is not equivalent to this receipt
contract or the durable coordination outbox.

## Adapter boundaries

Local handles and remote HTTP handles expose the same session operations and
advertised capabilities. Unsupported capabilities fail explicitly; a client
must not substitute deletion or cancellation for subtree stop.

- The App, full/lean TUI and embedded chat consume canonical handles and
  observations. Embedded chat can borrow an existing registry/session; closing
  that wrapper does not shut down the authority. Its default owned-runtime mode
  explicitly drains its supervisor, and stops toolsets only when it loaded them.
- ACP can attach a borrowed canonical session. Closing that registration detaches
  it; owned ACP sessions drain before shared toolsets stop. Historical ACP load
  replay currently projects user/assistant text, not complete tool or multimodal
  history. Delegated filesystem clients own final I/O race containment.
- MCP and A2A adapters may borrow a registry without shutting it down. Their
  default invocation-local supervisors are explicitly owned and drained. This
  does not transfer shared team/transport ownership to each invocation.
- Board uses canonical snapshots/events, interaction IDs and native attachment;
  cards reflect paused/attention state. Destructive card/worktree cleanup first
  attempts a bounded subtree stop and retains the card on failure. Only verified
  absence of the process bypasses that preflight; a missing socket is not proof.
- The WASM `connectAuthority` bridge is transport-only: server execution, tools
  and persistence remain authoritative. It validates canonical stream identities,
  epochs/sequences and chunk ordering, and treats gaps as terminal resnapshot
  barriers. CRUD requests have a 30-second bound. Subscription close/disconnect
  detaches observations, not sessions. Re-observation is caller-managed; there is
  no automatic mutation retry. Browser deployment needs HTTP(S), authentication
  and appropriate same-origin/CORS policy. The browser-local `chat`/`abort` demo
  remains a separate, simplified non-durable boundary, not service mode.

## Portable delegation policy

The optional `SessionDelegationController` reads and updates the effective
`UseSubagents` policy for one root tree. Descendants resolve the canonical root;
updates through a child affect that root, not sibling roots or a daemon-global
switch. `GET`/`PATCH /api/v2/sessions/{id}/delegation-policy` and remote handles
expose the same advertised capability. UI controls asynchronously read the
effective session-tree value and commit through that capability; they do not
rewrite the saved runtime/config default. Root policy is stored in session metadata,
committed before live mutation, and inherited after restart. A root without an
explicit policy retains its runtime default. Missing or cyclic child ancestry
fails closed for new delegation rather than falling back to an enabled default.

Policy changes serialize with new autonomous admission. Revocation prevents new
delegation but does not cancel accepted work, invalidate existing communication
or imply subtree stop. This policy is separate from safety/permission settings.
Unsupported clients must report the missing capability, not toggle a local flag
and present it as portable authority.

## Stop and recoverable start

The optional `SessionTreeController.StopSubtree` capability works for roots and
children and preserves transcript rows. Clients type-assert this capability;
it is not part of the compatibility `SessionHandle` interface. Root stop fences new tree admission, cancels/drains its driver and stops its
children under the caller's context. The root fence is process-local, not a new
durable root tombstone. Child stop uses committed subtree tombstones before
retiring/draining execution; repeated stop is idempotent. Deadline or persistence
failure is returned, not treated as successful destructive cleanup. Cancellation
of one turn, detachment, shutdown and durable child stop are distinct operations.

`POST /api/v2/sessions/start` requires a caller-selected root session ID, agent and
initial submit request ID. It persists a SHA-256 proof of the normalized supplied
creation/input contract with the initial session attributes, then submits through
the canonical handle. Exact retries recover a partially completed start; an
existing unrelated ID or changed contract conflicts. Normalization resolves the
source and default submit mode, but does not hash later mutable bindings/defaults.
This is a recoverable **two-stage operation**, not an atomic database transaction
combining session creation and first input, nor an exactly-once promise for
external model/tool effects.

## Explicit limits

Claims, event epochs and root-stop fences are process-local: there are no
distributed leases or cross-process execution fencing. Durable guarantees require
a supporting store; memory-only operation is not crash persistence. Legacy or
expired terminal evidence remains uncertain. Internal lifecycle orphan delivery
is volatile. Team, backend, transport and delegated filesystem lifetimes remain
with their owners. These boundaries are intentional and must not be hidden by an
adapter's UI, successful HTTP acknowledgement or local browser demo.
