# Persistent API-backed TUI for the sandbox kit

> Historical plan: the managed-API Kit launcher and subsequent remote-session/restore fixes have since been committed. The inspection and gap list below describe the earlier checkpoint, not a current unfinished-work list.

## Goal

Make the kit launch a foreground TUI attached to a background API server. Preserve the current interactive UX and launcher arguments, while sessions and accepted work survive TUI exit and remain accessible to other clients.

Use the existing HTTP session-v2 API and TUI. ConnectRPC is out of scope.

## Current state

API support has not been removed: `docker-agent serve api` and `run --remote` exist. The missing work is remote-TUI parity and kit lifecycle integration, not a new API or TUI.

- `kit/launch.sh` currently execs a local `run`, selecting an explicit `--team`, workspace `hackerspace.yaml`, or bundled team.
- The v2 API separates accepted execution from observation. Remote TUI exit/disconnect detaches observers; cancellation is a separate operation.
- Sessions have snapshots, event cursors/replay, catalog/history, approvals, and subagent views.
- The kit requests optional `long-running@1`; a compatible fresh local SBX grants detached sandbox lifetime. This is not API process supervision or uninterrupted recovery after sandbox/server restart.

Findings are from read-only inspection of a concurrently modified working tree. Recheck the gaps against the implementation when picking this up.

## Remaining work

### 1. Complete remote session attachment

In `cmd/root/run.go` and `cmd/root/backend.go`, allow remote session selection/resume rather than always creating a new session. Preserve `--session ID`, `--session=-1`, source/team identity, startup model overrides, and approval settings.

Map working directories to the server's sandbox workspace explicitly. Do not blindly forward an arbitrary remote client's local path. Add authenticated client configuration and preserve headless `--exec` behavior.

### 2. Make observations long-lived and reconnectable

Inspect `pkg/runtime/client.go`, `pkg/runtime/remote_session.go`, and `pkg/runtime/client/observation.go`. The inspected client has a 30-second default HTTP timeout; the observation helper has four total attempts. Use streaming-appropriate timeout policy and bounded backoff that can recover after healthy connections.

Preserve snapshot/replay/live ordering, cursors, gap recovery, outstanding interactions, and subagent-tree observation. Disconnect must not cancel execution. Do not retry mutations automatically without an idempotency guarantee.

### 3. Close the TUI parity gaps

Use the existing `SessionRuntime` / `SessionHandle` seams in `pkg/runtime/session_api.go`; keep one server-owned runtime/store.

- Add remote session spawning for new tabs and saved-tab restoration.
- Move fork/edit-branch operations off the local-only `SessionStore` dependency.
- Supply required remote tool, permission, MCP, skill, and todo capabilities/metadata. Audit current support rather than assuming every operation is missing.
- Preserve history reopening, approvals, model/thinking controls, session/subagent views, and user settings.

Relevant starting points: `pkg/runtime/remote_runtime.go`, `pkg/tui/handlers.go`, hosted-session/spawner code, and `pkg/server/session_http.go`.

### 4. Integrate the kit lifecycle

Update `kit/launch.sh` and, where appropriate, the descriptor's lifecycle hooks to ensure one independently running API server per sandbox/workspace and launch the TUI as the foreground client.

- Start/reuse safely under concurrent launches; check readiness and server identity/configuration before attaching.
- Detach the server's process group and redirect all stdio away from the terminal. Keep logs and explicit database state under the agent's private sandbox-local state directory, not the mounted workspace. `serve api` currently defaults to relative `session.db`, unlike local `run`.
- Do not stop the server when the TUI exits. Handle stale process state and sandbox restart; distinguish saved-state recovery from uninterrupted execution.
- Preserve explicit/workspace/bundled team selection, model arguments, prompts, headless execution, and the descriptor's session-list/resume contract. Decide how incompatible team/model startup arguments are handled when a server already exists; do not silently ignore them or kill active work.
- Keep current telemetry/auto-update settings and host credential/context/skills integrations.

Update `kit/async-agent.yaml`, launcher tests in `tests/kit/`, and `docs/kit.md` as needed. Do not expand permissions implicitly.

### 5. Expose access safely

Default to guest loopback plus an explicit tunnel. Direct SBX port publication targets the sandbox network address, so it needs an authenticated non-loopback guest listener; keep the host binding loopback unless deliberately configured otherwise.

The current API bearer token grants API-wide access, not per-user/session isolation. Treat clients as trusted peers with tool-execution authority. Keep tokens out of transcripts/logs; use TLS or a trusted tunnel beyond loopback. Set browser CORS only for explicitly approved origins.

## Acceptance checks

Use deterministic fake providers before live sandbox tests.

- Default kit launch looks and behaves like today's TUI; existing launcher/session arguments still work.
- Start work, quit or kill the TUI, and observe completion through a second client. Reattach to the same session without duplicate execution.
- Observe beyond 30 seconds; interrupt/reconnect the network and verify replay, approvals, and subagent views without lost or duplicate events.
- Exercise new/restored tabs, history, fork/edit-branch, model overrides, and relevant tool/MCP/skill/todo controls.
- Concurrent launches reuse the intended server safely; incompatible configuration produces a clear result. Unauthorized clients are rejected.
- Stop/restart the sandbox/server and verify the documented recovery boundary and sandbox-local state retention.
- Run focused CLI/client/server/TUI/kit checks, then the project build, test, and lint commands. Report unrelated working-tree/toolchain failures separately.

## Scope and handoff

Keep HTTP/SSE clients compatible; avoid a TUI rewrite or a parallel execution authority. No API/launcher implementation was performed for this investigation. This document is the implementation handoff.
