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

A handle's immutable creation-agent binding is distinct from its current active
agent. Capabilities resolve the canonical active agent, model and binding revision,
not the age of the handle or runtime-global selection. Results resolved across a
binding change fail stale validation; skill-operation events retain the active
agent captured at admission. Settled snapshot replacement cannot change creation
identity or the active model override and invalidates earlier binding revisions.

Creating an existing bound durable root loads its canonical stored state when
cold, rather than adopting the caller's transcript or safety defaults. Explicit
agent, model, origin, source or parent conflicts fail. Resident retries retain
owner state; cold child attachment still requires the prepared ancestry route.

## Persistence and turn evidence

Resident metadata writes use the session owner's reserved I/O lane. Detached
payloads are materialized after the preceding write acknowledgement, and storage
runs outside the owner before its memory delta commits. The persistence journal's
run-start effect obtains fresh authoritative metadata **on each retry**, not a
stale queued snapshot. Handle edits commit to the store before changing live
metadata. `LocalRuntime.UpdateSessionTitle` routes resident sessions through the
driver; cold writes update only the title under `Session.LockMetadata`. A stale
title caller cannot overwrite safety or permissions. Caller cancellation can
return before a reserved write finishes; its acknowledgement and any required
stop withdrawal still complete before the lane is released.

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

The session owner registers each confirmation or max-iteration request with its
own one-response buffered channel; elicitation uses a request-specific waiter.
There is no runtime-global response-channel or waiter registry. The owner checks
interaction kind, turn and generation, then claims and delivers a response in
one transition. Duplicate, stale-generation and mismatched elicitation responses
fail rather than resolving another request. Cancel/stop removes outstanding
interactions and cancels execution; waiting operations must honor that context.

Each event hub admits at most **256 active subscribers**, combining compatibility
and sequenced subscriptions. Requested subscriber buffers clamp to **1024 events**
plus one terminal/gap slot. Each subscriber also has an **8 MiB serialized queued
payload** budget, independent of replay retention. Oversized or backlogged
sequenced delivery produces a gap and closes; compatibility delivery disconnects.
Initial live seeds and replay are checked against the same payload budget before
large snapshots are cloned. These bounds do not cap the authoritative transcript
or the memory required for a session snapshot.

The ordinary single-session client helper resets on epoch change, retries
recoverable transport failures with bounded backoff, and resnapshots on gaps;
repeated gaps without progress terminate explicitly. It rejects tree observations
(including ordered tree updates or a descendant-addition channel) rather than
dropping children. Tree observation does not support cursor-based reconnect.
`OrderedTree` selects one bounded `TreeUpdates` tail: each dynamically admitted
child arrives as an atomic snapshot plus sequence-zero live seeds, before any
of that child's live events. Those seeds preserve uncommitted assistant and tool
state and do not advance the journal cursor. HTTP carries dynamic seeds in the
additive `snapshot.live_seeds` field, including chunked snapshots. Legacy
`SessionsAdded`/`Events` projections remain available, but separate channels do
not guarantee snapshot/event ordering; older clients ignore the added seeds.
Remote subtree watching carries descendant events, so bandwidth scales with the
subtree; a temporarily missing topology is rebaselined at most eight times and
remote child visibility can briefly lag. An epoch/cursor is not an exactly-once
execution token or a durable event log.

## Invocation-scoped tools and communication

Shared toolsets use stable callback dispatchers that resolve elicitation,
sampling and OAuth ownership from the operation's `tools.HandlerScope`.
Missing scopes fail closed. MCP callback ownership is established in two ways:
response-derived `InputRequests` fulfilled inline by the installed SDK retain a
private exact-operation context marker; older reverse JSON-RPC callbacks must
explicitly echo the originating `tools/call` capability described below. Both
paths validate active call membership, exact SDK session, connection generation
and cancellation before elicitation or either sampling handler. Temporal
activity (even a sole in-flight call) and connection-level scopes never confer
ownership. Legacy direct setters remain a single-owner integration boundary,
but cannot bypass this MCP causal validation. Callback contexts retain the
originating operation's scope, including an explicit `WithoutHandlerScope` opt-out;
causal ownership does not install or re-enable a scoped handler. Subscription APIs
are required for custom notifiers/RAG integrations; unsupported opaque todo composites or extra
toolsets without binding fail explicitly. Unsolicited tool-list notifications
retain their subscribed agent identity; a shared toolset fans out to every
affected agent in each runtime, without inferring a session from global current
selection. Memory accepts an injected backend
factory; selecting one does not create a native directory as a side effect.

#### MCP callback interoperability

The public metadata key `docker-agent.dev/callback-token` has a JSON string
value. Each `tools/call` carries a fresh cryptographically random capability in
`params._meta`; input metadata maps are copied rather than modified. A
cooperating server implementing reverse elicitation or sampling copies that
string into the callback's `params._meta`:

```go
meta := mcp.Meta{
    "docker-agent.dev/callback-token": req.Params.Meta["docker-agent.dev/callback-token"],
}
result, err := req.Session.Elicit(ctx, &mcp.ElicitParams{
    Meta: meta, Message: "Confirm deployment?",
})
```

The same key applies to both sampling variants. Missing, non-string, unknown,
expired or foreign-session capabilities are rejected **before** any registered
handler runs. Capabilities expire when the call completes, its owning context
is canceled (including resource-owner retirement), or the connection closes or
changes generation. They authorize only callbacks for that active tool call,
not API access, OAuth, tool execution or other host handlers. Tokens are removed
from metadata before dispatch to application handlers; integrations must never
log, persist, put them in transcripts, or treat them as API/bearer credentials.

Stock modern servers returning response-derived `InputRequests` need no token
echo: the installed SDK fulfills those inline with the exact private outgoing
call context. Stock older servers sending untagged reverse callbacks receive an
error (no prompt or sampling/provider invocation), even with only one active
operation; ordinary tool calls remain supported. Explicit echo is a cooperating
server protocol, not proof against a server that knowingly misuses another
active call's capability. Connection isolation alone is not callback causality.

Startup tool-info projection admits at most **64 subscribers per agent** and
retains at most **128 history events**; each live subscriber queue also caps at
**128 events**. The publisher never waits on a subscriber: cancellation or a
rejected send unregisters it, and a full live queue removes and closes the slow
subscription. Capacity rejection or overflow ends only that startup projection;
tool discovery and execution continue. A caller must re-emit startup info to
refresh a rejected or truncated projection. History is a bounded latest tail,
not a durable or lossless startup log.

The compatibility `LiveSessions` view projects detached canonical owner state;
caller session objects supply identity only. It lists active owners within the
addressed canonical root tree and retains settled parents for ancestry, without
a separate writable live-session registry. `CompactLiveSession` forwards to the
addressed owner's compaction reservation; active-turn requests execute at a safe
boundary or drain during teardown, rather than using an independent mailbox.

`send_message` addresses only the sender's parent or direct children. Typed
`DeliveryReceipt` fields distinguish acceptance, idempotent retry, durable
admission, queued state, delivery disposition and rejection. Acceptance is an
**inbox commit**, not proof of waking or model consumption. `guidance` joins a
busy recipient at a safe boundary; `new_turn` stays in its ordinary FIFO. Stable
explicit request IDs, or IDs derived from sender session plus tool-call ID,
allow exact retries; changed payload/mode under the same identity conflicts.
An unavailable live inbox rejects delivery, including internal lifecycle notes;
there is no unknown-session buffer. Durable child completion reports use the
coordination outbox and are restored through canonical sessions.

## Hooks and extension correctness

`WithHooksRegistry` takes a private registration snapshot; later caller registry
changes and construction of another runtime cannot replace its handlers. Caller
stock-builtin overrides are preserved. `cache_response` is runtime-owned and a
caller registration with that name is rejected; the model-hook factory is bound
to the runtime's provider registry. Handler closures remain caller-owned and
must support concurrent sessions. Hook inputs identify both the emitting session
and its canonical root tree.

`WithMessageTransform` is a best-effort projection boundary, not authorization or
redaction authority. Each transform receives detached messages and attribution;
errors and panics discard its rewrite without changing the retained input.
`WithMessagePolicy` is the authoritative outbound boundary: policies run after
projections on every provider attempt, including retries and fallbacks, and before
external harness delivery. An error, panic or cancellation prevents delivery and
is not retried through another model. Policies can inspect resolved model
capabilities where available; external harnesses supply none. Invalid policy
registrations fail closed rather than silently removing enforcement. Callbacks
execute on the existing owned execution worker and must honor cancellation;
shutdown retains that worker and reports a drain deadline if a callback ignores
its context, rather than abandoning an extension goroutine or claiming success.

Custom `EventObserver` callbacks receive independently detached session/event
snapshots on an observation worker. They cannot mutate execution, change another
observer's payload or suppress persistence; panics are contained. Callbacks remain
ordered and a slow observer can backpressure observation. Cancellation is
cooperative: joins wait for callbacks, so an extension that ignores its context
can prevent a bounded drain. There is no hard callback-kill guarantee.
Persistence is an explicit authoritative journal path, not a best-effort custom observer. Dynamic
instruction assembly uses worker-local scratch state and returns only the
instruction-context effect for durable owner commit, not a stale whole-session
metadata write.

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
  default invocation-local supervisors are explicitly owned and drained. Server
  teardown fences new invocations and joins accepted workers before closing
  owned dependencies. A drain deadline retains that ownership and returns a
  retriable drain error, not permission to close dependencies early. This does
  not transfer shared team/transport ownership to each invocation.
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
children and retains completed transcript history. Clients type-assert this
capability; it is not part of the compatibility `SessionHandle` interface. Root
stop fences new tree admission and cancels its driver. It durably withdraws
accepted pending and steering input and records canceled outcomes, including
reconciliation of a reserved append that commits after cancellation, before
acknowledging successful stop. It also stops and drains children under the
caller's context. Withdrawn pending entries do not replay after reload. The root
admission fence remains process-local: withdrawal is durable, but there is no new
durable root tombstone preventing later explicit execution after restart.

Child stop uses committed subtree tombstones before retiring/draining execution;
repeated stop is idempotent. Root withdrawal and child stops are separate durable
operations, not one atomic tree transaction. Deadline or persistence failure can
leave partial stop progress and is returned for retry, not treated as successful
destructive cleanup. Stores must support pending-message deletion to withdraw
durable queued input. Cancellation of one turn, detachment, shutdown and durable
subtree stop are distinct operations; shutdown preserves queued root input.

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

Runtime execution and tool-handler concurrency default to 32 each through
`SessionResourcePolicy.MaxExecutions` and `MaxTools`. Positive limits wait for
capacity with context cancellation; zero disables admission and returns a typed
capacity error. Tool batches use bounded workers. Synchronous delegation lends
the waiting parent's execution slot and invocation's tool permit to canonical
children, then reacquires both before continuing; nested delegation therefore
progresses with a single slot without bypassing the bounds. Coordination-only
`transfer_task` and `handoff` handlers do not retain tool-handler permits.

Claims, event epochs and root-stop fences are process-local: there are no
distributed leases or cross-process execution fencing. Durable guarantees require
a supporting store; memory-only operation is not crash persistence. Legacy or
expired terminal evidence remains uncertain. Explicit root-stop withdrawal is
durable on supporting stores, while its admission fence is process-local. Team,
backend, transport and delegated filesystem lifetimes remain with their owners.
These boundaries are intentional and must not be hidden by an adapter's UI,
successful HTTP acknowledgement or local browser demo.

## Canonical delegation and root budgets

`transfer_task`, synchronous `RunAgent`, `spawn_subagent`, and the compatibility
background-agent tool names all admit children through the canonical child
manager. There is no separate background task registry or fixed background-task
concurrency/depth policy. The configured descendant and depth limits apply to
all routes, including zero-valued limits. Blocking delegation temporarily lends
execution/tool capacity while waiting for its canonical child, then reacquires
capacity before the parent continues. Compatibility list/view/stop tools cannot
inspect or stop another root's children; directed communication remains limited
to the parent and direct children.

Budget wallets are isolated per canonical root tree and shared by its
descendants across turns. Elapsed limits cancel the execution context, including
provider/tool waits, and use wall time from the wallet's first execution rather
than summing concurrent child durations. Cost/token usage is charged when usage
is reported; exhausted shared wallets cancel participating executions and stop
further execution. Token accounting includes cached prompt tokens. These limits
are not exact external billing ceilings: concurrent or already-started calls can
report spend above a limit, and unpriced usage cannot enforce a monetary ceiling.
Wallets are process-local; persisted transcripts do not restore their counters or
elapsed clock after restart. Driver eviction retains a root wallet so reopening
a session cannot reset its allowance; deleting the root releases its wallet. Cancellation requires providers/tools to honor their
context and cannot undo an external effect already performed.
