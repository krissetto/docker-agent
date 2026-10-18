# AgentKit wire fixtures

These fixtures use deterministic synthetic identities, content, and timestamps;
they do not come from a live server and contain no credentials. Their JSON was
marshaled in Go with the transport DTOs and runtime/session event types, rather
than assembled as Swift dictionaries. The checked-in generator is
`Fixtures/Generate/main.go`; from the repository root, regenerate with:

```sh
go run ./apple/Tests/AgentKitTests/Fixtures/Generate
```

The generator writes only the named synthetic JSON/SSE fixture files beside
this README; it does not call a server or require credentials.

Source/type provenance:

- `server_identity.json`: `api.ServerInfo` (`pkg/api/server.go`), matching
  server-owned version and capability behavior in `pkg/server/server_info.go`.
- `agents.json`: `[]api.Agent` (`pkg/api/types.go`), matching
  `/api/agents` source projection in `pkg/server/server.go`. `name` is the
  source ID (`workspace-source.yaml`); `root_agent_name` is the configured
  first agent (`planner`), deliberately distinct to guard source/agent identity.
- `catalog.json`: `api.SessionCatalog[runtime.SessionState]`, source, and
  resource types (`pkg/api/session.go`); catalog mapping is in
  `pkg/server/session_catalog_page.go`.
- `advanced_session_contract_examples.json` wraps real Go-marshaled request
  and response DTOs with fixture-only method/path/status metadata. Pause and
  wait examples use `api.SessionStatus`, `runtime.WorkflowSettlement`, and the
  exact response shapes from `pkg/server/session_http.go`; subtree stop,
  message edit, and fork examples use `api.UpdateMessageRequest`,
  `session.Message`, `api.ForkSessionRequest`, and `api.SessionResponse`.
  Delegation-policy and attachment-removal examples follow the handlers in
  `pkg/server/session_delegation.go` and `pkg/server/session_http.go`; source
  registration uses `api.SourceRegistrationRequest/SourceRegistration`.
  `pause_status.json` is the v2 status DTO. `child_catalog_candidates.json`
  demonstrates a summary catalog child candidate with
  `requires_confirmation`; `historical_open_view.json` uses the actual
  `runtime.PreparedSessionViewInfo`, `api.SessionOpened`, metadata and status
  DTOs for read-only historical opening. `execution_available:false` may be
  omitted on wire (`omitempty`); historical identity and capability gate
  execution. Synthetic routes/IDs do not call a
  live server. The method/path/status wrapper exists only in this fixture.
- `session_media_parts.json`: `api.SessionInputRequest` with actual
  `[]chat.MessagePart`/`chat.Document` types; covers inline text and
  Go-`[]byte` base64 inline document data (`pkg/api/session.go`,
  `pkg/chat/chat.go`, `pkg/chat/document.go`). `generated_media_ref.json` is
  `runtime.GeneratedFileRef` (`pkg/runtime/generated_file.go`).
  `generated_media_event.json` is a real `api.SessionEnvelope` carrying
  `runtime.MessageAddedAt`; generated media is metadata-only at the event
  boundary (`pkg/runtime/event.go`, `pkg/runtime/session_generated_file.go`).
- `agent_info.json`: `runtime.SessionAgentInfo` with bound `AgentInfoEvent`,
  `TeamInfoEvent`, `AgentDetails`, and config `types.Commands` fields
  (`pkg/runtime/session_api.go`, `pkg/runtime/event.go`).
- `agent_config.json`: `runtime.SessionAgentConfig` (alias of the API wrapper)
  with the actual `runtime.AgentConfigInfo`, `ToolsetDetail`, and
  credential-safe `teamdef.AgentDefinition` types. All strings are synthetic;
  the instruction is harmless fixture text, not copied source/config
  (`pkg/api/session_agent_config.go`, `pkg/runtime/session_agent_config.go`,
  `pkg/runtime/runtime.go`, `pkg/teamdef/definition.go`).
- `session_permissions.json`: `runtime.SessionPermissionsInfo` with actual
  `runtime.PermissionsInfo` and `session.PermissionsConfig`. `session_tools.json`
  is `runtime.SessionToolsInfo` / `api.SessionToolsetStatus` with actual
  `tools.Tool` descriptors and lifecycle state (`pkg/runtime/session_capabilities.go`,
  `pkg/api/session_capabilities.go`, `pkg/tools/tools.go`).
- Workflow fixtures are built with current wire/runtime payload types:
  `mcp_prompts.json` uses `map[string]tools.PromptInfo`, paired with
  `mcp_prompt_execute_request.json` / `mcp_prompt_execute_response.json`
  (`api.SessionPromptRequest/Result`); `fork_skills.json` uses `[]skills.Skill`
  (including capitalized no-tag properties), and run request/accepted response
  plus `fork_skill_operation_event.json` use the actual skill args and
  `runtime.SkillOperationEvent`.
- `context_breakdown.json` is `runtime.ContextBreakdown` with category/file DTOs.
  `workspace_snapshots.json` and undo/reset request/result files use
  `api.WorkspaceSnapshots`, `WorkspaceSnapshotRequest`, and
  `WorkspaceSnapshotResult`; its deterministic proof is computed with the
  same Go JSON/SHA-256 inputs (`{ID,Infos}` using actual
  `hooks/builtins.SnapshotInfo`) as `runtime.workspaceSnapshotProof`.
- `batch_delete_request.json` / `batch_delete_response.json` and
  `batch_export_request.json` use the API request/response types. The actual
  batch delete response is typed. **Actual current batch export is an ad hoc Go
  JSON map, not `BatchExportSessionsResponse`**; `batch_export_response.json`
  intentionally marshals that source implementation shape (including ignored
  ZIP request vs `export_format:"json"`). `recovery_response.json` similarly
  mirrors the current handler's ad hoc recovery map, including its optional
  recorded-error list. These are small synthetic values, not production dumps.
- `prepare_info.json` and `open_view.json`:
  `runtime.PreparedSessionViewInfo`, `runtime.SessionBinding`,
  `api.SessionOpened[...]`, and `api.SessionMetadata` / status DTOs
  (`pkg/runtime/session_api.go`, `pkg/api/session.go`).
- `snapshot.json`: `api.SessionSnapshot[...]` containing a synthetic
  `session.Session`, with the same field mapping used by
  `pkg/server/session_http.go`.
- `branch_snapshot_proof_cases.json` is generated from actual
  `api.SessionSnapshot` / `session.Session` / `session.Item` values. Each case
  carries the compact raw snapshot JSON or chunked snapshot frames, the exact
  extracted raw `session.messages` member, the independently marshaled
  `session.MessagesSnapshot()` bytes, `runtime.SnapshotProof`, and real
  `api.SessionBranchRequest` examples for valid array snapshots. It covers Go
  HTML escaping, quotes, Unicode/U+2028/U+2029, system messages, summaries,
  embedded sub-sessions, errors/termination, top-level item positions, and
  reconstructed snapshot chunks. It also records allocated-empty `[]` and the
  nil-slice edge case: raw snapshot `messages:null` is marked unavailable for
  positional proof because the actual `MessagesSnapshot()` proof hashes `[]`.
  Clients should fail closed for that null snapshot representation. Do not hash
  the envelope, pretty-printed JSON, or a re-encoded Swift value.
  `tool_toggle_semantics.json` records synthetic real
  `session.Session` state snapshots around `ToggleYolo`, including its
  bodyless `POST /api/v2/sessions/:id/tools/toggle` JSON `null` response shape
  and remembered-prior-policy behavior (`pkg/session/session.go`,
  `pkg/server/server.go`).
- `event_sequence.sse`: synthetic `runtime.Event` values inside
  `api.SessionEnvelope` and `api.SessionStreamMessage`. SSE `event:`/`data:`
  lines and numeric `id:` lines follow `sessionEventStream` in
  `pkg/server/session_http.go`; payloads use `pkg/runtime/event.go` and
  `pkg/runtime/session_turn.go` types.
- `snapshot_chunk_frames.json` and `snapshot_chunked.sse`: marshaled snapshot
  data split into the server's 64 KiB maximum chunks, encoded by the Go JSON
  marshaler (which base64-encodes `[]byte`) and framed as named SSE events.

The existing 16-event `event_sequence.sse` is unchanged. Generated-media event
coverage is in separate `generated_media_event.json`, so the lifecycle transcript
stays compact.

`message_added` deliberately carries only the commit boundary fields: its Go
`Message` field is tagged `json:"-"`. Use the session snapshot/transcript for
message content; the event's `session_position`, `message_role`, and
`tool_call_ids` are reconciliation metadata. The chunked snapshot is large only
because its synthetic title is repeated text, not because it represents a real
conversation.
