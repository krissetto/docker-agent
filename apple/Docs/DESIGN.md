# docker-agent for Apple platforms — Design Blueprint

Native SwiftUI client for iPhone, iPad and macOS that connects to one or more
user-added docker-agent API servers (`docker agent serve api`). It is a native
product, not a terminal clone or web view.

Status: design blueprint with the current implementation and bounded gaps
recorded in [Scope and status](#scope-and-status).

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

### iPhone, iPad and Mac

The primary hierarchy is **active server → sessions → session detail**. The
home is a workbench with two parts that never overlap:

- **Board** — every root session that is live right now, as a team tag:
  title and age, the root agent with its current activity (or the reason a
  person is needed) and cost, then the agents under it as hue dot + name +
  state word. Sub-agents come from the live `agentTree` while the session is
  open and otherwise from child sessions in the catalog; neither is presented
  as the complete hierarchy. "Needs you" tags come first with a Signal spine;
  "Working" tags follow. Idle servers show one sentence, "Nothing is running
  on <server>", never a row of zero counts.
- **Log** — everything else, newest first, grouped Today / Yesterday / This
  week / Earlier, as quiet rows (title, agent, cost, age).

A toolbar server menu switches the active profile and exposes connection
state, session count, needs-you count, add, manage and reconnect actions. The
toolbar's More menu narrows the board (Everything / Needs you / Working),
toggles starred-only and enters selection. Search covers loaded sessions and
shows plain rows. Per-window active server and last selected session are
remembered independently. Server settings remain available from the profile
menu and the macOS Settings scene.

"Needs you" is only ever derived from server facts: an open interaction,
pending input, a reported error or a failed run. A `waiting` state on its own
means waiting on an agent, a tool or a pause and is shown as "Waiting".

New Session is a direct action on the active server. It uses that server's
reported default source and root agent, plus its workspace root. A source key
and its configured root-agent name are separate identities; clients never
invent a `root` name or infer a default from catalog order. If a server has no
identified default and more than one source, the action reports that an agent
team must be chosen in **New Session Options…**. Options retains source
registration, explicit team selection, title and server working-directory
fields; a successful explicit choice is saved per server profile.

On wide Mac/iPad layouts, `NavigationSplitView` places the workbench beside
the session detail and its `.inspector`. On iPhone the same hierarchy
collapses into a native navigation stack. With no profiles, show a Connect a
Server empty state with an Add Server action; do not open setup automatically.

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

1. **Team line**: owning agent, current activity (or "Needs you" when an
   interaction is open), one overlapping dot per live sub-agent in its hue
   with the count, a progress indicator while running, and Stop. Tap opens
   the Agents inspector. Hidden when idle.
2. **Interaction cards**: inline, not modal; they stack; each is agent-owned
   and drawn in the board-tag language with a Signal spine — no stroke, no
   shadow.
   - Tool approval: agent, tool, short argument preview with Details. Actions:
     Allow Once (prominent), Deny, Always Allow This Tool, More (Deny with
     Reason, Balanced Mode, Allow All Tools — labeled as session-wide). On
     narrow screens the four controls sit in two rows of two.
   - Like the TUI (default "No"), no global or blind keystroke approves. On Mac,
     when a card has focus, TUI letters work: Y, T, N, R.
   - Elicitation uses native form fields; max-iterations gets Continue/Stop.
   - Cards resolve on `interaction_resolved`, with a brief "Answered
     elsewhere" fade when another client answered.
3. **Composer**
   - One leading "+" menu: Photos, Files, Prompts & Skills (when supported).
   - Multiline field; placeholder names the agent ("Message root…", "Queue for
     root…" while a turn runs).
   - Send submits (queues while a turn runs). Stop lives on the team line,
     next to the work it stops; ⌘. always stops. Steer is a secondary action
     on Send (press and hold / right-click, ⌥⏎).
   - On iPhone the Conversation / Review / Files picker sits at the top of the
     session menu (⋯) so the title keeps its width.
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

- **Agents**: live subagent tree with an accessible outline and a pan/zoom GPU-
  composited Canvas map (name, status, attention marker, cost). Opening a child
  requires explicit confirmation, then uses prepare-info and open-view to
  validate identity and ancestry; no new work is started.
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
- **New Session Options…** retains an optional "Working directory on server"
  field; empty uses the server's configured workspace when one is available.
- **Edit** in Recent Messages appears only for full (not shortened) user
  messages while the session is idle with nothing queued (`session_editing`).
  The sheet shows the original text. Saving changes the stored message only,
  and concurrent edits are last-write-wins. Errors keep the edited text and
  offer Reload History.

### New Session

The primary New Session action immediately starts with the active server's
resolved default source and actual `root_agent_name`, rooted at `workspace_root`
when supplied. **New Session Options…** exposes source registration and explicit
team selection for ambiguous multi-source profiles, plus optional title and a
server-host working directory. It never submits a client-local path or guesses
an agent named `root`.

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

The spine is the team. One structural device — a 3pt bar at the leading edge
in the owning agent's hue — carries who (colour) and state (Signal orange when
a person is needed, full when live, 40% when quiet). Everything aligns to one
text edge beside it. Cards appear only where something asks for a decision
(interaction cards), where live work is shown (board tags) or where you spoke
(user turns); history and transcript rows have none.

- **Type**: system fonts with full Dynamic Type. `headline` for board-tag
  titles, `body` medium for log rows, `body` for prose, `callout` for tool rows
  and controls, `footnote` for the agent/activity line, `caption` for meta. SF
  Mono only for commands, paths, code and diffs; monospaced digits for cost,
  tokens and ages so numbers line up as data. No all-caps labels.
- **Color**: two owned colors — Harbor `#1D63ED`, the app tint (you, your
  sends, running state), and Signal (system orange) for attention only. All
  other colors are semantic system colors. Agents get a fixed 8-hue palette
  assigned deterministically by agent name per server, used for the spine,
  dots and small capsules — never fills behind text. Red is failure.
- **Words**: state vocabulary shared by board, team line, team map and widget
  — Needs you, Working, Waiting, Idle, Done, Failed — in sentence case. Idle is
  said once in words, not as zero counts.
- **Material**: Liquid Glass only via standard toolbars, sidebars, sheets and
  the composer background. Content stays opaque.
- **Spacing**: 4pt grid; 12pt between rows, 20pt between turns, 12pt tag
  insets; continuous corners 12pt (tags, cards), 8pt (chips), 16pt (decision
  cards).
- **Motion**: short and subtle; streamed text appends without per-token
  animation; team-line activity cross-fades; disclosure answers a tap. Respect
  Reduce Motion.
- **Accessibility**: VoiceOver labels include the agent ("coder ran git status,
  succeeded"); board tags read title, agent, state, team size and cost as one
  element; interaction cards are announced; 44pt minimum targets on iOS;
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
  Sessions/  SessionListView (board + log), BenchBoard (BoardEntry, BoardTag,
             LogBucket, FlowLayout), SessionRow, NewSessionSheet
  Session/   SessionView (+ ConversationPane), TranscriptView, TranscriptRows,
             ActivityStrip (team line), InteractionCards, ComposerView, PendingSendsView,
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

- **Native WidgetKit**: separate iOS/iPadOS and macOS widget builds share a
  minimal App Group snapshot of aggregate agent/active/attention counts,
  session/server counts, and freshness time. macOS offers systemSmall and
  systemMedium only. No credentials, transcript text, session titles or prompts
  enter the snapshot. The app publishes during foreground life on a bounded
  five-minute cadence; stale/no-live/container-unavailable states are explicit.
  Distribution profiles must enable the fixed App Group entitlement.

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
