# Native Apple client status and parity ledger

The native SwiftUI app targets iOS/iPadOS and macOS 26 and connects to one or
more configured `docker agent serve api` servers. This is an early functional
client milestone, not full TUI parity. Build setup/signing policy is in
[Docs/BUILD.md](Docs/BUILD.md); registered API facts are in
[Docs/API_CONTRACT.md](Docs/API_CONTRACT.md). Source-level ownership and API
capability gating remain authoritative where this ledger summarizes behavior.

## Open and build in Xcode

From the repository root, run `open apple/DockerAgent.xcodeproj`. Choose the
`DockerAgent-iOS` or `DockerAgent-macOS` scheme. The minimum OS is 26 for both;
SDK build checks in [Docs/BUILD.md](Docs/BUILD.md) do not require signing.

## Implemented in current app source

- **Server onboarding:** multiple server profiles; HTTPS required except
  explicit loopback HTTP; bearer token in Keychain; exact-tested draft required
  before save; API/session version and instance identity checked. Blank token
  retains a same-origin credential and changing origin deletes the previous
  credential before the new profile is saved.
- **Catalog and session:** paged root-session catalog, agent listing, create or
  create-and-start, confirmed prepare/open flow, initial snapshot, live bounded
  SSE replay/cursor validation, stateful transcript projection, reconnect and
  re-snapshot on gaps/epoch changes. Stream tasks are scoped to open session
  views; detach does not stop server work.
- **Conversation:** submit/steer, stop/cancel, failed-turn retry, durable
  idempotent sends with explicit handling for ambiguous delivery, interaction
  responses (five supported approval decisions, elicitation, max iterations),
  transcript tool/reasoning/usage/tree facts. The outbox preserves immutable
  pending payloads and never retries an accepted send. The composer supports
  bounded inline document attachments (`multi_content`); user history displays
  metadata-only attachment summaries. Generated-media references are resolved
  only through the authenticated owner-bound API, with a 32 MiB response cap.
- **Common session controls:** title/star, queue edit/cancel, todos, model and
  thinking-level selection, compaction, branch, and switch-to-agent-as-new
  session using the bound session's actual `/agent-info` team roster, and
  verified branch-from-item with a server JSON snapshot proof. Branch anchors
  are tied to the selected server instance, snapshot epoch/cursor, complete
  message-array proof, and exact item bytes; live journal events invalidate
  anchors until Refresh Conversation re-establishes one. The server's branch
  endpoint remains the final CAS authority. Mutations are
  capability/history/identity gated where supplied by the server.
  Recent-chat is available as a client call; this is not a complete historical
  conversation browser.
- **Inspector:** typed read-only agent details (no instruction/definition text),
  actual team roster, effective session/source permission inspection, safety
  policy and session-rule editing, a confirmed legacy Toggle Autonomy action,
  tool descriptors and restartability-gated toolset restart. The toggle sends
  the server's real bodyless toggle endpoint, requires explicit confirmation in
  both directions, rechecks the displayed mode just before dispatch, and never
  retries an uncertain response; it refreshes the server's remembered mode.
  Because the endpoint has no CAS, another client can still change the mode
  after the final check. Strict/Balanced are available as explicit policy
  alternatives. Permission edits re-read expected rules before a replace-all
  request; the API provides no atomic compare-and-swap guarantee.
- **Review and files:** read-only Git review snapshot/worktree/directory/document
  API, files view, bounded local review drafts with explicit anchors and
  re-anchor, fresh document validation, send preview, and immutable ambiguous
  review-send target/payload. No staging, commit, or hosted-PR action is exposed.
- **Native surface:** SwiftUI iPhone/iPad/macOS navigation, session transcript,
  interaction/composer and document attachments, authenticated generated-media
  display, inspectors, Git review/files, MCP prompt-to-draft, fork-skill
  operations, context estimates, JSON recovery/export, confirmed deletion, and
  proof-bound workspace restore. Fork-skill `operation_id` is correlation only:
  ambiguous acceptance remains unknown, is not retried, and can be resolved only
  by matching journal events when retained same-epoch replay is available. See the
  concrete app sources under `Sources/DockerAgentApp/Views/`.

## Supported behaviors and client/API limits

The following routes are now exposed through capability-checked native controls:

- Session pause/stop-subtree, exact-turn/workflow wait and delegation policy
  controls are exposed with capability/confirmation gates; waits are bounded,
  cancellable observations and do not cancel remote work.
- Parent navigation and bounded recent-history pages are exposed. Local search
  covers the transcript already loaded for the session; there is no server
  full-text search or arbitrary-range transcript API.
- Fork-before-user-message uses the server's flat user-role ordinal. The UI
  only offers eligible top-level, untruncated, text-only user entries; nested
  sub-session targets and attachment-bearing targets are not forked as a text
  prefill. Branch-at-an-item is separate: it retains raw bounded snapshots,
  hashes the exact Go-compatible top-level `messages` bytes, and submits the
  selected item's exclusive cut with the expected proof. Snapshot or selected
  item changes return stale/409; the client does not retarget or retry.
- User-message editing is limited to cached recent-chat entries that can be
  revalidated against the same top-level raw snapshot item, with durable
  database ID and complete raw message metadata. Truncated, changed,
  runtime/agent-origin, non-user, attachment-bearing or otherwise complex
  records are rejected. The API has no compare-and-swap, so a concurrent edit
  can still race the final revalidation.
- Working-directory selection is an explicitly entered server-side path, not a
  local filesystem picker or a server-provided/validated workspace chooser.
  Source registration registers a server-host path only; it never uploads or
  opens a client file path.
- The composer supports bounded inline document submission, but there is no
  upload/presigned/resumable endpoint, each JSON request is capped at 768 KiB,
  and arbitrary local files are not processed through server-side
  `ProcessAttachment`. Attachment deletion only removes future prompt
  propagation; it does not delete a workspace file or historical transcript.
- The legacy tools-approval toggle is exposed as “Toggle Autonomy”, not a
  set-value control: its remembered prior mode and lack of CAS mean a racing
  change can alter the result. It requires explicit confirmation and never
  auto-retries. There is no typed agent/team YAML editor or typed-team mutation
  route.

The second through last items above are the bounded gaps and safety limits; the
first bullet records supported controls so readers do not mistake them for
omissions. Use `Docs/API_CONTRACT.md` for exact wire facts and proof semantics.

## Backend/API limits (not client omissions)

- The Git-review surface is read-only and has no stage/commit, hosted pull
  request, server-persisted comment draft, or comment-anchored submission API.
  Review comments are serialized into an ordinary session message.
- Review confirmation rechecks current files and recipient, but the API has no
  atomic content fingerprint/conditional-send operation; a filesystem change
  can race the final check. Directory listing has no pagination cursor.
- Conflict documents use combined diffs that the current parser cannot display;
  binary documents are not renderable. These should be stated, not synthesized.
- Team/agent configuration editing is not a registered mutation workflow in
  the current API route inventory. Do not build a fake native-only editor or
  imply remote state changes without a real API contract.

## Verification and environment

From `apple/`:

```sh
swift test
./Scripts/build-native.sh
```

The authoritative unfiltered command `swift test --scratch-path /tmp/source-final-finite`
passed all 115 tests in 14 suites (log: `/tmp/docker-agent-native-final-unfiltered.log`). Coverage
includes deterministic Go-marshaled session, media, generated-media,
source/root, inspector, workflow and advanced API fixtures; mocked AppModel
HTTP/SSE send/retry/interaction flow; SSE idle-timeout, heartbeat, cancellation
and authorization tests; scene lifecycle tests with a stalled URLProtocol;
outbox/media persistence; raw-message-edit metadata preservation; fork ordinal
and parent projection; and seven view-helper regressions. No live server or real
credentials were used. Packaging built the current production sources with
`DERIVED_DATA_ROOT=/tmp/DockerAgentProofFinal ./Scripts/build-native.sh` from
`apple/`: unsigned iOS Simulator-SDK, iOS device-SDK, and macOS SDK builds all
succeeded. Its safe macOS launch check remained alive for four seconds without
log output before packaging terminated it; this validates process launch only,
not visual behavior, live-server behavior, or iOS runtime. Current Xcode
is 26.6 with Swift 6.3.3 / SDK
26.5; no simulator runtime is installed. SDK builds do not launch the app. A
packaging-owned safe macOS launch used the built app's `--ui-smoke-test` mode;
PID 61325 remained alive four seconds with no log output before packaging
terminated its own process. This validates launch only, not visuals, live server
behavior, or iOS runtime.

## Remaining first-class product scope

- Typed team/agent configuration editing has no canonical mutation route.
  Source registration is available, but a server-selected workspace-choice
  API is not present; the native working-directory field accepts an explicit
  remote path only.
- Larger/prepared/multipart attachments are limited by the absent upload API
  and client request budget; inline bounded document support is present.
- Remaining history limits: no server full-text search. Branch anchors are
  only available for rows from a complete verified raw snapshot and are
  invalidated by live journal changes until refreshed. Recent user-message text
  edits work only for a fresh, untruncated, simple text user record revalidated
  against the raw snapshot and positive durable database ID; the API provides no
  CAS, so a concurrent edit can race final revalidation. Fork-before-user is
  distinct from arbitrary-item branch and only accepts exact server user
  ordinals for eligible top-level text rows.
- Pause, subtree stop, delegation policy, waits, source registration and remote
  working-directory entry are implemented; no typed team/agent YAML mutation
  route or workspace chooser exists. Image/document sends are inline and capped
  by request budget; server-side preparation/upload is not available.
- Plan notifications expose only changed-plan identity/version; the server has
  no public plan CRUD/content route. Board and evaluation HTTP routes are not
  registered, so these workflows cannot be implemented as native client calls
  without a server API.
- Git review is client-side annotation serialized as ordinary conversation
  text over read-only review routes, not a server comment or hosted-PR system.
- The macOS smoke run verifies process launch only, not visual/accessibility
  behavior. No simulator runtime or iOS runtime smoke test was available.
  Background streaming and reliable off-screen notifications are not provided.

This is not complete TUI parity. Capability availability, source-defined safety
and server identity remain authoritative; do not infer support from view or
historical transcript content.
