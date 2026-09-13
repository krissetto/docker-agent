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

Session execution has one API surface:

| Method | Path | Description |
| --- | --- | --- |
| `GET` / `POST` | `/api/sessions` | Catalog sessions (`?active=true` lists attached sessions without reading stored history) / create a session-bound session (`source`, `agent_name`, optional `model`, `title`, `working_dir`, `safety_policy`, `tools_approved`, `permissions`). |
| `GET` / `DELETE` | `/api/sessions/:id` | Inspect or delete the session. Legacy stored rows remain inspectable but are not attachable. |
| `PATCH` | `/api/sessions/:id` | Canonical edit: `kind` is `policy`, `permissions`, `title`, `message`, `summary`, `tokens`, or `attachment`; returns the updated session. Transcript edits require a quiescent session. |
| `GET` | `/api/sessions/:id/status` | Session state, active turn, pending inputs, and last error. |
| `GET` | `/api/sessions/:id/snapshot` | Canonical transcript/status/interactions snapshot and cursor. |
| `GET` | `/api/sessions/:id/events` | Versioned SSE snapshot, replay, ready barrier, and live ordered envelopes. |
| `POST` | `/api/sessions/:id/messages` | Submit input. `mode` is `submit` (starts a turn, or queues one when a turn is running) or `steer` (urgent in-turn input). |
| `POST` | `/api/sessions/:id/responses` | Answer a confirmation, max-iteration, or elicitation using required `interaction_id` and `kind`. |
| `POST` | `/api/sessions/:id/cancel` | Cancel the active or exactly named active/queued `turn_id` without closing the session. |
| `POST` | `/api/sessions/:id/turns/:turnID/wait` | Wait for this exact turn to settle, including durable completion; `204` on success, typed `404` for unknown/expired turns. Disconnecting cancels only the wait. |
| `POST` | `/api/sessions/:id/retry` | Retry the last failed settled turn. |
| `PATCH` | `/api/sessions/:id/title` | Session-ordered durable title change. |
| `GET` | `/api/sessions/:id/tree` | Authoritative subtree rooted at any session node; metrics are cumulative per node and can be summed for a subtree rollup. |
| `GET` | `/api/sessions/:id/todos` | Current session-keyed todo snapshot. SQLite-backed runtimes persist it; other stores may provide volatile storage. `shared: true` toolsets use the stable root-session ID so agents in one tree share a list without cross-root leakage. |
| `POST` | `/api/sessions/:id/compact` | Compact the session at its execution boundary. |
| `POST` | `/api/sessions/:id/compact/:target` | Compact a live target in the same session tree. |
| `GET` | `/api/sessions/:id/context` | Inspect context-window composition. |
| `GET` | `/api/sessions/:id/live-sessions` | List live sessions visible from this session. |
| `GET` / `POST` | `/api/sessions/:id/skills` / `/api/sessions/:id/skills/run` | List skills or start a fork skill. |
| `GET` / `POST` | `/api/sessions/:id/models` / `/api/sessions/:id/models/refresh` | List or refresh available models. |
| `PATCH` | `/api/sessions/:id/model` | Change the session's pinned model. |
| `GET` / `PATCH` | `/api/sessions/:id/thinking-level` | Read or set the thinking level. |
| `POST` | `/api/sessions/:id/thinking-level/cycle` | Cycle to the next thinking level. |
| `POST` | `/api/sessions/:id/pause` | Toggle iteration-boundary pause. |
| `POST` | `/api/sessions/:id/switch-agent` | Branch into a new session bound to another agent. |
| `PATCH` | `/api/sessions/:id/starred` | Set starred state. |
| `DELETE` | `/api/sessions/:id/attachments` | Remove an attachment. |

Legacy-compatible session routes remain available for existing clients:

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/api/sessions/:id/tools/toggle` | Toggle tool auto-approval. |
| `PATCH` | `/api/sessions/:id/safety-policy` | Update safety policy. |
| `PATCH` | `/api/sessions/:id/permissions` | Update permissions. |
| `PATCH` | `/api/sessions/:id/tokens` | Update persisted token/cost totals. |
| `POST` | `/api/sessions/:id/fork` | Fork before a user message. |
| `PATCH` | `/api/sessions/:id/messages/:msg_id` | Update a persisted message. |
| `POST` | `/api/sessions/:id/summaries` | Add a persisted summary. |
| `GET` | `/api/sessions/:id/recovery` | Read recovery data. |
| `POST` | `/api/sessions/batch/delete` | Delete sessions in a batch. |
| `POST` | `/api/sessions/batch/export` | Export sessions in a batch. |

Todo state is mutated through the configured todo tools; the API endpoint is a
read-only projection and is not a direct todo mutation surface. SQLite persistence
survives restart. Memory/custom backends retain their own durability guarantees.

A session created with `working_dir` runs on a runtime whose toolsets operate
in that directory (the server builds one runtime per source and working
directory, and rebuilds for new sessions when `--pull-interval` refreshes the
source). Without `safety_policy` or `tools_approved` the session starts in the
agent's author-declared safety mode, exactly like a fresh local session.

There are no separate resume, elicitation, steer, follow-up, execution-stream,
or direct transcript-mutation routes. All execution is session-owned and all
interactive responses are correlated by the envelope's immutable
`interaction_id`.

### Sending and observing a turn

```bash
SID=$(curl -s -X POST http://localhost:8080/api/sessions \
  -H 'Content-Type: application/json' \
  -d '{"agent_name":"root"}' | jq -r .session_id)

curl -N http://localhost:8080/api/sessions/$SID/events &
curl -X POST http://localhost:8080/api/sessions/$SID/messages \
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

For `PATCH /api/sessions/:id`, send a `kind` and its matching payload:
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
This chunk extension and `request_id` rename are breaking wire changes.

Live event envelopes carry monotonically increasing sequence IDs,
`turn_id`, and (for interactions) `interaction_id`. `interaction_resolved`
removes the matching pending interaction and carries reason `responded`,
`canceled`, or `stopped`, with the same session/interaction envelope identity.
`stream_stopped` is not transport EOF or a durable-completion barrier; use the
turn wait endpoint when durable settlement is required.

Reconnect with either `?since=<last sequence>` or `Last-Event-ID`. A retained
cursor is replayed before `ready`. A `gap` envelope is a hard resnapshot
barrier. When `DELETE /api/sessions/:id` closes an attached stream, the stream
emits a terminal `stream_stopped` event with reason `deleted` before transport
EOF. Clients should consume that terminal event rather than treating EOF alone
as successful deletion.

Use `/api/sessions/:id/events?tree=true` for a rooted multiplexed stream. It
rejects cursors, emits a fresh snapshot for every current node, then events
with per-session sequences, and observes descendants added later. It emits no
SSE IDs. Reconnect without a cursor for fresh snapshots and events from then.
Observer disconnect never stops sessions. `POST /api/sessions` accepts optional
`parent_session_id`; such children begin idle. Tree nodes expose
`needs_attention` and `waiting_on` (`failed`, `approve tool`, or `answer
question`, in that precedence). `/children` is removed.

To answer an interaction:

```bash
curl -X POST http://localhost:8080/api/sessions/$SID/responses \
  -H 'Content-Type: application/json' \
  -d '{"interaction_id":"<from envelope>","kind":"confirmation","confirmation":"approve"}'
```

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

1. Discover sources and sessions with `GET /api/sessions`.
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
| `--session-workingdir-root` | (none — unrestricted) | Confine the `working_dir` accepted by `POST /api/sessions` to this directory: after resolving symlinks, the requested directory must be the root or one of its descendants. By default any clean host directory is accepted — the intended behaviour for local single-user daemons that open arbitrary workspaces — but raw values containing `..` are always rejected. Set a root whenever the API serves callers that must not reach arbitrary host paths (multi-user or network-exposed deployments). |
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
2. Client reviews and sends `POST /api/sessions/:id/responses` with the envelope `interaction_id`, kind, and decision
3. Execution continues based on approval/denial

Toggle auto-approve with `POST /api/sessions/:id/tools/toggle` for automated workflows.

## Driving a running TUI with `--listen` {#listen}

An interactive run can expose the same canonical session API with
`--listen`. Send input through `/messages` using `submit` or `steer`
mode and observe it through the versioned session SSE stream. Attached and
headless servers use the same snapshot/replay/ready/gap contract described
above; transport closure without a terminal error is not successful turn
completion.

```bash
docker agent run agent.yaml --listen 127.0.0.1:8080
curl -X POST http://127.0.0.1:8080/api/sessions/$SID/messages \
  -H 'Content-Type: application/json' \
  -d '{"mode":"submit","content":"Now add tests"}'
curl -N -H 'Last-Event-ID: 42' \
  http://127.0.0.1:8080/api/sessions/$SID/events
```

The run keeps its interactive TUI; accepted input is executed by the session
owner and observed by both the terminal and connected API clients. Disconnecting
an SSE client cancels only its observation, not the accepted turn.

> [!NOTE]
> **Discovering a run**
>
> Each run started with `--listen` writes a discovery record to `<data-dir>/runs/<pid>.json` containing its address and initial session ID. Tabs opened later are separate sessions on the same control plane; use `GET /api/sessions?active=true` to enumerate them without loading session history.

> [!WARNING]
> **The attached control plane has a fixed 1 MiB request-body cap and no built-in authentication**
>
> Unlike `docker agent serve api`, `--listen` exposes neither `--max-request-size` nor `--auth-token`. Keep it on loopback, a unix socket, or behind an authenticating reverse proxy. Use a standalone API server when you need a configurable cap or built-in bearer authentication.

## Session Forking

`POST /api/sessions/:id/fork` creates a new session whose history is a copy of the parent up to (but **excluding**) a specified user message. This lets a client "branch" a conversation — e.g. rewind to an earlier question and try a different prompt — without losing the shared history that came before.

**Request body:**

```json
{ "user_message_index": 1 }
```

`user_message_index` is a **0-based ordinal** that counts only user-role messages in the parent's flat, user-visible message list. The targeted user message is **excluded** from the fork so clients can prefill it into their chat input for the user to edit and resubmit.

**Example:**

```bash
# Fork a session before the second user message (ordinal 1)
$ curl -X POST http://localhost:8080/api/sessions/$SID/fork \
  -H 'Content-Type: application/json' \
  -d '{"user_message_index": 1}'
# Returns: api.SessionResponse for the new forked session
# New session title: "<parent title> (fork 1)", "(fork 2)", etc.
```

**Validation:**

- Out-of-range ordinals (negative, or at/past the user-message count) return `400 Bad Request`.
- An ordinal that resolves to a user message inside a sub-session returns `400 Bad Request`. A sub-session is a nested session created when a multi-agent config delegates work to a child agent; its messages are embedded within the parent session's message list and cannot be used as a fork boundary.

## Reliable queued messages

A successful `POST /api/sessions/:id/messages` is the acceptance boundary. If a
turn is already running, submit mode durably appends the input to that session's
bounded FIFO mailbox and returns `disposition: "queued"`; clients must not
resubmit merely because execution has not started yet. The response's immutable
`turn_id` identifies the accepted input. Observe that turn through the ordered
SSE journal, reconnecting from the last sequence when transport closes. A `gap`
requires a fresh snapshot before continuing. Capacity, stopped-session, and
persistence failures are returned as errors and are not acceptance.
