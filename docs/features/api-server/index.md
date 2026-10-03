---
title: "API Server"
description: "Expose your agents via an HTTP API for programmatic access, web frontends, and integrations."
keywords: docker agent, ai agents, features, api server
weight: 80
canonical: https://docs.docker.com/ai/docker-agent/features/api-server/
---

_Expose your agents via an HTTP API for programmatic access, web frontends, and integrations._

## Overview

The `docker agent serve api` command starts an HTTP server that exposes your agents through a REST-style API with Server-Sent Events (SSE) streaming. Use it to build web UIs, integrate with CI/CD pipelines, or connect agents to other services.

```bash
# Start the API server
$ docker agent serve api agent.yaml

# Custom listen address
$ docker agent serve api agent.yaml --listen 0.0.0.0:8080

# With session persistence
$ docker agent serve api agent.yaml --session-db ./sessions.db

# Auto-refresh from OCI registry every 10 minutes
$ docker agent serve api myorg/coder --pull-interval 10
```

> [!TIP]
> **When to use API server vs. chat server**
>
> Use the **API server** when you want full control over sessions, agent execution, tool-call confirmations, and streamed runtime events — this is Docker Agent's native protocol. Use the [Chat Server](../chat-server/index.md) when you want to plug Docker Agent into existing OpenAI-compatible tooling (chat UIs, IDE integrations, OpenAI SDK clients) instead.

## Endpoints

Most endpoints are under the `/api` prefix. The process health and readiness probes are the exceptions: `/health` and `/ready` are top-level routes.

### Agents

| Method | Path              | Description                       |
| ------ | ----------------- | --------------------------------- |
| `GET`  | `/api/agents`     | List all available agents         |
| `GET`  | `/api/agents/:id` | Get an agent's full configuration |
| `GET`  | `/api/agents/:id/:agent_name/tools/count` | Count the named agent's available tools |

Each agent entry in the `GET /api/agents` response contains:

| Field        | Type            | Description                                                                                   |
| ------------ | --------------- | --------------------------------------------------------------------------------------------- |
| `name`       | string          | Agent identifier (config filename without `.yaml`).                                           |
| `description`| string          | The root agent's `description` field.                                                         |
| `multi`      | boolean         | `true` when the config defines more than one agent.                                           |
| `commands`   | array of string | Sorted list of named command keys defined on the root agent. Omitted when no commands exist.  |

### Remote agent sources

For an agent loaded from a remote HTTP(S) configuration source, endpoints that need to load that source return `502 Bad Gateway` when fetching it fails. A missing configured agent returns `404 Not Found`; invalid source URLs or configuration return `500 Internal Server Error`.

### Canonical sessions

New integrations use `/api/v2/sessions`. The unversioned upstream HTTP API
remains available through compatibility translation over the same session
runtime; it is not a second execution engine. There is no `/api/v1` alias.

The canonical endpoints are:

| Method | Path | Description |
| --- | --- | --- |
| `GET` / `POST` | `/api/v2/sessions` | Catalog sessions (`?active=true` lists attached sessions without reading stored history) / create a session-bound session (`source`, `agent_name`, optional `model`, `title`, `working_dir`, `safety_policy`, `tools_approved`, `permissions`). |
| `GET` / `DELETE` | `/api/v2/sessions/:id` | Inspect or delete the session. Legacy stored rows remain inspectable but are not attachable. |
| `PATCH` | `/api/v2/sessions/:id` | Canonical edit: `kind` is `policy`, `permissions`, `title`, `message`, `summary`, `tokens`, or `attachment`; returns the updated session. Transcript edits require a quiescent session. |
| `GET` | `/api/v2/sessions/:id/status` | Session state, active turn, pending inputs, and last error. |
| `GET` | `/api/v2/sessions/:id/snapshot` | Canonical transcript/status/interactions snapshot and cursor. |
| `GET` | `/api/v2/sessions/:id/events` | Versioned SSE snapshot, replay, ready barrier, and live ordered envelopes. |
| `POST` | `/api/v2/sessions/start` | Recoverable idempotent root creation and initial submission. Requires `session_id` and `input.request_id`; retry the identical creation/input payload. |
| `POST` | `/api/v2/sessions/:id/messages` | Submit input. `mode` is `submit` (starts a turn, or queues one when a turn is running) or `steer` (urgent in-turn input). |
| `POST` | `/api/v2/sessions/:id/responses` | Answer a confirmation, max-iteration, or elicitation using required `interaction_id` and `kind`. |
| `POST` | `/api/v2/sessions/:id/cancel` | Cancel the active or exactly named active/queued `turn_id` without closing the session. |
| `GET` / `PATCH` | `/api/v2/sessions/:id/delegation-policy` | Read or set session-tree delegation (`enabled` boolean). A child resolves its verified canonical root. Absent override uses the local runtime default; updates persist on the root and affect only new delegations, never daemon-global preferences or existing children. |
| `POST` | `/api/v2/sessions/:id/stop-subtree` | Stop a child session and its descendants without deleting history. Requires the `stop_subtree` capability; including a root and its entire tree. |
| `POST` | `/api/v2/sessions/:id/turns/:turnID/wait` | Wait for this exact turn to settle, including durable completion; `204` on success, typed `404` for unknown/expired turns. Disconnecting cancels only the wait. |
| `POST` | `/api/v2/sessions/:id/retry` | Retry the last failed settled turn. |
| `PATCH` | `/api/v2/sessions/:id/title` | Session-ordered durable title change. |
| `GET` | `/api/v2/sessions/:id/tree` | Authoritative subtree rooted at any session node; metrics are cumulative per node and can be summed for a subtree rollup. |
| `GET` | `/api/v2/sessions/:id/todos` | Current session-keyed todo snapshot. SQLite-backed runtimes persist it; other stores may provide volatile storage. `shared: true` toolsets use the stable root-session ID so agents in one tree share a list without cross-root leakage. |
| `POST` | `/api/v2/sessions/:id/compact` | Compact the session at its execution boundary. |
| `POST` | `/api/v2/sessions/:id/compact/:target` | Compact a live target in the same session tree. |
| `GET` | `/api/v2/sessions/:id/context` | Inspect context-window composition. |
| `GET` | `/api/v2/sessions/:id/live-sessions` | List live sessions visible from this session. |
| `GET` / `POST` | `/api/v2/sessions/:id/skills` / `/api/v2/sessions/:id/skills/run` | List skills or start a fork skill. |
| `GET` / `POST` | `/api/v2/sessions/:id/models` / `/api/v2/sessions/:id/models/refresh` | List or refresh available models. |
| `PATCH` | `/api/v2/sessions/:id/model` | Change the session's pinned model. |
| `GET` / `PATCH` | `/api/v2/sessions/:id/thinking-level` | Read or set the thinking level. |
| `POST` | `/api/v2/sessions/:id/thinking-level/cycle` | Cycle to the next thinking level. |
| `POST` | `/api/v2/sessions/:id/pause` | Toggle iteration-boundary pause. |
| `POST` | `/api/v2/sessions/:id/switch-agent` | Branch into a new session bound to another agent. |
| `PATCH` | `/api/v2/sessions/:id/starred` | Set starred state. |
| `DELETE` | `/api/v2/sessions/:id/attachments` | Remove an attachment. |

| `GET` | `/api/v2/server` | Authenticated server identity, readiness and portable capability discovery without reading session history. |
| `GET` | `/api/v2/sessions/:id/agent-info` | Session-bound agent presentation metadata. |
| `GET` | `/api/v2/sessions/:id/tools` | Tool definitions and lifecycle statuses. |
| `POST` | `/api/v2/sessions/:id/toolsets/restart` | Restart an eligible toolset at a safe session boundary. |
| `GET` | `/api/v2/sessions/:id/permissions` | Effective session permissions. |
| `GET` | `/api/v2/sessions/:id/mcp/prompts` | Discover MCP prompts. |
| `POST` | `/api/v2/sessions/:id/mcp/prompts/execute` | Expand an MCP prompt to text. |
| `POST` | `/api/v2/sessions/:id/branches` | Create a canonical branch with optional transcript position and expected-snapshot proof. |
| `PATCH` | `/api/v2/sessions/:id/todos/:todoID` | Update todo status or description; description edits require the expected description. |
| `DELETE` | `/api/v2/sessions/:id/todos/:todoID` | Remove a session todo. |

Additional canonical metadata and editing endpoints:

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/api/v2/sessions/:id/tools/toggle` | Toggle tool auto-approval. |
| `PATCH` | `/api/v2/sessions/:id/safety-policy` | Update safety policy. |
| `PATCH` | `/api/v2/sessions/:id/permissions` | Update permissions. |
| `PATCH` | `/api/v2/sessions/:id/tokens` | Update persisted token/cost totals. |
| `POST` | `/api/v2/sessions/:id/fork` | Fork before a user message. |
| `PATCH` | `/api/v2/sessions/:id/messages/:msg_id` | Update a persisted message. |
| `POST` | `/api/v2/sessions/:id/summaries` | Add a persisted summary. |
| `GET` | `/api/v2/sessions/:id/recovery` | Read recovery data. |
| `POST` | `/api/v2/sessions/batch/delete` | Delete sessions in a batch. |
| `POST` | `/api/v2/sessions/batch/export` | Export sessions in a batch. |

### Catalog pagination and confirmed views

Catalog responses contain metadata only, never message bodies. `view=summary`
selects the dedicated metadata schema. Use `include_children=true` to include
child candidates; a child marked `requires_confirmation` is not an access grant.
Ancestry, source, agent binding and durable tree membership are checked only on
confirmed selection, not while paging the catalog.

Use `limit` (default 50, maximum 200) and the opaque `next_cursor` from the response
as the next request's `cursor`. Preserve the same `view`, `include_children` and
`active` scope across pages; malformed or differently scoped cursors return 400.
Stored rows are ordered by creation time descending, then session ID ascending.
Pages use bounded metadata queries, not full transcript hydration. Concurrent
inserts or deletes do not provide a point-in-time catalog snapshot. Start a fresh
scan to discover new rows inserted before the current cursor.

`active=true` is a bounded live metadata projection ordered by session ID and
does not read historical transcripts. It cannot be combined with `view=summary`.
Fetch one session's detail or snapshot explicitly when its transcript is needed.
`GET .../:id?view=prepare-info` confirms identity without publishing execution;
`PATCH .../:id` with `{"kind":"open_view"}` commits a dormant view through the
same runtime owner. Neither operation starts a model turn.

Todo state is mutated through the configured todo tools; the API endpoint is a
read-only projection and is not a direct todo mutation surface. SQLite persistence
survives restart. Memory/custom backends retain their own durability guarantees.

A session created with `working_dir` runs on a runtime whose toolsets operate
in that directory (the server builds one runtime per source and working
directory, and rebuilds for new sessions when `--pull-interval` refreshes the
source). Without `safety_policy` or `tools_approved` the session starts in the
agent's author-declared safety mode, exactly like a fresh local session.

The canonical API uses `messages` and `responses` rather than separate
resume, elicitation, steer, follow-up, or execution-stream routes. All execution
is session-owned and canonical interactive responses are correlated by the
envelope's immutable `interaction_id`. The unversioned compatibility routes
translate older requests into those same operations; see the migration table
below.

### Sending and observing a turn

```bash
SID=$(curl -s -X POST http://localhost:8080/api/v2/sessions \
  -H 'Content-Type: application/json' \
  -d '{"agent_name":"root"}' | jq -r .session_id)

curl -N http://localhost:8080/api/v2/sessions/$SID/events &
curl -X POST http://localhost:8080/api/v2/sessions/$SID/messages \
  -H 'Content-Type: application/json' \
  -d '{"mode":"submit","content":"Hello","request_id":"greeting-1"}'
```

Input provenance is server-authored metadata: `input_origin` is `user`, `agent`,
or `runtime`; `sender_id` and `sender_name` attribute explicit agent communication.
Pending snapshot inputs and accepted/promoted/user-message events also carry
`input_mode`, independently of provenance. Stored session messages keep their
existing `actor_input_mode` field. These are response/event fields, not accepted
fields on public `/messages` requests.

Normal queued-user displays exclude typed agent/runtime inputs, but canonical
`status.pending` still counts all accepted inputs. Render agent communication
with its clean body and typed sender when consumed; do not render runtime-origin
input as chat or queue text. Missing or unknown origins are legacy/unprivileged
and remain visible: never classify input from its text or mode. In particular,
a user's literal `<system_info>` text is ordinary user content.

For echo reconciliation, `user_message.turn_id` identifies the accepted input;
the envelope's `turn_id` may instead identify the active execution. Use accepted
input identity and snapshot transcript positions, not matching message bodies,
to avoid replay duplicates without dropping distinct identical messages.

Submission `request_id` replaces the formerly ignored `client_id` field. The
canonical runtime deduplicates matching payload/mode requests per session and
returns the original submission; reusing an ID for different input returns
`409 conflict`. Accepted identities survive restart while their transcript is
retained; canceled/retry identities have a bounded recent retention window.
Omit `request_id` to submit independently each time. Persistence-blocked
submissions return `503` with typed `error: "persistence"` and a diagnostic
`detail`; they are not accepted or automatically retried by the transport.

For `PATCH /api/v2/sessions/:id`, send a `kind` and its matching payload:
`policy` uses `safety_policy`, `tools_approved`, or `toggle_tools_approved`;
`permissions` uses `permissions`; `title` uses `title`; `message` uses
`message_index` and `message`; `summary` uses `summary`; `tokens` uses
`input_tokens`, `output_tokens`, and `cost`; `attachment` uses `attachment_path`.
The runtime serializes edits and persistence; message/summary/token edits reject
busy sessions instead of racing a turn. Existing policy and catalog routes
remain available and delegate canonical rows to the same owner.

The SSE stream starts with a versioned snapshot, emits zero or more replayed
`event` messages, then a `ready` message whose cursor matches the snapshot.
Snapshots up to 64 KiB use `snapshot`. Larger snapshots use contiguous
`snapshot_begin`, `snapshot_chunk`, `snapshot_end` frames, all with the same
`cursor`. Each chunk's `chunk` field is base64 encoding of at most 64 KiB of
snapshot JSON bytes; concatenate decoded bytes, not JSON strings. No events
interleave within a snapshot. The decoded snapshot cursor must match the frame
cursor. Clients must reject oversized chunks, mismatched boundaries and missing
end frames. This bounds framing overhead, not total reconstructed history RAM;
use request cancellation/deadlines to bound waits. History is never truncated.
The chunk extension and `request_id` field belong to the v2 wire contract;
unversioned clients continue to receive raw runtime events, not these frames.

Live event envelopes carry monotonically increasing sequence IDs,
`turn_id`, and (for interactions) `interaction_id`. `interaction_resolved`
removes the matching pending interaction and carries reason `responded`,
`canceled`, or `stopped`, with the same session/interaction envelope identity.
`stream_stopped` is not transport EOF or a durable-completion barrier; use the
turn wait endpoint when durable settlement is required.

Reconnect with `?since=<last sequence>&since_epoch=<snapshot epoch>` (or use
`Last-Event-ID` for the numeric sequence and retain the `since_epoch` query).
Snapshots and envelopes carry an `epoch`; sequences are ordered only within
that epoch. A matching retained cursor is replayed before `ready`. A missing
or mismatched epoch establishes a fresh snapshot, outstanding interactions,
and live seeds rather than replaying business events. Clients must replace
their projection when the snapshot epoch changes. Numeric SSE IDs remain
compatible, but alone cannot resume an earlier process's journal. A `gap`
envelope is a hard resnapshot barrier.
When `DELETE /api/v2/sessions/:id` closes an attached stream, the stream
emits a terminal `stream_stopped` event with reason `deleted` before transport
EOF. Clients should consume that terminal event rather than treating EOF alone
as successful deletion.

Use `/api/v2/sessions/:id/events?tree=true` for a rooted multiplexed stream. It
rejects cursors, emits a fresh snapshot for every current node, then events
with per-session sequences, and observes descendants added later. It emits no
SSE IDs. Reconnect without a cursor for fresh snapshots and events from then.
Observer disconnect never stops sessions. `POST /api/v2/sessions` accepts optional
`parent_session_id`; such children begin idle. Tree nodes expose
`needs_attention` and `waiting_on` (`failed`, `approve tool`, or `answer
question`, in that precedence). `/children` is removed.

To answer an interaction:

```bash
curl -X POST http://localhost:8080/api/v2/sessions/$SID/responses \
  -H 'Content-Type: application/json' \
  -d '{"interaction_id":"<from envelope>","kind":"confirmation","confirmation":"approve"}'
```

### Migrating the upstream HTTP API

The unversioned `/api/sessions` protocol and canonical `/api/v2/sessions`
protocol are **different wire formats**, not interchangeable URL prefixes.
Existing HTTP clients can continue using the unversioned routes. New clients
should use v2 for correlated interactions, explicit acceptance identities,
bounded catalogs, and snapshot/replay barriers.

| Upstream HTTP request | Canonical v2 operation | Compatibility treatment |
| --- | --- | --- |
| `GET /api/sessions` | `GET /api/v2/sessions` | Adapted: old clients receive a bare array with `id`; v2 returns a versioned catalog with `sessions`, `session_id`, and `next_cursor`. |
| `POST /api/sessions` with a session template | `POST /api/v2/sessions` with a binding request | Adapted: old creation returns a session with `id`; v2 returns session metadata with `session_id`. Old execution selects its source through `/agent/:source[/agent_name]`. |
| `GET /api/sessions/:id` and `.../snapshot` | v2 detail and snapshot | Adapted: old snapshot has flat `messages`, `streaming`, and `last_event_seq`; v2 has `session`, `status`, `interactions`, `pending_inputs`, and `cursor`. |
| `POST .../agent/:source[/agent_name]` | `POST .../messages` plus `GET .../events` | Adapted: old `messages` arrays start session-owned work and receive raw runtime SSE events; v2 separates acceptance from observation. |
| `POST .../steer` and `.../followup` | `POST .../messages` with `mode: "steer"` or `"submit"` | Adapted into the same session mailbox, not independent legacy queues. |
| `GET .../queue` | v2 status and snapshot pending inputs | Adapted: old `steer`/`followup` depth fields project the canonical pending inputs. |
| `POST .../resume` and `.../elicitation` | `POST .../responses` | Adapted only when exactly one matching interaction can be identified. Zero or multiple matches return `409 Conflict`; use v2 `interaction_id` rather than guessing. |
| `GET .../events` | v2 events | Adapted raw events, not v2 envelopes or snapshot chunk frames. See replay restrictions below. |
| Title, starred, policy, permissions, token, message, summary, fork, recovery, and batch routes | Corresponding v2 session operations | Retained adapters. Mutations remain subject to the canonical owner's admission and persistence checks; transcript edits cannot race a busy turn. |
| `/health`, `/ready`, `/api/ping`, agent discovery, OAuth callback | Same routes | Direct, unchanged by session API versioning. |

The complete retained unversioned session routes are:

| Method | Path | Description |
| --- | --- | --- |
| `GET` / `POST` | `/api/sessions` | Bare-array catalog / create a stored session template. |
| `GET` / `DELETE` | `/api/sessions/:id` | Legacy detail / delete through the session owner. |
| `GET` | `/api/sessions/:id/status` | Legacy live status; optional `wait` duration bounds readiness waiting. |
| `GET` | `/api/sessions/:id/snapshot` | Flat snapshot with `last_event_seq`. |
| `GET` | `/api/sessions/:id/events` | Raw-event replay and live observation. |
| `POST` | `/api/sessions/:id/agent/:agent` | Bind the source named by `:agent` on first run and execute. |
| `POST` | `/api/sessions/:id/agent/:agent/:agent_name` | First run with an explicitly named agent in that source. |
| `POST` | `/api/sessions/:id/resume` | Respond to the sole matching confirmation or iteration prompt. |
| `POST` | `/api/sessions/:id/elicitation` | Respond to the sole matching elicitation. |
| `POST` | `/api/sessions/:id/steer` | Submit message-array guidance to the active turn. |
| `POST` | `/api/sessions/:id/followup` | Append message-array follow-ups to the canonical mailbox. |
| `GET` | `/api/sessions/:id/queue` | Project steer and follow-up depths and capacities. |
| `POST` | `/api/sessions/:id/messages` | Append a stored message at a quiescent boundary, not a v2 input request. |
| `PATCH` | `/api/sessions/:id/messages/:msg_id` | Edit a stored message at a quiescent boundary. |
| `PATCH` | `/api/sessions/:id/title` | Durable title update. |
| `PATCH` | `/api/sessions/:id/starred` | Set starred state. |
| `GET` | `/api/sessions/:id/models` | Legacy model list for the session. |
| `POST` | `/api/sessions/:id/tools/toggle` | Toggle tool auto-approval. |
| `PATCH` | `/api/sessions/:id/safety-policy` | Update safety policy. |
| `PATCH` | `/api/sessions/:id/permissions` | Update permissions. |
| `PATCH` | `/api/sessions/:id/tokens` | Update stored usage at a quiescent boundary. |
| `POST` | `/api/sessions/:id/fork` | Fork before a user message. |
| `POST` | `/api/sessions/:id/summaries` | Append a stored summary at a quiescent boundary. |
| `GET` | `/api/sessions/:id/recovery` | Read recovery data. |
| `POST` | `/api/sessions/batch/delete` | Delete the selected sessions. |
| `POST` | `/api/sessions/batch/export` | Export the selected sessions. |

The old `client_id` was not a deduplication promise. To safely retry a submission,
migrate to v2 `request_id`; do not replay an old execution POST merely because
its stream disconnected. Canonical accepted work belongs to the session, not
its observer. A legacy
execution POST that freshly admits a turn retains historical request ownership:
disconnecting it cancels and drains that exact turn, not unrelated queued work.
The cancellation drain is bounded to five seconds. A reused submission must
not acquire cancellation ownership. Disconnecting a
GET event observer only detaches it. V2 clients cancel explicitly through
`POST .../cancel`, or delete the session. Use `POST .../turns/:turnID/wait` when you
need a durable completion barrier rather than a `stream_stopped` notification.

An old follow-up accepted while a headless session is idle stays queued until
an explicit run starts it. Unlike canonical `mode: "submit"`, accepting that
legacy follow-up alone does not start a model turn. Old message-array roles
retain upstream semantics: each supplied message is user input, not permission
to inject assistant or system messages.

Legacy interaction matching is intentionally restricted: a response without an
immutable interaction ID cannot safely select among simultaneous prompts. An
optional old `elicitation_id` narrows elicitation matching, but still must identify
exactly one outstanding request. Stale replies fail instead of resolving an
unrelated prompt.

Compatibility is not a promise to emulate removed execution machinery. The
legacy GET event stream treats a missing, invalid, or overflowing cursor as
zero (the `since` query takes precedence over `Last-Event-ID`) and
replays retained history. A replay gap emits a raw `gap` event and closes the
stream without applying its stale tail. Execution POST streams additionally
emit an error with code `observation_gap` on a gap; neither silently succeeds nor waits indefinitely.
Fetch a fresh old
snapshot and reconnect from `last_event_seq`, or migrate to the v2 `gap` and
snapshot barrier. `session_exited` marks session deletion, never normal turn
completion. A premature run-observation EOF emits `observation_ended`; an
observation error emits `observation_failed`. These observation failures do not
cancel accepted work or fabricate successful completion. They differ from an
owning POST request disconnect, which cancels its exact turn as described above.

Legacy agent-switch commands change the **active agent in the same session**
through its owner; they do not change the initial binding or create a fork.
Built-in local handles support command selection and model ordering as part of
atomic input admission. A custom handle missing the required command-admission
capability can return `501`; a custom handle without atomic legacy model
admission returns `422` when a run requests a model override. These unsupported
operations reject before appending input. Unsupported capabilities return
an explicit HTTP error; they
do not start a fallback execution loop. Historical client method names for
`tools`, MCP prompts, `skills`, `compact`, `toolsets`, `pause`, `snapshots`,
`undo`, and `reset` do not establish an unversioned server contract: those
routes were not registered by the upstream server and are not compatibility
aliases. Use the documented v2 capabilities where available.

HTTP compatibility does **not** imply Go SDK source compatibility or automatic
conversion of old database rows into executable sessions. See the
[Go SDK migration notes](../../guides/go-sdk/index.md#http-and-go-sdk-migration)
for these separate boundaries.

### Health

| Method | Path        | Description                               |
| ------ | ----------- | ----------------------------------------- |
| `GET`  | `/health` | Process health check — returns `{"status": "ok"}`. |
| `GET`  | `/ready` | Store readiness and active-session count. |
| `GET`  | `/api/ping` | API health check — returns `{"status": "ok"}`. |
| `GET`  | `/api/ready` | Wait until at least one session is registered; accepts `?timeout=<duration>`. |

### OAuth

| Method | Path                     | Description                                                                                                                                                                                                                                          |
| ------ | ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST` | `/api/mcp-oauth/callback` | Deliver an OAuth deeplink callback to a pending unmanaged OAuth flow. Success path: `?state=<state>&code=<code>`; authorization-server error path: `?state=<state>&error=<error>&error_description=<desc>`. Returns 400 if `state` is missing or neither `code` nor `error` is provided; 404 if no flow is awaiting that `state`. See [Remote MCP OAuth](../remote-mcp/index.md) for details. |

## Workflow summary

1. Discover sources and sessions with `GET /api/v2/sessions`.
2. Create a session-bound session.
3. Attach SSE and establish the snapshot/replay/ready barrier.
4. Submit input through `messages` with the desired mode.
5. Correlate and answer interaction envelopes through `responses`.
6. Reconnect from the last cursor; resnapshot on a gap; surface terminal errors.

## CLI Flags

```bash
docker agent serve api <agent-file>|<agents-dir> [flags]
```

| Flag               | Default          | Description                                      |
| ------------------ | ---------------- | ------------------------------------------------ |
| `-l, --listen`     | `127.0.0.1:8080` | Address to listen on                             |
| `--auth-token`     | (none)           | Bearer token required for all API requests. Leave empty to disable authentication (safe when listening on loopback interfaces only). Recommended when `--listen` binds to a network-reachable interface. |
| `--max-request-size <bytes>` | `1048576` (1 MiB) | Maximum request body size in bytes. Requests whose body exceeds this limit are rejected with HTTP 413 (Request Entity Too Large) — see [Troubleshooting: HTTP 413](../../community/troubleshooting/index.md#http-413-request-body-too-large) if you hit this. |
| `--session-workingdir-root` | (none — unrestricted) | Confine the `working_dir` accepted by `POST /api/v2/sessions` to this directory: after resolving symlinks, the requested directory must be the root or one of its descendants. By default any clean host directory is accepted — the intended behaviour for local single-user daemons that open arbitrary workspaces — but raw values containing `..` are always rejected. Set a root whenever the API serves callers that must not reach arbitrary host paths (multi-user or network-exposed deployments). |
| `-s, --session-db` | `session.db`     | Path to the SQLite session database              |
| `--pull-interval`  | `0` (disabled)   | Auto-pull OCI reference every N minutes          |
| `--fake`           | (none)           | Replay AI responses from cassette file (testing) |
| `--record`         | (none)           | Record AI API interactions to cassette file. Routes through `--models-gateway` when one is configured. |
| `--mcp-oauth-redirect-uri` | (none)   | Public HTTPS URL advertised as the OAuth `redirect_uri` for unmanaged MCP OAuth flows. When set, Docker Agent drives PKCE and code exchange in-process and sends the full authorize URL to the client via elicitation. See [Remote MCP](../remote-mcp/index.md) for details. |

> [!NOTE]
> **What `--max-request-size` does and doesn't cover**
>
> This is a finite, process-wide cap on one serialized inbound HTTP request body — it isn't a model context-window limit, and raising it doesn't increase what a provider/model accepts or how large a local attachment/prompt file can be. A larger cap also means the server buffers more memory per request from an unauthenticated or malicious client, so weigh that against your deployment's exposure. If a reverse proxy or gateway sits in front of this server, it may enforce its own, lower cap regardless of this flag. See [Troubleshooting: HTTP 413](../../community/troubleshooting/index.md#http-413-request-body-too-large) for full diagnosis.

> [!TIP]
> **Live profiling (advanced)**
>
> For production diagnostics, set the `CAGENT_PPROF_ADDR` environment variable (or the hidden `--pprof-addr` flag) to a TCP address such as `127.0.0.1:6060`. Docker Agent will start a Go pprof HTTP server at `/debug/pprof/`, which you can query with `go tool pprof`. Use a loopback address — a non-loopback binding logs a security warning. This flag is intentionally hidden from `--help`.

> [!TIP]
> **Multi-agent configs**
>
> You can point `docker agent serve api` at a directory containing multiple agent YAML files. Each becomes a separate agent accessible via `/api/agents`. Combine with `--pull-interval` to auto-refresh agents from an OCI registry.

## Session Persistence

Sessions are stored in a SQLite database (default: `session.db` in the current directory). This means:

- Sessions survive server restarts
- Multiple server instances can share a database
- Use `--session-db` to specify a custom path

## Tool Call Approval

By default, tool calls require approval. In the API workflow:

1. Agent makes a tool call → server emits a `tool_call_confirmation` event
2. Client reviews and sends `POST /api/v2/sessions/:id/responses` with the envelope `interaction_id`, kind, and decision
3. Execution continues based on approval/denial

Toggle auto-approve with `POST /api/v2/sessions/:id/tools/toggle` for automated workflows.

## Driving a running TUI with `--listen` {#listen}

An interactive run can expose the same canonical session API with
`--listen`. Send input through `/messages` using `submit` or `steer`
mode and observe it through the versioned session SSE stream. Attached and
headless servers use the same snapshot/replay/ready/gap contract described
above; transport closure without a terminal error is not successful turn
completion.

```bash
docker agent run agent.yaml --listen 127.0.0.1:8080
curl -X POST http://127.0.0.1:8080/api/v2/sessions/$SID/messages \
  -H 'Content-Type: application/json' \
  -d '{"mode":"submit","content":"Now add tests"}'
curl -N -H 'Last-Event-ID: 42' \
  "http://127.0.0.1:8080/api/v2/sessions/$SID/events?since_epoch=$EPOCH"
```

The run keeps its interactive TUI; accepted input is executed by the session
owner and observed by both the terminal and connected API clients. Disconnecting
an SSE client cancels only its observation, not the accepted turn.

> [!NOTE]
> **Discovering a run**
>
> Each run started with `--listen` writes a discovery record to `<data-dir>/runs/<pid>.json` containing its address and initial session ID. Tabs opened later are separate sessions on the same control plane; use `GET /api/v2/sessions?active=true` to enumerate them without loading session history.

> [!WARNING]
> **The attached control plane has a fixed 1 MiB request-body cap and no built-in authentication**
>
> Unlike `docker agent serve api`, `--listen` exposes neither `--max-request-size` nor `--auth-token`. Keep it on loopback, a unix socket, or behind an authenticating reverse proxy. Use a standalone API server when you need a configurable cap or built-in bearer authentication.

## Session Forking

`POST /api/v2/sessions/:id/fork` creates a new session whose history is a copy of the parent up to (but **excluding**) a specified user message. This lets a client "branch" a conversation — e.g. rewind to an earlier question and try a different prompt — without losing the shared history that came before.

**Request body:**

```json
{ "user_message_index": 1 }
```

`user_message_index` is a **0-based ordinal** that counts only user-role messages in the parent's flat, user-visible message list. The targeted user message is **excluded** from the fork so clients can prefill it into their chat input for the user to edit and resubmit.

**Example:**

```bash
# Fork a session before the second user message (ordinal 1)
$ curl -X POST http://localhost:8080/api/v2/sessions/$SID/fork \
  -H 'Content-Type: application/json' \
  -d '{"user_message_index": 1}'
# Returns: api.SessionResponse for the new forked session
# New session title: "<parent title> (fork 1)", "(fork 2)", etc.
```

**Validation:**

- Out-of-range ordinals (negative, or at/past the user-message count) return `400 Bad Request`.
- An ordinal that resolves to a user message inside a sub-session returns `400 Bad Request`. A sub-session is a nested session created when a multi-agent config delegates work to a child agent; its messages are embedded within the parent session's message list and cannot be used as a fork boundary.

## Reliable queued messages

A successful `POST /api/v2/sessions/:id/messages` is the acceptance boundary. If a
turn is already running, submit mode durably appends the input to that session's
bounded FIFO mailbox and returns `disposition: "queued"`; clients must not
resubmit merely because execution has not started yet. The response's immutable
`turn_id` identifies the accepted input. Observe that turn through the ordered
SSE journal, reconnecting from the last sequence when transport closes. A `gap`
requires a fresh snapshot before continuing. Capacity, stopped-session, and
persistence failures are returned as errors and are not acceptance.

### Borrowed adapter lifetimes

The daemon's workspace/source runtimes share one process-local `SessionService`.
Embedders can attach `embeddedchat.Config.SessionRuntime` plus `SessionID` to an
existing canonical session; `Close` detaches that embedding without shutting
down its borrowed authority. Elicitation and iteration-limit interactions are
emitted with correlation tokens and answered with `Respond`, or handled by
`Config.InteractionHandler`; they are not silently declined.

A2A `RunOptions.SessionRuntime` and MCP `HTTPOptions.SessionRuntime` (or the
optional registry argument to `CreateToolHandler`) likewise borrow a host-owned
registry. Without one, they retain invocation-owned runtime behavior. Cancelling
an invocation cancels and drains its exact accepted turn, never the borrowed
service. The host remains responsible for the service, backing store, agent
bindings, toolsets, and durable background-work lifetime.

ACP exposes `AttachSession` for host-controlled canonical attachment. ACP
`session/load` resumes canonical ownership and replays user/assistant text from
a detached snapshot; historical tool activities and multimodal blocks are not
reconstructed in this text projection.
Slash commands are not advertised until they have a lifecycle dispatcher.
Filesystem requests enforce ACP workspace roots and configured filesystem
allow/deny rules before delegated client I/O; the client owns the final I/O
boundary and must preserve containment against concurrent symlink changes.

`POST /api/v2/sessions/start` provides recoverable idempotent creation plus initial
submission. It persists an immutable creation/input proof, then submits the stable
input token. If submission fails after creation, retrying the same request resumes
the second step; a different payload or unrelated existing identity conflicts.
The two writes are not a database-atomic transaction.

For clients using the separate creation and submission operations:
Clients requiring recovery after ambiguous creation should supply a stable
`session_id` and inspect that identity after a conflict. Retrying creation
without an ID can create another session. Input retries must reuse the same
`request_id` and payload; a new or omitted token is a distinct input.
