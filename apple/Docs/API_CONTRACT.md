# Docker Agent API v2 contract for the native Apple client

This is a read-only handoff from current source inspection, not a promise that
all optional capabilities are available from every server/session. “API fact”
records current server behavior. “Client policy” is recommended app behavior.

## Connect, identify, create, and open

- **API fact — auth:** For `serve api` with auth enabled, send
  `Authorization: Bearer <token>` on every API request, including SSE. The
  native stream must support arbitrary request headers; browser `EventSource`
  is not suitable. `/health` and `/ready` are top-level unauthenticated probes;
  `OPTIONS` is also exempt. See `pkg/server/server.go`.
- **API fact — process identity:** `GET /api/v2/server` returns
  `{version:1, session_api_version:2, instance_id, ready, capabilities, ...}`;
  optional identity fields are `workspace_root`, `source`, and
  `config_fingerprint`. Capabilities currently include `session_v2`,
  `session_tools`, `session_permissions`, `mcp_prompts`, `todo_editing`,
  `session_branching`, and `session_explicit_ids`. `ready` uses the bounded
  `session.StorePinger` probe; it is false for stores without that interface.
  It does not mean every session operation is supported.
- **API fact — probes:** `GET /health` means process alive; `/ready` reports
  store readiness/active-session count; `/api/ping` is an API health response;
  `/api/ready?timeout=<duration>` waits for at least one session to register.
  Use `/api/v2/server` for authenticated identity/readiness. See
  `pkg/server/server.go`, `pkg/server/server_info.go`, `pkg/api/server.go`.
- **API fact — list:** `GET /api/v2/sessions` returns a versioned catalog with
  `sessions`, `sources`, and opaque `next_cursor`. Default page size is 50,
  max 200. `view=summary` is metadata-only; `active=true` lists attached
  sessions without reading stored history and cannot combine with summary
  view. Keep `view`, `include_children`, and `active` constant across pages.
  A child marked `requires_confirmation` is a candidate, not access permission.
- **API fact — create:** `POST /api/v2/sessions` accepts
  `session_id?`, `source?`, `agent_name`, `model?`, `title?`,
  `parent_session_id?`, `working_dir?`, `safety_policy?`,
  `tools_approved?`, and `permissions?`; it returns session metadata with
  `session_id`. `working_dir` is server-side and may be confined by
  `--session-workingdir-root`. Omitted `working_dir` deliberately creates a
  workspace-less session. A supplied value rejects any raw `..`, must name an
  existing directory, and is confined to the opt-in root after symlink
  resolution; without that option, trusted local servers intentionally accept
  arbitrary host directories. See `pkg/server/session_manager.go`.
- **API fact — source registration:** `POST /api/v2/sources` accepts
  `{"path":"workspace-relative.yaml","working_dir":"…"?}` and returns
  `{"source_id":"…","revision":"…"}`. The registered path is validated
  by the configured SourceRegistrar; servers with no registrar return 404.
  The managed Apple server resolves an accessible workspace-relative YAML
  path into a stable source identity and immutable revision. This endpoint
  does not upload source files. See `pkg/api/source_registration.go`,
  `pkg/server/source_registration.go`, and `cmd/root/managed_source.go`.
- **API fact — initial work with recoverable create:**
  `POST /api/v2/sessions/start` accepts the creation fields plus
  `input:{request_id,content,multi_content?,mode?}`. Root submit requires a
  stable `session_id`, nonempty `agent_name`, and `input.request_id`; only
  submit mode is valid. Retry the *identical* request. Creation proof and
  input admission are separate writes, not one database transaction.
- **API fact — existing session open:**
  `GET /api/v2/sessions/:id?view=prepare-info` reads confirmed identity
  without publishing execution. Commit with
  `PATCH /api/v2/sessions/:id?view=open-view` and
  `{"kind":"open_view","view_confirmation":<confirmation from prepare-info>}`.
  A successful response is versioned `SessionOpened` (`view:"open-view"`,
  `info`, `metadata`, `status`). Open is not a model turn. Cold child candidates
  require confirmed ancestry/access validation.
- **Client policy:** verify v1 identity/session API v2 before using canonical
  operations; distinguish a failed server probe from unsupported operation.
  On attach, use prepare/confirm/open instead of assuming a catalog row grants
  access. Server and per-session capabilities are separate; inspect
  `metadata.capabilities` before showing optional controls.

- **API fact — agent discovery identity:** `GET /api/agents` returns an array
  of `{name,root_agent_name,description,multi,commands?}`. `name` is the
  source key/ID used to create a session, not an agent name;
  `root_agent_name` is the first configured agent (e.g. `planner`).
  `GET /api/agents/:id` retrieves that source config. This discovery route is
  outside `/v2`. See `pkg/api/types.go` and `pkg/server/server.go`. The
  `agents.json` fixture intentionally separates `workspace-source.yaml` from
  `planner`.

## Send, observe, reconnect, interactions

- **API fact — submit:** `POST /api/v2/sessions/:id/messages` accepts
  `mode:"submit"|"steer"`, `content`, `multi_content?`, `request_id?`, and
  `generate_title?`. Empty mode means submit. Submit starts work or queues it;
  steer is in-turn guidance. Accepted response is 202 with `session_id`,
  `turn_id`, and optional `disposition:"queued"`. It has no target-agent field:
  input goes to the session owner’s active agent. `GET /:id/status` reports
  `metadata.agent_name`; `GET /:id/agent-info` returns richer current agent
  (`agent.agent_name`) and team (`team.current_agent`) presentation.
  `POST /:id/switch-agent` returns a new session; it is not a send-to-agent
  parameter on the original session.
- **API fact — idempotency:** `request_id` is the stable submission identity.
  Matching ID and exact payload/mode in a session returns the original
  submission; reusing it for different input conflicts. Omission creates an
  independent submission. Retry ambiguous `/start` only with the identical
  full request including `session_id` and input ID. A 503 persistence error is
  not acceptance. Old `client_id` is not a deduplication guarantee.
- **API fact — separate cancellation/settlement:** Detaching `GET …/events`
  does not cancel accepted work. Cancel explicitly via
  `POST /api/v2/sessions/:id/cancel` (optionally `turn_id`); use
  `POST /:id/turns/:turnID/wait` for durable exact-turn settlement. `POST
  /:id/workflow/wait` waits through descendants/followups. `stream_stopped` is
  not a durable-completion barrier; transport EOF alone is not success.
- **API fact — pause, subtree stop, and waits:** `POST /:id/pause` is bodyless
  and toggles the arm; `{"paused":true}` means a pause is armed, not that
  running work has reached its safe boundary. `runtime_pause_changed` reports
  the toggle; `runtime_paused` reports the boundary, and a second POST resumes.
  Status exposes `pause_armed`, `paused`, and `pause_generation`. Bodyless
  `POST /:id/stop-subtree` returns 204; on the root it fences/stops the whole
  tree and withdraws pending root inputs, while on a child it stops only that
  child subtree. This is not `cancel`, which cancels a turn. Bodyless
  `POST /:id/turns/:turnID/wait` returns 204 for that exact known turn;
  settled includes completed, failed, or canceled outcomes, while unknown IDs
  are not-found and a turn without durable terminal evidence is interrupted.
  `POST /:id/workflow/wait` returns `{"epoch":"…","cursor":N}` only after
  root-tree quiescence (descendants, report outboxes, pending inputs, and skill
  operations). Both waits end with their request context; disconnecting a wait
  does not cancel the turn/tree. There is no server-side `timeout` query
  parameter contract: impose client timeouts by canceling the HTTP request.
  See `pkg/server/session_http.go`,
  `pkg/runtime/session_driver.go`, `pkg/runtime/session_turn.go`,
  `pkg/runtime/session_workflow.go`, and `pkg/runtime/session_service_stop.go`.
- **API fact — canonical SSE:** `GET /api/v2/sessions/:id/events` emits a
  version-2 JSON `snapshot`, zero or more `event` frames containing an
  envelope, then `ready`. Envelope fields include `session_id`, `epoch`,
  `sequence`, `transcript_position`, `event`, optional `turn_id` and
  `interaction_id`, and gap metadata. Sequenced event frames carry numeric
  SSE `id` equal to `sequence`; snapshot/ready and sequence-zero live seeds do
  not. Server idle heartbeat is an SSE comment every 15s.
- **API fact — runtime event payload map:** `event` is the marshaled concrete
  `pkg/runtime.Event` struct, not a normalized transcript item. `agent_choice`
  and `agent_choice_reasoning` carry their text in `content`; partial and
  complete tool calls carry `tool_call` (and usually `tool_definition`).
  `tool_call_confirmation` includes the tool and request ID; the server copies
  that request ID to envelope `interaction_id`. `interaction_resolved` carries
  `interaction_id` and `reason` (`responded|canceled|stopped`). Tool execution
  is split across `tool_call_output` (`tool_call_id`, `output`) and
  `tool_call_response` (`tool_call_id`, `response`, optional `result`).
  `token_usage` nests counters/cost in `usage`; `subagent_tree` nests its
  snapshot; `turn_settled` carries `session_id`, `turn_id`, and durable outcome.
  `message_added` is a commit boundary (`session_position`, optional
  `message_role`, `tool_call_ids`, generated media), **not message content**:
  its `Message` field is explicitly tagged `json:"-"`. Read committed content
  from the snapshot transcript and reconcile by position/identity, not event
  text. See `pkg/runtime/event.go`, `pkg/runtime/session_turn.go`, and the
  Go-marshaled examples in `apple/Tests/AgentKitTests/Fixtures/event_sequence.sse`.
- **API fact — frame baseline:** non-tree streams write `snapshot` (or a
  chunked snapshot), replay frames if any, then `ready`; live events follow.
  Event frames are `event: event`, while snapshot/ready use their message type
  as the SSE event name. Only sequenced envelopes have numeric SSE IDs. This
  framing is implemented in `pkg/server/session_http.go`.
- **API fact — large snapshots:** up to 64 KiB is one `snapshot`. Larger
  snapshots are contiguous `snapshot_begin`, `snapshot_chunk`, `snapshot_end`
  messages with one `cursor`; `chunk` is base64 of at most 64 KiB of snapshot
  JSON bytes. Concatenate decoded bytes. Reject wrong cursor/boundaries,
  oversized/empty chunks, missing end, or a reconstructed snapshot whose
  cursor differs. History is not truncated. See `pkg/api/session.go` and
  `pkg/server/session_http.go`.
- **API fact — replay:** reconnect with
  `?since=<last sequence>&since_epoch=<epoch>` or numeric
  `Last-Event-ID` plus `since_epoch`. Query `since` wins over the header;
  `since_epoch` without a cursor is invalid. Sequence order is per epoch. A
  matching retained cursor replays before `ready`; absent/mismatched epoch
  yields a fresh snapshot with outstanding interactions/live seeds, not old
  business-event replay. `gap` is a hard resnapshot barrier. Default journal
  retention is 1,024 events/8 MiB; slow observers can also gap. Replace local
  projection when the snapshot epoch changes. `tree=true` is a fresh
  multiplexed snapshot/live stream for all descendants, rejects cursors and
  emits no SSE IDs.
- **Client policy:** retain `(epoch,sequence)`, not numeric ID alone; dedupe
  transcript/event projection by server identities/positions, never by message
  text. On epoch change or gap, replace the projection and pending interactions
  from the new snapshot. Reconnect observation only; never re-submit accepted
  input because SSE ended.
- **API fact — interactions:** snapshot `interactions` and event envelopes
  carry immutable `interaction_id`; interaction kinds are `confirmation`,
  `max_iterations`, `elicitation`. Resolve with
  `POST /api/v2/sessions/:id/responses`, required `interaction_id` and `kind`.
  Confirmation/max iterations require `confirmation`: `approve`,
  `approve-balanced`, `approve-autonomous`, `approve-tool` (also `tool_name`),
  or `reject` (optional `reason`). Elicitation requires `action` (`accept`,
  `decline`, `cancel`), optional `elicitation_id` and `content`. Elicitation
  answer must match both interaction identity and event elicitation ID.
  `interaction_resolved` carries same interaction ID and reason
  `responded|canceled|stopped`. Stale/double answers fail; don’t auto-decline.

## Native-client expansion contracts

### Submit attachments and generated media (priority)

- **API fact — submit payload:** `POST /api/v2/sessions/:id/messages` accepts
  `content` plus optional `multi_content` (`[]chat.MessagePart`); server
  requires at least one of nonblank `content` or a nonempty `multi_content`.
  The request goes through as parts and is admitted as a user turn; there is
  no separate upload endpoint and the HTTP server does not read client file
  paths or call `chat.ProcessAttachment` for you. New attachment shape is
  `{type:"document",document:{name,mime_type,size?,source:{inline_text? |
  inline_data?}}}`; `source.inline_data` is Go `[]byte`, hence standard
  base64 in JSON, while `inline_text` remains text. `image_url` and `file` are
  legacy part shapes. The legacy image URL attachment parser accepts only
  `data:<mime>[;charset=...];base64,<payload>` (base64 required); remote URLs
  are rejected by `chat.ProcessAttachment` for attachment processing, so a
  client should not send an arbitrary local path or assume a URL is fetched.
  See `pkg/api/session.go`, `pkg/chat/chat.go`, and `pkg/chat/attach.go`.
- **Limits / client implication:** server request bodies default to 1 MiB and
  are configurable with `--max-request-size` (`pkg/server/server.go`,
  `cmd/root/api.go`); inline base64 expands binary bytes. The local attachment
  processor caps text at 5 MiB, binary at 20 MiB, and normalizes images to
  at most 2,000 px / 4,500,000 bytes (`pkg/chat/chat.go`, `attach.go`,
  `image.go`), but that processor is not automatically invoked on v2 submit.
  Thus the effective v2 HTTP cap is usually lower and must be treated as
  authoritative for request transport. No upload/presigned/multipart route or
  resumable large-input facility is present.
- **API fact — generated media round trip:** model-generated media is
  materialized to workspace and represented in assistant message history as
  document metadata (`artifact_owner_session_id`, `artifact_root:"workspace"`,
  relative `artifact_path`); it is not embedded as binary transcript data.
  `message_added.generated_media` is a portable reference list, never bytes;
  `message_added.message` remains omitted. Resolve a visible reference with
  `POST /api/v2/sessions/:id/generated-media`, body
  `{"owner_session_id":"...","root":"workspace","path":"relative/path"}`.
  Success is raw bytes (`application/octet-stream`), `Cache-Control: no-store`,
  max 32 MiB; every failure is intentionally collapsed to unavailable/404.
  Resolver verifies message visibility plus the owning session/path manifest,
  safe workspace-relative path and regular file/blob; do not resolve a path
  directly, assume current viewer owns it, follow redirects, or persist the
  result under a server filesystem path. Branches keep owner identity for
  inherited artifacts. See `pkg/runtime/event.go`,
  `pkg/runtime/session_generated_file.go`, `pkg/runtime/generated_file.go`,
  and `pkg/server/session_generated_media.go`.
- **Current Apple client behavior / remaining media scope:** `AgentKit`
  constructs typed inline-document `multi_content` from security-scoped Files,
  Photos, and file URLs dropped on the composer; validates name/MIME/source/size;
  caps a document at 512 KiB, a message at 16 document parts and the encoded
  request at 768 KiB. Photo input is re-encoded/downsampled as JPEG. There is
  no evidence of a clipboard paste-to-attachment path. Generated-media refs
  are validated and fetched through authenticated owner-bound `requestBytes`,
  then shown as an image thumbnail or file tile with Quick Look/Share. The app
  does not run CLI-local `chat.ProcessAttachment` preprocessing for generic
  selected files, so images retain picked bytes/metadata and text files remain
  binary attachments; provider/model conversion may drop unsupported MIME or
  capability combinations. There is no upload/presigned/resumable large-file
  transfer. The app's encoded request cap is below the default server 1 MiB
  HTTP body cap. Source:
  `apple/Sources/AgentKit/SessionMedia.swift`,
  `apple/Sources/DockerAgentApp/Views/Session/Attachments/AttachmentViews.swift`,
  and `pkg/chat/attach.go` / `pkg/model/provider/*/attachments.go`.

### Agent inspector versus edits

- **API fact — read-only config inspector:**
  `GET /api/v2/sessions/:id/agent-config?agent=<agent-name>` requires `agent`.
  Outer response is `session_id`, `source`, `agent_name`, `info`, and optional
  historical identity/definition fields (`historical`, `execution_available`,
  `history_reason`, `agent_id`, `agent_key`, `definition_known`, `definition`).
  `info` is `runtime.AgentConfigInfo`; its Go fields currently serialize with
  capitalized names (`SubAgents`, `Handoffs`, `Fallbacks`, `Skills`,
  `MaxIterations`, `NumHistoryItems`, `MaxConsecutiveToolCalls`, `Options`,
  `Toolsets`, `IsCurrent`), and nested `ToolsetDetail` uses `Name`, `Kind`,
  `State`, `Tools`. It is static/live inspection data, not a config edit
  schema; local inspection does not start toolsets. Historical `definition`,
  when recorded, is a credential-safe authored projection, but its instruction
  text can itself contain literal secrets. Resolved environment/provider
  credentials are excluded. Remote session runtimes return zero-valued
  config-derived info. See `pkg/api/session_agent_config.go`,
  `pkg/runtime/session_agent_config.go`, and `pkg/runtime/runtime.go`.
- **API fact — bound agent presentation:** `GET /api/v2/sessions/:id/agent-info`
  returns `{agent?,team?,commands?}`. `agent` presents the bound current agent
  (`agent_name`, `model`, `description`, optional `welcome_message` and context
  limits); `team` includes `available_agents` plus `current_agent`. Each team
  entry includes name/description and optional provider/model, model catalog,
  thinking mode/level/levels and command metadata. This is presentation, not
  the config inspector or a config mutation route. See
  `runtime.SessionAgentInfo` in `pkg/runtime/session_capabilities.go` and
  event types in `pkg/runtime/event.go`.
- **API fact — model and reasoning are separate session controls:**
  `GET /api/v2/sessions/:id/models` gives model choices (`name`, `ref`,
  optional provider/model metadata and default/current flags);
  `PATCH /api/v2/sessions/:id/model` body
  `{"model":"ref"}` selects a supported override.
  `GET /api/v2/sessions/:id/thinking-level` yields
  `{levels,current,metadata}`; `PATCH /api/v2/sessions/:id/thinking-level` body
  `{"level":"..."}` sets a level and
  `POST /api/v2/sessions/:id/thinking-level/cycle` cycles it. These are capability-gated session controls, not mutations to
  an agent's authored source config.
- **API fact — permissions and tool controls:**
  `PATCH /api/v2/sessions/:id/safety-policy` body
  `{"safety_policy":"strict|balanced|restricted|autonomous"}`; empty resets
  legacy-default mode and legacy aliases are normalized. This also synchronizes
  `tools_approved` (`autonomous` is true);
  `POST /api/v2/sessions/:id/tools/toggle` toggles blanket approval. `PATCH /api/v2/sessions/:id/permissions` body
  `{"permissions":{"allow":[...],"ask":[...],"deny":[...]}}` replaces
  session rules. `GET /api/v2/sessions/:id/permissions` returns effective
  session policy and
  source/session rule sets (`policy`, `tools_approved`, `source`, `session`;
  inner source rules are capitalized Go fields `Allow|Ask|Deny`).
  `GET /api/v2/sessions/:id/tools` returns `{tools,statuses}` descriptors; descriptors are inspection-only and
  contain no callable authority.
  `POST /api/v2/sessions/:id/toolsets/restart` takes
  `{"name":"toolset"}` and requires a restartable toolset. Permission
  precedence/mode is runtime behavior; controls are not an authored config
  editor. See `pkg/server/server.go`, `pkg/server/session_capabilities.go`,
  `pkg/api/types.go`, `pkg/api/session_capabilities.go`,
  `pkg/runtime/session_capabilities.go`, and `pkg/session/session.go`.
- **Current Apple client / true config boundary:** the app has read-only typed
  agent detail/roster, a safety-policy editor, and per-session allow/ask/deny
  rule add/remove UI. It displays source permission rules read-only; its
  read-then-replace session-rule update is not atomic CAS. Model/thinking-level
  controls are session-bound overrides, not edits to authored YAML. Source
  registration, create-time server workspace selection, per-tree delegation
  policy, and attached-file removal are user-facing flows. There is still no
  registered canonical agent/team YAML mutation route; team-editing tools
  exposed to an agent are execution features, not client CRUD. See
  `apple/Sources/DockerAgentApp/AppModelInspector.swift`,
  `apple/Sources/DockerAgentApp/Views/Sessions/RegisterSourceSheet.swift`,
  `apple/Sources/DockerAgentApp/Views/Sessions/NewSessionSheet.swift`, and
  `apple/Sources/DockerAgentApp/Views/Session/Inspector/SafetyPanel.swift`.
- **API fact — legacy blanket-approval toggle:** `POST
  /api/v2/sessions/:id/tools/toggle` takes no body and returns JSON `null`.
  It toggles the session to Autonomous (blanket approve) and back to the prior
  explicitly selected Balanced/Strict mode. An explicit safety-policy change
  while Autonomous clears that remembered prior mode; a later toggle-off then
  returns to the legacy unspecified default. The Apple safety inspector now has
  “Toggle Autonomy” rather than emulating this with a policy write: both
  directions require confirmation, and enabling warns that any tool may run
  without asking. Before toggling it compares the currently displayed
  `tools_approved` boolean with a freshly loaded value and conflicts if that
  boolean changed; this check is not CAS and can miss a concurrent
  Balanced-to-Strict change while both values remain false. The server returns
  `null`, so the app re-reads safety state; if the result is not confirmed, it
  displays “The result wasn't confirmed” with Refresh and never blindly
  retries the toggle. A concurrent change after preflight can still race it.
  See
  `pkg/server/server.go:toggleSessionYolo`,
  `pkg/session/session.go:ToggleYolo` / `SetSafetyPolicy`,
  `apple/Sources/DockerAgentApp/AppModelAdvanced.swift:toggleToolsApproval`,
  and `apple/Sources/DockerAgentApp/Views/Session/Inspector/SafetyPanel.swift`;
  `tool_toggle_semantics.json` records representative state transitions.

### Per-message history, branch, search, and context

- **API fact — positional branch:**
  `POST /api/v2/sessions/:id/branches` accepts
  `{"position":N,"expected_snapshot":"<proof>"}` and returns `201`
  `{metadata,session}`. Omit position/proof to branch at the end. A supplied
  position requires `expected_snapshot`, the lowercase hex SHA-256 of
  `json.Marshal(session.MessagesSnapshot())` exactly as computed by
  `runtime.SnapshotProof`; a stale proof fails rather than silently moving the
  branch point. `/snapshot` does **not** return this proof. The Apple client now
  fetches a bounded raw snapshot response and scans it without re-encoding; for
  ordinary compact, non-nil histories it retains exact `session.messages`
  member bytes, validates per-item positions, binds branch anchors to session
  identity/instance/epoch/cursor/item hash, then sends the whole-array proof with
  the exclusive cut after the selected top-level item. Eligible transcript
  rows expose “New Session Up to Here”; the session menu's existing “Branch
  Session” remains an end branch. The raw extractor rejects transformed or
  pretty-printed member bytes, duplicate keys, missing/ambiguous identity,
  null messages, oversized snapshots and stale anchors. Specifically, when the
  stored slice is nil the snapshot says `messages:null`, but
  `MessagesSnapshot()` normalizes nil to `[]`; the snapshot does not contain
  the required proof input, so this edge fails closed rather than substituting
  empty-array bytes. The Go-generated
  `branch_snapshot_proof_cases.json` covers compact raw cases, chunked byte
  reconstruction, raw proof requests, nil/empty distinction, Go HTML escaping,
  Unicode, and nested/nonmessage items. Each selectable item is anchored to
  exact position/item bytes plus instance, epoch and cursor. A stale branch
  conflict offers refresh and a new selection, never an automatic retry. The
  fixtures verify synthetic Go examples; parser bounds or unsupported JSON
  shapes may still safely reject a branch.
  Position is a top-level session item boundary
  (includes summaries, errors/termination, and sub-session items), not a
  flattened transcript row or user-message ordinal. `POST
  /api/v2/sessions/:id/fork` instead accepts
  `{"user_message_index":N}`: zero-based ordinal among flat user-role
  messages, and excludes the selected user message from copied history so it
  can be prefetched into the composer. It needs no snapshot proof and can
  truthfully support a “fork before user message N” action, not arbitrary
  transcript-item branch. These are distinct features. See
  `pkg/api/session_capabilities.go`, `pkg/runtime/session_capabilities.go`,
  `pkg/session/branch.go`, `pkg/server/session_capabilities.go`, and
  `pkg/server/server.go`.
- **API fact — individual message edit:**
  `PATCH /api/v2/sessions/:id/messages/:msg_id` body
  `{"message":<session.Message>}` updates by durable numeric database message
  ID (not transcript position); success is `{"status":"updated"}`. But
  `session.Message.ID` is tagged `json:"-"`, and the `session.messages` item
  DTO has no `position`; the ordinary snapshot therefore exposes no edit ID.
  It does serialize the ordered top-level `session.messages` array, whose
  array index is the branch position; nested sub-session arrays are their own
  sequence. `recent-chat.position` identifies that same top-level item index.
  The patch is a full replacement, not a content-only merge: omitted
  `agent_name`, serialized `chat.Message` fields, or provenance/pending/sender
  fields are replaced with zero values. The Apple app now exposes a deliberately
  narrow edit for nontruncated, text-only user messages: it uses recent-chat's
  durable `id` and top-level `position`, fetches a fresh snapshot as generic
  `JSONValue`, verifies identity/role/origin/content at that position, and
  changes only `message.content` while retaining the other JSON fields. It
  fails closed on attachments, mismatches, and unsupported message shapes; it
  does not rebuild a message from recent-chat text alone. There is no
  compare-and-swap/expected-content guard, so another edit can still race the
  final snapshot check and be overwritten. `message_added.session_position`
  is a commit boundary, not a durable DB message ID. Bounded
  `GET /:id/recent-chat` exposes both `id` and `position` for selected visible
  messages but remains a partial/truncated browsing projection, not the full
  transcript index. Active/busy sessions conflict; the canonical owner rejects
  transcript mutation while executing, settling, or with pending input. There
  is no per-message DELETE or dedicated message-search route. Do not use
  `message_added` as content. `GET /:id/recent-chat` is bounded: default 12/max
  24 items, max 2,048 UTF-8 text bytes/item and 32 KiB total response, opaque
  before cursor, stale-anchor HTTP 409, no reasoning text.
- **API fact — user-message fork:** Body `POST /api/v2/sessions/:id/fork`
  takes `{"user_message_index":N}` and returns a new `SessionResponse`.
  `N` is a zero-based ordinal among flattened, non-system user-role messages,
  not a database ID or transcript-item position; the selected user message is
  excluded from copied history, so the client needs the source transcript to
  prefill its content. A target in a sub-session is HTTP 400. The Apple app
  exposes “Continue from Here in New Session” on eligible top-level user
  messages and prefills the excluded message; nested and attachment-bearing
  messages are not offered because the current composer handoff is text-only.
  This remains distinct from proof-gated arbitrary item-position branching;
  do not label it “fork before every transcript message.” See
  `pkg/server/session_manager.go:userMessageOrdinalToItemIndex` and
  `pkg/server/server.go:forkSession`.
- **API fact — MCP prompts (discovery, explicit execution):**
  `GET /api/v2/sessions/:id/mcp/prompts` returns a map keyed by an
  opaque session-local discovery key; values are
  `{name,description,arguments:[{name,description,required}]}`.
  For a bound session, keys are `"<toolset-index>/<prompt-name>"` (index is
  the agent's toolset order), not just the prompt name; the server uses that
  key for `POST /api/v2/sessions/:id/mcp/prompts/execute` body
  `{"name":"<discovery-key>","args":{"arg":"value"}}`, returning
  `{"text":"..."}`. Execution only fetches prompt text: it does not submit
  to the agent or execute a tool; show returned text for user review and
  explicit send. The legacy unbound app surface uses bare names and is not
  equivalent to this bound canonical route. Source is `pkg/tools/prompts.go`,
  `pkg/runtime/session_capabilities.go`, and `pkg/server/session_http.go`.
- **API fact — fork-mode skills:** `GET /api/v2/sessions/:id/skills` returns
  only fork-context skills, not inline slash-command skill bodies. Items are
  `pkg/skills.Skill` Go structs (no JSON tags, so exported properties are
  capitalized: `Name`, `Description`, `FilePath`, `BaseDir`, `Files`, `Local`,
  `License`, `Compatibility`, `Metadata`, `AllowedTools`, `Toolsets`, `Context`,
  `Model`, `InlineContent`); local file/base paths and inline skill content
  can consequently be exposed by this discovery response. `POST /api/v2/sessions/:id/skills/run` takes
  `{name,task,operation_id?}` (`operation_id` is generated if omitted),
  rejects names not in current fork-skill inventory, and returns `202`
  `{session_id,operation_id}` after starting asynchronous child execution.
  Observe `/events` for `skill_operation` lifecycle status `accepted`, then
  `completed|failed` with same operation ID; detaching observation does not
  undo execution. `operation_id` is a correlation label, **not an idempotency
  key**: the driver stores it only in volatile active-operation fields; no
  dedupe/retry endpoint or durable skill-operation record exists. A second
  POST while active is rejected busy even with matching ID; after terminal
  completion the slot clears, so retrying the same ID can launch another fork.
  `/status` reports only generic `state:"running"` while the op is active
  (normally no `turn_id` or skill operation ID); snapshot has no skill-operation
  field. Only the `skill_operation` journal event exposes exact
  ID/name/status/error. It is replayable only within the same in-memory epoch
  and bounded event retention (default 1,024 events/8 MiB); a fresh no-cursor
  snapshot carries no prior completed operation event, and restart/recovery
  has no operation record. A stop/delete can fence the worker before its
  terminal event, so even a previous `accepted` does not guarantee a later
  `completed|failed` record. Therefore after an ambiguous POST result do
  **not blindly retry**. Before posting, preserve a client-generated
  `operation_id` and current `(epoch,cursor)` from snapshot/events; reconnect
  with `?since=<cursor>&since_epoch=<epoch>` to seek matching event. If epoch
  changed or a gap evicted it, report outcome unknown and require explicit
  user choice before starting a new operation. Discovery also requires the
  session `fork_skills` capability; execution rejects unavailable/non-fork
  names; an active skill operation occupies the session operation slot until
  terminal
  completion. Unlike MCP prompt execution, this starts an isolated fork
  subagent; native UI should confirm task/name and render async lifecycle.
  See `pkg/server/session_http.go`, `pkg/tools/builtin/skills/skills.go`,
  `pkg/runtime/session_handle.go`, and `pkg/runtime/event.go`.
- **API fact — workspace snapshot undo/reset:**
  `GET /api/v2/sessions/:id/workspace-snapshots` returns
  `{session_id,enabled,files:[count,...],proof}`; list is oldest-first and
  keeps only file counts, not individual paths. `POST
  /api/v2/sessions/:id/workspace-snapshots/undo` body
  `{"expected_proof":"<proof>"}` reverts the last checkpoint.
  `POST /api/v2/sessions/:id/workspace-snapshots/reset` takes the same proof
  plus `keep:N`: keep 0 is the original pre-agent workspace; keep N retains
  that many leading checkpoints and resets later state. Both return
  `{session_id,restored_files,restored}`. The proof is session-bound
  SHA-256 of Go-marshaled `{ID,Infos}` (IDs and file counts), not hashes of
  captured path/content; it binds inventory/order, not checkpoint file bytes.
  Stale proofs fail before restore. Operations require
  `metadata.capabilities.snapshots`, a bound workspace and a quiescent owner;
  they mutate workspace files and should be explicit destructive actions.
  The native Go TUI `/undo` waits until not working; `/snapshots` lists
  file-count checkpoints and `r` directly restores the selected point,
  including original (there is no extra confirmation prompt). This endpoint
  cannot preview affected paths or provide atomic file-level confirmation;
  a native desktop UI should show a destructive-action confirmation based on
  session/workspace and selected checkpoint. See `pkg/api/workspace_snapshots.go`,
  `pkg/runtime/workspace_snapshots.go`, `pkg/server/workspace_snapshots.go`,
  and `pkg/tui/handlers.go`.
- **API fact — recovery, batch export/delete:**
  `GET /api/v2/sessions/:id/recovery` returns one JSON object with `id`,
  `title`, `created_at`, `messages`, token counts, `working_dir`,
  `tools_approved`, `permissions`, and optional `errors` (ordinary batch
  export omits permissions/errors). `POST /api/v2/sessions/batch/export` body
  `{"session_ids":[...]}` (format field has documented `json|zip`, default
  JSON) returns a JSON map `{export_format:"json",timestamp,sessions,
  session_count}`; each session has ID/title/time/messages/tokens/working_dir/
  tools_approved. Missing IDs are silently skipped and session count reports
  exported, not requested, rows. **Implementation does not consume `format`,
  create `BatchExportSessionsResponse.data_url` / `.file_path`, or perform ZIP
  export**: actual JSON map wins over declared response DTO/comments. No export
  cap/paging contract was found. `POST /api/v2/sessions/batch/delete` requires
  nonempty `session_ids`, attempts independently, and reports
  `{deleted_count,failed_count,failed_ids?}`; partial failure is HTTP 200.
  It is destructive with no preview, undo, confirmation token, or CAS. Session
  deletion routes through owning runtime/source; invalid/unroutable or
  unsupported child deletion can be a failed ID, not a full-batch abort.
  Native UI should explicitly confirm IDs/count, and only show rows reported
  deleted. See `pkg/server/server.go`, `pkg/server/session_manager.go`,
  `pkg/api/types.go`.
- **API fact — context breakdown:** `GET /api/v2/sessions/:id/context`
  returns estimated next-prompt composition: `system_prompt`,
  `tool_definitions`, `prompt_files`, `messages`, `tool_results`,
  `compaction_summary` (`{tokens,items}`), optional prompt/attached-file items,
  `latest_compaction_summary`, `context_limit`, `model`, and optional
  compaction model window attribution. Token counts are estimates, not exact
  provider tokenizer/accounting. Historical handles and handles lacking
  `context_inspection` reject it. No server-side full-text history search,
  arbitrary message range query, or deletion endpoint is present; for current
  native search, use the opened snapshot transcript locally, with recent-chat
  only for bounded browsing. See `pkg/runtime/context_breakdown.go`,
  `pkg/server/session_recent_chat.go`, `pkg/api/session_recent_chat.go`, and
  `pkg/runtime/session_edit.go`.
- **API fact — child/historical browsing:** The catalog defaults to root
  sessions. `view=summary&include_children=true` can list child candidates,
  but `requires_confirmation` is not an access grant: opening a cold child
  uses prepare/confirm/open and validates ancestry plus source/tree membership.
  An opened historical session is marked `historical:true` and is transcript-
  only; `execution_available` is false internally but may be omitted from the
  JSON because its DTO tag is `omitempty`. The native app exposes an Open
  Parent action from an opened child and a cursor-paged Recent Messages view.
  Those flows do not amount to a complete historical catalog browser or an
  unbounded transcript-page API. See `pkg/server/session_catalog_page.go` and
  `pkg/runtime/session_view.go`.
- **API fact — delegation policy:** `GET /:id/delegation-policy` returns
  `{"enabled":bool}`; `PATCH` takes the same shape and returns the updated
  value. Child calls resolve to the canonical root policy; absent root override
  uses the local runtime default. Changes affect new autonomous delegation,
  not accepted work; descendants inherit the root policy. The Apple inspector
  now provides the per-tree “Allow new sub-agents” toggle. See
  `pkg/server/session_delegation.go` and `pkg/runtime/session_delegation.go`.
- **API fact — remove attachment:** `DELETE /:id/attachments` carries JSON
  body `{"path":"/absolute/attached/file"}` and returns 204. It removes that
  exact path from session attachment metadata so it is no longer propagated
  to future sub-agent delegations or skill prompts; it does not delete the
  file or erase attachment content already in prior transcript messages. The
  Apple inspector exposes removal for attached paths. See
  `pkg/server/session_http.go:removeCanonicalSessionAttachment`,
  `pkg/runtime/session_driver.go:RemoveAttachment`, and
  `pkg/session/session.go:RemoveAttachedFile`.
- **Material Apple-client omissions (not generic API availability):** current
  sources implement inline document/media submission and generated-media
  display, MCP prompt-to-draft, fork-skill tracking, context estimates,
  workspace restore, recovery/JSON export and confirmed deletion. The app also
  has user-facing pause, stop-subtree, exact-turn and root-tree workflow waits,
  per-message eligible user-text edit/fork, proof-gated branch-from-selected
  top-level item, end branch, parent navigation, bounded paged recent history,
  source registration, create-time working-directory choice, delegation
  policy, attachment removal, safety policy and a confirmed Toggle Autonomy
  action. Canceling a wait only detaches observation. The proof-gated branch
  fails closed for null-message snapshots, unsupported/raw-transformed inputs,
  or stale anchors and offers a refresh rather than silently retargeting. The
  proof-free user-message fork is a distinct action with stricter text-only/
  top-level eligibility. Message
  edit remains limited to plain user text and has no CAS, so a concurrent edit
  can race the final snapshot check. Recent history is cursor-paged but bounded;
  there is no backend full-text search. The thinking picker exposes model-
  supported levels, not an authored configuration editor.
  Message edit is intentionally restricted to plain user text and has no CAS,
  so a concurrent edit may race its last snapshot check. Recent history is
  cursor-paged but remains a bounded user-visible projection, not an unbounded
  transcript API or historical catalog browser. Local transcript search is
  limited to loaded items; there is no backend full-text search. The thinking
  picker exposes model-supported levels, not an authored configuration editor.
  See `apple/Sources/DockerAgentApp/Views/Session/SessionView.swift`,
  `apple/Sources/DockerAgentApp/Views/Session/TranscriptView.swift`,
  `apple/Sources/DockerAgentApp/Views/Session/Workflows/RecentHistorySheet.swift`,
  and `apple/Sources/DockerAgentApp/AppModelAdvanced.swift`.
- **API fact — plans, board, evaluation:** the public v2 route map has no
  plan CRUD, board, or evaluation endpoint. The built-in plan toolset exposes
  agent-callable operations, and `plan_changed` is a process-global SSE
  notification containing only scope/name/action/version, not plan content;
  it is not a client-readable plan service. Board and evaluation may be
  available in other product/CLI surfaces, but not through these public session
  APIs. See `pkg/tools/builtin/plan/plan.go`, `pkg/runtime/event.go`, and the
  registered routes in `pkg/server/server.go` / `pkg/server/session_http.go`.
- **True backend/product limits:** no server-side full-text search, resumable
  or presigned upload, general authored agent/team YAML mutation, Git
  stage/commit/hosted-PR or comment-anchored review API exists. Review/Files
  views and bounded local drafts are already implemented, but review comments
  are sent as an ordinary prompt. These are not missing routes for the client.

## Git review API and native-client policy

- **API fact — route/wire casing:**
  `GET /api/v2/sessions/:id/git-review` returns `{"snapshot": snapshot}`.
  Requests:
  - `POST …/git-review/worktree`: `{"generation":N,"worktree_id":"ID"}`
  - `POST …/git-review/directory`: `{"generation":N,"worktree_id":"ID","path":"relative/path"}`
  - `POST …/git-review/document`:
    `{"generation":N,"key":{"mode":"changes|files","worktree_id":"ID","path":"relative/path","group":"conflicts|staged|unstaged|untracked|none"}}`

  Outer request/response wrapper names use lower snake case (schemas in
  `pkg/api/gitreview.go`). **Inner `pkg/gitreview` structs have no JSON
  tags**, so current wire properties are Go field names/capitalized, e.g.
  `Generation`, `Repositories`, `ID`, `OldPath`, `Worktrees`, `Directory`,
  `Document`, `WorktreeID`, `PatchHeader`, `Old`, `New`, `Binary`, and
  `Truncated`. Do not assume lower_snake_case for those nested objects.
- **API fact — snapshot/worktree:** snapshot fields are `Generation`,
  `CapturedAt`, `MaybeOutdated`, `Scope`, `Repositories`. Scope has `Label`,
  `Note`, `Remote`, `RemoteLabel`, `Unavailable`. Repository has `ID`, `Name`,
  `Kind` (`primary|nested|submodule|none`), `Worktrees`, `HiddenOutside`, `Err`.
  Worktree has `ID`, display `Path`, `Branch`, `Detached`, `DetachedHead`,
  `Current`, `Linked`, `State` (`unscanned|clean|changed|error`), `Err`, and
  lazily filled `Files`. FileChange has `Path`, `OldPath`, `Group`, raw `Status`,
  `Added`, `Removed` (negative means unavailable), `Binary`.
- **API fact — directory/document:** directory returns `WorktreeID`, relative
  `Path`, `State`, `Entries[{Name,Dir}]`, `Truncated`, `Err`; no page cursor.
  Document returns `Key`, `State`, `Lines[{Kind,Old,New,Text}]`, `Binary`,
  `Truncated`, `Note`, `Err`, `PatchHeader`. Line kinds are
  `context|add|remove|hunk|note`. Changes document groups compare conflicts,
  staged (`HEAD → index`), unstaged (`index → working tree`), and untracked
  files; files mode reads plain text. Status/diff/file content bounds are 8 MiB
  status, 2 MiB or 10K lines per document; directory returns at most 1K
  entries and marks truncation. Traversal omits symlinks, `.git`, heavy/ignored
  folders and paths outside session workspace scope.
- **API fact — generation/cache nuance:** every explicit GET snapshot rescans,
  increments provider generation, and clears cached documents. Generation is
  not a content revision. Within the generation, `DocumentKey` results are
  cached and can be stale if workspace files change. Same-generation output
  echoes generation but carries no content hash/ETag. Fresh GET followed by
  document read gets current content at read time, not an atomic filesystem
  snapshot; a write can race it. See `pkg/server/session_git_review.go` and
  `pkg/gitreview/local.go`.
- **Client policy — draft isolation/freshness:** scope each in-memory draft by
  canonical session + workspace + repository/worktree, never one process-wide
  draft or a pane label. Refresh resets review data; preserve comments only
  within the same scope and mark them stale if their target changes. On send
  confirmation: GET a fresh snapshot; verify repo/worktree identity; fetch each
  anchored document with that new generation; compare a local document
  fingerprint and exact selected context/ranges to captured anchors. Changed,
  missing, binary/unavailable, truncated, or ambiguous anchors require new
  preview/confirmation; never silently move a comment. Generation equality is
  not this test because explicit refresh always changes generation. This is
  best-effort only: race-free validation requires a server fingerprint or
  conditional-send API, which current API does not provide.
- **Current implementation gaps (facts):** conflict status/group exists, but
  conflict document uses `git diff --cc` while parser recognizes ordinary
  `@@ `, not combined `@@@`; current conflict documents may be empty. Binary
  contents are intentionally not rendered. No directory pagination, generic
  filesystem write, server-persisted comment/draft API, or comment-anchored
  review submission API exists. Current TUI `pkg/tui/reviewdraft` is local
  memory state and serializes comments into an ordinary prompt; it is not a
  server comment contract. The current TUI anchor version hashes coordinates
  and selected context only, not document generation/content fingerprint.
- **Client policy — recipient:** pin prompt send to the active session owner.
  Current `GET /api/v2/sessions/:id/status` returns `metadata.agent_name`;
  `GET /:id/agent-info` gives `team.current_agent` and
  `agent.agent_name`. `POST /:id/messages` has no recipient/agent field and
  submits to the session’s active agent. `POST /:id/switch-agent` creates a
  new session; it is not a way to target another agent in-place. Pin target
  session ID, agent name, source/workspace identity at preview; revalidate
  session/agent before sending and show the actual recipient. Submission is a
  normal literal user message with `request_id`, not a GitHub action.

## API v2 route parity map (registered server surface)

Paths below are rooted at `/api`; `:id`, `:turnID`, `:todoID`, `:msg_id`,
`:target` are path parameters. Methods and paths are registered in
`pkg/server/server.go`, `pkg/server/session_http.go`,
`pkg/server/session_capabilities.go`, `pkg/server/session_git_review.go`,
`pkg/server/session_generated_media.go`, and `pkg/server/workspace_snapshots.go`.

- **Identity / sources / batch:** `GET /v2/server`; `GET /agents` (source
  discovery, outside `/v2`); `GET /agents/:id` (source config);
  `POST /v2/sources`;
  `POST /v2/sessions/batch/delete`; `POST /v2/sessions/batch/export`.
- **Catalog and session lifecycle:** `GET|POST /v2/sessions`;
  `POST /v2/sessions/start`; `GET|PATCH|DELETE /v2/sessions/:id`;
  `GET /v2/sessions/:id/status`; `GET /v2/sessions/:id/snapshot`;
  `GET /v2/sessions/:id/events`; `GET /v2/sessions/:id/recovery`;
  `GET /v2/sessions/:id/recent-chat`; `POST /v2/sessions/:id/messages`;
  `PATCH /v2/sessions/:id/messages/:msg_id`;
  `POST /v2/sessions/:id/retry`; `POST /v2/sessions/:id/responses`;
  `POST /v2/sessions/:id/cancel`; `POST /v2/sessions/:id/stop-subtree`;
  `POST /v2/sessions/:id/turns/:turnID/wait`;
  `POST /v2/sessions/:id/workflow/wait`.
- **Session settings and presentation:** `POST /v2/sessions/:id/tools/toggle`;
  `PATCH /v2/sessions/:id/safety-policy`;
  `PATCH /v2/sessions/:id/permissions`; `PATCH /v2/sessions/:id/tokens`;
  `PATCH /v2/sessions/:id/title`; `POST /v2/sessions/:id/title`;
  `PATCH /v2/sessions/:id/starred`; `GET /v2/sessions/:id/agent-info`;
  `GET /v2/sessions/:id/agent-config`; `POST /v2/sessions/:id/switch-agent`;
  `GET|PATCH /v2/sessions/:id/delegation-policy`.
- **Tree, tools and optional controls:** `GET /v2/sessions/:id/tree`;
  `GET /v2/sessions/:id/live-sessions`; `GET /v2/sessions/:id/todos`;
  `PATCH /v2/sessions/:id/todos/:todoID`;
  `DELETE /v2/sessions/:id/todos/:todoID`; `GET /v2/sessions/:id/tools`;
  `POST /v2/sessions/:id/toolsets/restart`;
  `GET /v2/sessions/:id/permissions`; `GET /v2/sessions/:id/mcp/prompts`;
  `POST /v2/sessions/:id/mcp/prompts/execute`;
  `POST /v2/sessions/:id/branches`; `POST /v2/sessions/:id/fork`;
  `POST /v2/sessions/:id/summaries`; `POST /v2/sessions/:id/compact`;
  `POST /v2/sessions/:id/compact/:target`;
  `GET /v2/sessions/:id/context`; `GET /v2/sessions/:id/skills`;
  `POST /v2/sessions/:id/skills/run`; `GET /v2/sessions/:id/models`;
  `POST /v2/sessions/:id/models/refresh`;
  `PATCH /v2/sessions/:id/model`;
  `GET /v2/sessions/:id/thinking-level`;
  `POST /v2/sessions/:id/thinking-level/cycle`;
  `PATCH /v2/sessions/:id/thinking-level`; `POST /v2/sessions/:id/pause`;
  `DELETE /v2/sessions/:id/attachments`.
- **Workspace snapshots, generated media and Git review:**
  `GET /v2/sessions/:id/workspace-snapshots`;
  `POST /v2/sessions/:id/workspace-snapshots/undo`;
  `POST /v2/sessions/:id/workspace-snapshots/reset`;
  `POST /v2/sessions/:id/generated-media`;
  `GET /v2/sessions/:id/git-review`;
  `POST /v2/sessions/:id/git-review/worktree`;
  `POST /v2/sessions/:id/git-review/directory`;
  `POST /v2/sessions/:id/git-review/document`.

This lists registered v2 API routes; it does not imply every per-session
operation is supported by every handle or capability set. Top-level `/health`,
`/ready`, `/api/ping`, and `/api/ready` probes and non-v2 `/api/agents` and
legacy `/api/sessions` routes are separate surfaces.

## Source map

- Route/auth/handlers/readiness: `pkg/server/server.go`,
  `pkg/server/server_info.go`, `pkg/server/session_http.go`,
  `pkg/server/session_capabilities.go`, `pkg/server/session_git_review.go`.
- Wire DTOs: `pkg/api/server.go`, `pkg/api/session.go`,
  `pkg/api/gitreview.go`; Git provider/types: `pkg/gitreview/local.go`,
  `pkg/gitreview/types.go`; remote adapter: `pkg/runtime/remote_git_review.go`.
- TUI reference: `pkg/tui/review.go`, `pkg/tui/review_adapter.go`,
  `pkg/tui/components/review/{types.go,reader.go,draft.go,model.go}`,
  `pkg/tui/reviewdraft/draft.go`, `pkg/tui/review_targets.go`,
  `pkg/tui/review_draft.go`, `pkg/app/review_submit.go`.

Audit basis: source/docs/tests inspected read-only; no live API calls or package
tests/app builds were run. A one-off temporary Go helper was run only to marshal
synthetic fixtures from existing wire types.
