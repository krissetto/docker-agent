# docker-agent for Apple platforms — Design Blueprint

Native SwiftUI client for iPhone, iPad and macOS that connects to one or more
user-added docker-agent API servers (`docker agent serve api`). It is a native
product, not a terminal clone or web view.

Status: design handoff. See [Scope and status](#scope-and-status) before
treating any section as a commitment.

## Principles

- **Calm, native, agent-aware.** The transcript is the product; chrome stays
  system-standard. Every message, tool and approval shows which agent owns it.
- **Glanceable.** Live work is one compact line by default, details on demand.
  At any moment the user can answer: who is acting, what are they doing, do
  they need me?
- **Server-honest.** Affordances follow server-reported capabilities: hide what
  is unsupported, disable with a reason what is temporarily unavailable. Never
  imply that the client owns execution or that closing it stops work.

## Platform targets

- One multiplatform SwiftUI app for iOS/iPadOS/macOS 26 (not Catalyst), using
  `@Observable` models.
- Suggested layering: an `AgentKit` package (API client, SSE decoding, session
  projection, Keychain; no SwiftUI) under the app/UI targets. Views read
  projections, never raw events.

## Information architecture and navigation

### Mac and iPad

`NavigationSplitView` with three columns plus an `.inspector`.

1. **Sidebar**
   - **Needs You** across all servers, from tree nodes' `needs_attention` /
     `waiting_on`; each row states why (approve tool, answer question, failed).
   - **Running** across all servers.
   - One section per server: name, health dot, session count. Context menu:
     Edit, Reconnect, Remove. "Add Server…" at the bottom.
2. **Session list** for the selected server or filter
   - Groups: Waiting, Running, Recent; Starred filter; search.
   - Row: title, owning agent (dot + name), relative time, one-line status,
     cost when space allows (hover on Mac).
   - Root sessions only; subagents live in the session's tree, not the list.
3. **Session detail**: transcript, activity strip, interactions, composer.

### iPhone

`NavigationStack`: root (Needs You, then Servers) → Sessions → Session. The
inspector is a sheet with medium/large detents, opened from the toolbar agent
chip.

### Windows instead of TUI tabs and panes

- `WindowGroup(for: SessionRef.self)` opens a session in its own window.
- macOS window tabbing and iPad multiwindow replace TUI tabs/split panes. No
  in-app tab system.
- Server management: Settings scene on Mac, sheet on iOS.

## Screens

### Add / Edit Server (sheet)

- Fields: name, base URL, bearer token (Keychain, obscured).
- "Test Connection" calls `GET /api/v2/server` and shows identity, version and
  capabilities as plain checkmarks.
- Saving requires a successful test.
- Transport: HTTPS by default. Plain HTTP is allowed only under the app's
  explicit local-development exception policy (for example loopback); other
  plain-HTTP hosts are rejected with an inline explanation, not merely warned
  about. The exact allowed set is an integration decision, mirrored in ATS
  configuration.
- Inline errors: unauthorized, unreachable, not a docker-agent server,
  insecure transport not permitted.

### Session detail — transcript

- Left-aligned, full width, reading width capped near 720pt and centered on
  wide windows. `LazyVStack` in a `ScrollView`; follows the bottom unless the
  user scrolls up, then shows a "Jump to latest" capsule.
- **Ownership**: an agent header (colored dot + name) appears only when the
  owner changes; subagent context reads `coder · via root`.
- **User turns**: subtle filled card (secondary system fill). Origin follows
  server metadata: agent-to-agent input shows "from root"; runtime-origin input
  is never rendered as chat. Never classify origin from message text.
- **Assistant text**: native Markdown; code blocks in SF Mono with Copy.
- **Tools**: one line — SF Symbol, verb, target (path/URL), status, duration;
  disclosure reveals arguments and result.
- **Shell**: muted single line `$ git status`; output in a collapsed monospace
  disclosure.
- **Reasoning**: collapsed "Thought · 12s".
- **Delegation**: thin inline divider `root → coder · Open` linking to the
  child session; the return shows `coder → root`.
- **Errors**: inline row with Retry.

### Session detail — bottom stack (top to bottom)

1. **Activity strip**: owning agent dot, current tool or "Thinking…", elapsed
   time, "+2 agents" when subagents run. Tap opens the Agents inspector. Hidden
   when idle.
2. **Interaction cards**: inline, not modal; they stack; each is agent-owned.
   - Tool approval: agent, tool, short argument preview with Details. Actions:
     Allow Once (prominent), Always Allow This Tool, More (Balanced Mode, Allow
     All Tools — labeled as session-wide), Deny, Deny with Reason.
   - Like the TUI (default "No"), no global or blind keystroke approves. On Mac,
     when a card has focus, TUI letters work: Y, T, N, R.
   - Elicitation uses native form fields; max-iterations gets Continue/Stop.
   - Cards resolve on `interaction_resolved`, with a brief "Answered
     elsewhere" fade when another client answered.
3. **Composer**
   - Agent chip: current agent details. Switching agent reads "Continue with
     coder in a new session" because the server branches.
   - Multiline field; "/" suggests agent commands, MCP prompts, skills.
   - Send submits (queues while a turn runs). Steer is an explicit secondary
     action (long-press Send, ⌥⏎). Stop replaces Send while running.
   - Queued inputs appear as small chips above the field.

### Session modes

The session toolbar has a segmented control: **Conversation · Review · Files**.
Git review is a first-class workspace, not an inspector tab.

### Inspector (Conversation mode; ⌘1–3) and session menu

Every control is gated by the session's reported capabilities
(`session_editing`, `pending_message_removal`, `branching`, `model_switching`,
`model_catalog_refresh`, `compaction`, `thinking_levels`, `todo_editing`,
`todos`, `execution_available`). Unsupported controls are hidden, and
historical sessions or sessions without execution show "Read-only history"
with mutations disabled.

- **Agents**: live subagent tree (dot, name, status, needs-you marker); rows
  open child sessions.
- **Todos** (only with `todos`): grouped In Progress / To Do / Completed
  (collapsed). Tapping the status glyph cycles status; the context menu sets a
  status, edits the text (compare-and-swap against the shown text) or deletes;
  swipe to delete on iOS.
- **Session**: model picker (configured / custom / catalog groups), refresh
  catalog, thinking level with the TUI's six-cell gauge, and usage (tokens,
  cost).
- **Session menu** (toolbar ⋯): Star/Unstar, Rename…, Compact Conversation…
  (optional instructions), Branch Session, Continue with Another Agent ▸
  (opens a new session; the original stays).
- **Queued inputs** sit above the composer: an Edit action on user messages
  that haven't started, and Clear Queue. Agent-sent inputs show their sender.

### Review workspace

Built only on the read-only `git-review` snapshot/worktree/directory/document
routes. No staging, committing or hosted-PR features.

- **Mac / iPad (regular width)**: three panes in the session window.
  1. Navigator (280pt, toggle in toolbar): repositories → worktrees
     (disclosure, loaded lazily on expand) → files grouped Conflicts / Staged /
     Unstaged / Untracked. File rows show raw status, rename source, `+a −r`
     (hidden when the server reports −1), binary, and draft comment count.
     Hidden linked worktrees outside the workspace are counted, not invented.
  2. Diff (flexible): unified or split. Split is offered only when the pane
     is at least 760pt wide; otherwise it falls back to unified with an
     explanation. Find in file (⌘F, ⌘G / ⇧⌘G).
  3. Side panel (380pt, ⌥⌘J): Conversation (full transcript, interactions and
     composer) or Draft.
- **iPhone**: navigator list → pushed unified diff; Draft and Conversation
  open as medium/large sheets from the toolbar.
- **Selection**: tap a line to select; shift-click (Mac), "Extend" toggle
  (iOS), context menu "Extend Selection to Here", arrow / shift-arrow keys,
  and VoiceOver actions (Select line, Extend selection, Comment). Return
  comments, Esc clears. Hunk and note rows are not selectable.
- **Anchors** use the document's real old/new line coordinates, the exact
  selected line texts, the document fingerprint and the scan generation.
  Limits are enforced in the UI: 100 comments per draft, 16 KiB per comment,
  400 lines / 256 KiB of quoted context.
- **States shown only from API facts**: binary (no preview), truncated
  (banner), document note, error, valid empty diff ("No Textual Changes"), and
  conflicts with no renderable lines ("Combined Conflict Diff Unavailable" —
  the server does not parse combined diffs). Expired scans (409) offer Rescan.
- **Drafts** are bound to server, instance, session, repository, worktree and
  workspace label. Only comments matching the current document fingerprint and
  not stale are drawn inline. Others are counted in a banner and kept in the
  Draft panel with their original lines; "Re-anchor…" requires an explicit new
  selection. Nothing is silently re-anchored or discarded.
- **Send**: "Review & Send…" rechecks every comment against a fresh scan and
  the session's current agent, then shows a preview (recipient, session,
  workspace, count, literal message). The recipient is always the focused
  session's current agent — no picker. A pending ambiguous send is retried
  with the same request and recipient and cannot be retargeted.

### Files

Separate read-only browser over `git-review/directory` and `document`
(files mode): worktree picker, folder navigation with back (⌘[), numbered
file view. Truncated listings say so; no pagination is invented. Files does
not create review comments.

### Pending sends

Unacknowledged submissions from the durable outbox appear as compact rows
above the composer: payload preview, state (Not sent yet, Sending…, Delivery
unknown — Retry is safe), **Retry** (same payload, mode and request ID) and
Discard (confirmation required when delivery is unknown). This is distinct
from the transcript's **Retry** on a failed turn, which asks the server to rerun
the turn. Accepted sends are never resent. The composer keeps its text on a
definite failure and clears it once the server accepts the message.

### Safety, tools and agent details (implemented)

- **Safety tab**: four policies, each with a one-line consequence. Changing the
  policy needs a confirmation sheet; Autonomous also needs an explicit
  acknowledgement toggle. Session allow / ask / deny rules can be removed and
  added one at a time (list plus pattern, sent with the shown rules as the
  expected value). Agent-config rules are read-only. **Autonomy** shows
  whether all tools are currently allowed and offers a **Toggle Autonomy…**
  button, deliberately not an on/off switch: the server flips the setting
  rather than setting a value, and another client may change it. Every toggle
  is confirmed with its consequence in both directions (turning off returns to
  the mode used before). An unconfirmed result shows "The result wasn't
  confirmed" with Refresh and no retry. To set a safer mode explicitly rather
  than switching, choose Strict or Balanced. That sets a value but doesn't
  stop other clients from changing it later. It has no keyboard shortcut.
- **Tools tab**: toolsets show a state dot and last error, with Restart only
  when the server reports the toolset as restartable. Tools are listed by
  category.
- **Inspect** (ⓘ on agent rows): a read-only sheet with typed fields only —
  model and fallbacks, sub-agents, handoffs, skills, limits, options and
  toolsets. Never instructions or raw configuration.
- **Continue with Another Agent** uses the session's team roster from
  agent-info and is hidden when the roster is unavailable. `/api/agents`
  lists sources, not team members.

### Attachments and generated media (implemented)

- The composer's paperclip menu offers Photos and Files; file URLs can also be
  dropped. Only user-selected local bytes are used. Each document is at most
  512 KiB; larger photos are re-encoded as JPEG to fit, which drops metadata.
  Chips show thumbnail, name and size, plus "X of 768 KB app limit per message",
  and Send is blocked over the limit. Attachments travel in the same durable
  send record; the pending row lists their names.
- Sent user messages show attachment metadata only (name, type, size).
- Assistant messages show generated media fetched only through the session's
  generated-media resolver. Images are downsampled thumbnails; other files
  show as tiles. Quick Look and Share use a sanitized owner-only (0600; iOS
  complete file protection) copy in the app's caches. It is kept while Quick
  Look is open and deleted when the preview is dismissed and the tile is gone.
  Shared copies have no guaranteed lifetime: ShareLink reports no completion,
  so they are left in place and swept after 24 hours.

### Session workflows (implemented)

- **Prompts & Skills** (✨ in the composer; shown only with `mcp_prompts` or
  `fork_skills`): MCP prompts take their arguments and show a preview, then
  Insert appends the text to the composer. Nothing is sent automatically.
  Fork skills show name, description and model only — never paths or skill
  content. Run starts a separate agent. If the start isn't confirmed, the UI
  shows "Run status unknown" with no retry; a new run is an explicit,
  destructive-styled choice because it can duplicate. Unfinished runs appear
  above the composer.
- **Context (estimated)** in the Session tab (`context_inspection`): stacked
  bar and per-category tokens and items, labelled as estimates.
- **Restore Workspace…** (`snapshots`): Original plus checkpoints with file
  counts. Undo Last and Restore… are enabled only when the session is idle,
  after a destructive confirmation naming the session and workspace. Affected
  files can't be previewed; a stale proof reloads the list.
- **Export as JSON…** saves the server's JSON export for the session. JSON
  only, no ZIP.
- **Delete Session…** is confirmed explicitly. The view closes only when the
  server reports the session deleted.
- **Find** (⌘F, Conversation): searches the transcript loaded in this window.
  Sidebar search covers loaded sessions only; there is no server search.
- The agent tree shows "waiting on" and cost per agent (cost is hidden at
  zero). The Session tab has a context gauge.

### Tree control, history and batch actions (implemented)

- **Parent**: child sessions show "↑ <parent>" in the toolbar and
  "Open Parent Session" in the menu, using only the server's parent field.
- **Pause** (`pause`): the menu offers Pause, Cancel Pause Request or Resume.
  Banners read "Pause requested — waiting for a safe point" or "Paused ·
  Resume"; the UI doesn't promise a pause after the current step.
- **Stop…** on active tree rows (`stop_subtree`) asks for confirmation that
  states the scope. For the root: the whole tree stops and queued messages are
  withdrawn. For a child: only that agent and what it started stop. History is
  kept either way.
- **Wait for This Turn** (only while a current turn ID is known) and **Wait
  for All Agents to Finish** show a banner with Stop Waiting, which only stops
  watching and never stops the agents. A timed-out or failed wait says the
  agents may still be running; it never reports completion.
- **Allow new sub-agents** (`delegation_policy`) applies to new delegations
  across the tree; running work continues.
- **Attached files** in the context section offer Stop Including, which keeps
  the file out of future prompts and sub-agents. It doesn't delete the file
  or the chat attachment.
- **Recent Messages…**: a bounded, text-only history with Load Earlier while
  more pages exist, shortened-row markers and a reload when the conversation
  changed. The transcript remains the full conversation.
- **Batch**: multi-select in the session list (⌘-click; Edit on iPhone)
  exports or deletes sessions from one server, with confirmation listing
  titles and an alert naming any failures.

### Continue from a message, sources and history edits (implemented)

- **Continue from Here in New Session…** appears on user messages that carry a
  server-mapped `userMessageIndex` and requires `branching`. After
  confirmation, the server forks before that message. The new session opens
  with the message in its composer (never sent). If that composer already
  holds a draft, a banner offers Insert instead of overwriting.
- **New Session Up to Here…** appears on top-level transcript items that carry
  a server-verified branch anchor (raw snapshot hash plus item position;
  nested items have none) and requires `branching`. It copies the conversation
  through that item and opens with an empty composer. It opens in a new
  window where supported; otherwise the current draft and attachments are
  kept for when you return. A changed conversation reports an error instead
  of branching elsewhere. This path is pending live-server verification.
- Unsent drafts, including attachments, are kept in memory per session while
  navigating within a window.
- **Refresh Conversation** (session menu, and offered after a stale-branch
  error) reloads the snapshot, which also restores branch anchors after live
  events. A failed branch is never replayed automatically.
- **Register Source…** in New Session takes a configuration path relative to
  the server's workspace plus an optional remote working directory. Nothing
  is read from this device, and servers without the route say so.
- **New Session** has an optional "Working directory on server" field; empty
  starts without a workspace.
- **Edit** in Recent Messages appears only for full (not shortened) user
  messages while the session is idle with nothing queued (`session_editing`).
  The sheet shows the original text. Saving changes the stored message only,
  and concurrent edits are last-write-wins. Errors keep the edited text and
  offer Reload History.

### New Session (sheet)

Server, agent (from `/api/agents`, with descriptions), optional title, model
only if allowed. Working directory is never a free-text local path; offer it
only if the server exposes workspace choices.

## States

- **Connection**: small toolbar status — Live, Reconnecting…, Offline — with
  last-updated time. On gap or epoch change, re-snapshot in place without a
  flash, preserving the scroll anchor.
- **Loading**: three or four skeleton transcript rows; no full-screen spinner.
- **Empty**: server without sessions shows "Start a session" and its agents; no
  servers shows a single Add Server action.
- **Errors**: unauthorized → Edit Server; unreachable → Retry; deleted session
  → terminal banner.

## Liveness and attention

- Continuous event streams run only for sessions that are open on screen.
- Needs You / Running are refreshed per server while the app is in the
  foreground (and on return to foreground); this is not a permanent stream.
- No permanent iOS background streaming is implied or designed. Background app
  refresh and local notifications are opportunistic. Reliable off-screen
  alerts would need server-side push, which is out of scope.
- The Dock/app badge reflects the last known Needs You count, labeled with its
  freshness where shown.

## Keyboard (Mac and iPad)

| Shortcut | Action |
| --- | --- |
| ⌘N | New session |
| ⌘⏎ / ⏎ (Mac) | Send (queues while running) |
| ⌥⏎ | Steer the current turn |
| ⌘. | Stop turn |
| ⌘L | Focus composer |
| ⌥⌘I | Toggle inspector |
| ⌘1 – ⌘5 | Inspector Agents / Todos / Safety / Tools / Session |
| ⌘R | Rescan (Review) |
| ⌥⌘J | Toggle conversation/draft panel (Review) |
| ⌘F, ⌘G, ⇧⌘G | Find in file, next, previous |
| ↑ ↓, ⇧↑ ⇧↓, ⏎, Esc | Move / extend line selection, comment, clear |
| ⌘[ | Parent folder (Files) |
| Y T N R | Allow once / always / deny / deny with reason — only while an approval card has focus |

Planned: ⌘⇧A next Needs You item, ⌘K command palette.

Slash-command equivalents live in "Session" and "Agent" menus on Mac.

## Visual system

- **Type**: system fonts with full Dynamic Type. Title weight only for session
  titles; agent headers subheadline semibold; metadata footnote secondary. SF
  Mono only for commands, paths, code and diffs.
- **Color**: semantic system colors plus one app tint (Docker-adjacent blue,
  e.g. #1D63ED, tuned for light/dark). Agents get a fixed 8-hue palette
  assigned deterministically by agent name per server, used only for dots,
  small capsules and a 2pt gutter — never fills behind text. Status uses system
  green/orange/red, always paired with a symbol or label.
- **Material**: Liquid Glass only via standard toolbars, sidebars, sheets and
  the composer background. Content stays opaque.
- **Spacing**: 4pt grid; 12pt between rows, 20pt between turns, 16pt card
  insets; continuous corners 12pt (cards), 8pt (chips).
- **Motion**: short and subtle; streamed text appends without per-token
  animation; delegation dividers fade in; activity strip cross-fades. Respect
  Reduce Motion.
- **Accessibility**: VoiceOver labels include the agent ("coder ran git status,
  succeeded"); interaction cards are announced; 44pt minimum targets on iOS;
  contrast passes in both appearances.

## Platform adaptation

- **Mac**: three columns plus inspector, menus, Settings scene, multiple
  windows/tabs, hover reveals for cost and actions.
- **iPad**: three columns in landscape; two columns in portrait with the
  inspector as overlay; hardware keyboard shortcuts; Stage Manager windows.
- **iPhone**: stack navigation, inspector as detent sheet, full-width
  interaction cards with large buttons, swipe actions in the session list (Star,
  Delete with confirmation).

## View implementation map

Designer owns `apple/Sources/DockerAgentApp/Views/**`; Engineer owns the
package, `AgentKit`, `DockerAgentApp.swift`, `AppModel.swift`, tests and build.
Views read `AppModel` projections only — no wire events, cursors or tokens.
The compiled declaration contract published by Engineer is authoritative.

```
Views/
  Root/      RootView, SessionWindowView, SidebarView
  Servers/   ServerEditorSheet, ServerRow, ServerSettingsView
  Sessions/  SessionListView, SessionRow, NewSessionSheet
  Session/   SessionView (+ ConversationPane), TranscriptView, TranscriptRows,
             ActivityStrip, InteractionCards, ComposerView, PendingSendsView,
             SessionInspector
  Review/    ReviewWorkspaceView, ReviewNavigator, DiffPane, DiffLinesView,
             DiffModel (split pairing, anchor builder, limits), CommentComposer,
             ReviewDraftPanel
  Files/     FilesBrowserView
  Design/    Theme (metrics, agent palette, formatting), AgentBadge, StatusBadge
```

View-local state (disclosure expansion, scroll follow, inspector tab, composer
draft) is keyed by stable projection IDs and never stored in `AppModel`. The
composer keeps its draft until a send is accepted.

## Implementation roadmap

1. **Phase 1**: servers (add/test/edit/remove, health); catalog (paged) and
   Needs You; open session (snapshot, SSE, resume, epoch/gap reset);
   transcript anatomy; Send, Steer, Stop, Retry; approvals, elicitation,
   max-iterations; new session with agent picker; inspector Agents and Usage;
   first-class Git review workspace with anchored draft comments; read-only
   Files; durable pending sends.
2. **Phase 2 (views built, capability-gated)**: queue editing, Todos,
   model/effort switching, title edit, star, Compact, branch, switch agent.
   Safety/permissions, tools, agent details, attachments and generated media
   are also built.
3. **Phase 3 (views built)**: continue from a user message, bounded history
   edit, workspace restore, MCP prompts and fork skills, context breakdown,
   JSON export and confirmed deletion, pause, stop subtree, waits, delegation
   policy, source registration.
4. **Out of scope**: plans, board and evaluation have no public content routes
   for this client. The `plan_changed` notification carries no plan content,
   so there is nothing faithful to render. Team and config editing stay out
   until the backend exposes a public contract.

## Scope and status

- **API contract gating.** Interaction specifics here (approval options,
  elicitation forms, queue/steer, agent switching, model/effort,
  compaction, Stop Subtree, attention fields) describe intended UX. Each must be
  confirmed against the actual API contract and server-reported capabilities
  before it is built; where the contract differs, the contract wins and the UI
  adapts.
- **Review and Files scope.** Both use only the existing read-only
  `git-review` routes; review drafts are client-side and sent as ordinary
  messages. Known server gaps (combined conflict diffs, directory paging, no
  atomic send-time freshness check) are shown honestly, not worked around.
  Team and config editing are not a remote affordance. The only
  source-related action is registering an existing server-workspace path.
- **Implemented, not shipped.** Sections marked "implemented" describe views
  present in the source tree. They are not a release or a claim of full TUI
  parity; the remaining gaps are listed in each section and in
  `API_CONTRACT.md`.
- **Transport and liveness policy** are as stated above: HTTPS by default with a
  strict, explicit local-development HTTP exception; continuous streams only for
  open sessions; foreground per-server attention refresh; no implied permanent
  iOS background streaming.
- **Verification status.** The views are checked by macOS builds, iOS 26
  typechecks and helper unit tests, plus ImageRenderer checks of a few
  transcript rows. Running layouts on device or simulator, VoiceOver, Dynamic
  Type at extreme sizes and live-server flows have not been visually
  validated.
