# docker-agent for Pebble Time 2 — UX spec

A small native watchapp that lets you check on and talk to a docker-agent
root agent from the wrist. It is a remote client of `docker agent serve api`,
reached through the official Pebble phone app. The transcript and the
"does it need me?" signal matter most. Everything else is out of scope.

Status: design handoff. Capabilities marked **gated** must be confirmed on
hardware or with the SDK before they're built (see [Capability gates](#capability-gates)).
This spec follows the Apple client design (`apple/Docs/DESIGN.md`):
server-honest affordances, agent ownership shown everywhere, glanceable
status.

## Scope

In scope:

- Select remote runtimes (servers) that were added on the phone.
- Pick an explicit root agent. Show its sessions with recent history,
  status and attention, and a chat transcript.
- Compose with voice or canned replies. Voice always ends in transcript
  review and an explicit Send.
- Haptic alerts, with optional sound, for completed replies and new
  attention.

Approvals, questions and other interactions are **read-only by scope**.
The watch shows them and says where to answer them.

Out of scope: Git or diff review, files, answering approvals or
elicitations, Stop, Steer, model or effort
controls, team editing, speech playback, offline outbox, and background
alerts while the app is closed.

## Principles

1. **Explicit target, always visible.** Every chat and send names the
   runtime and root agent. A stored selection is only restored after the
   server confirms it. If the agent has gone away, the app says so and never
   falls back to the server default.
2. **Nothing fake.** A control is actionable only when it works now. An
   expected input that is missing (Voice) stays visible and opens a concrete
   explanation. Nothing pretends to work.
3. **Honest freshness.** Cached data always shows its age. Offline,
   reconnecting and unauthorized each have their own wording.
4. **Buttons first.** Every flow can be completed with Back/Up/Select/Down.
   Touch mirrors the buttons. It is never required.
5. **Reads at a glance.** Use high-contrast colour bands with text plus a
   glyph, short lines and large type.

## Capability gates

| Capability | Status (from scouting) | Design stance |
| --- | --- | --- |
| Colour display, 200×228 (`emery`) | Target | Primary layout. Use 64-colour `GColor` only, with no dithered tones. |
| Phone bridge | Verified: AppMessage ↔ PebbleKit JS (PKJS) in the official Pebble app. Native PebbleKit iOS doesn't bridge through it. | PKJS only. No native or custom companion. |
| Runtime sign-in | **Experimental companion feature.** The bundled, app-owned config page (`data:text/html`, no remote scripts) sends to PKJS with `Pebble.sendMessage`. The bridge exists only in current public source: plugins default off, and no App Store release is verified. iOS gives no origin check. | Probe first and fail closed. The token lives **in PKJS RAM only**. It is lost whenever the watchapp stops, the app changes, the connection drops or the runtime is lost. It is never persisted, never in a URL, never sent to the watch. |
| Dictation API | Available: public C `DictationSession`, with built-in confirm/retry UI. Needs the companion app and phone Internet. | Voice compose uses it with system confirmation **on**, then the app's Review and explicit Send. |
| Dictation ready for this user | **Gated**: speech provider activation in the official app is unverified | Voice stays listed. Readiness is learned from the first attempt's status, never assumed. |
| Microphone | Present | Only through Dictation. No raw audio. |
| Speaker | Verified in SDK 4.33.1: `PBL_SPEAKER`, `speaker_play_notes` / `speaker_play_tone`, `speaker_is_muted`. Engineer verifies it compiles. | Optional alert tones ([Alerts](#11-alerts-preference-phone-settings-watch-shows-current-value-in-status-card)). No speech. |
| Touch | Verified in SDK 4.33.1: `PBL_TOUCH`, `touch_service_subscribe`, `app_touch_navigation_enable`. Engineer verifies it compiles. | Real secondary input that mirrors the buttons ([Touch](#touch)). |
| App glance in launcher | **Gated** on firmware | Optional subtitle "2 need you · 4m". |
| Background execution | Not available (JS runs only while the app is open) | No claims of background alerts. |

## Architecture boundary (UX-relevant)

```
Watch C app  ⇄ AppMessage ⇄  PebbleKit JS (official Pebble app)  ⇄ HTTPS ⇄  docker-agent API
 renders view models          owns HTTP, bounded polling,
 owns drafts, buttons         projection to compact text
```

- **Runtimes are added and signed into on the phone.** They are two
  distinct steps.
  - **Configured endpoint** (persisted, not secret): name, HTTPS base URL
    and auth mode.
  - **Signed in while the app is open** (PKJS RAM only, experimental): the
    bearer token entered in the bundled config page's password field.
    **Test connection** (`GET /api/v2/server`) must pass before it is used.
  - The token is lost on every watchapp exit or relaunch, app switch,
    phone reconnect or PKJS loss. Each token runtime then shows **Sign in on
    phone** until the token is entered again. This is expected, not an
    error.
  - The config page is bundled and app-owned, with no remote scripts. That
    is the trust boundary, because the iOS bridge doesn't check origin.
- **No-auth mode is a deliberate per-runtime choice.** In the config page it
  is labelled "No sign-in (trusted network only)" and needs a confirming
  checkbox. The watch marks those runtimes `open`.
  - A missing or rejected token never falls back to no-auth.
  - A no-auth runtime that answers 401 shows Unauthorized.
- **Sign-in not supported**: if the probe fails (plugin off, older
  companion, missing bridge), the config page shows "Sign-in isn't
  supported by this Pebble app" with no token field, and the watch shows the
  same reason. Only configured `open` runtimes stay usable.
- HTTPS is required, except for the Apple client's explicit local-dev
  exception. Canned replies and the alert preference are ordinary
  persisted settings.
- The watch receives display-ready text: Markdown stripped, code blocks
  replaced with `‹code · 12 lines›`, and tool calls reduced to one-line
  summaries.
- Sends go to `POST /api/v2/sessions/start` (new chat, `agent_name`
  explicit) or `POST /api/v2/sessions/:id/messages`. They carry a
  `request_id` that the watch generates once per draft. Retries reuse it, and
  the app never retries automatically.

## Button grammar

| Button | Lists | Chat | Reader / Review |
| --- | --- | --- | --- |
| Up / Down | Move highlight | Move between turns (older ↑) | Scroll |
| Select | Open | Open turn in Reader. On the **Reply** row: Compose | Open the actions menu (Review: Send / Re-record / Discard) |
| Hold Select | — | **Voice reply** (or its unavailable card) | — |
| Hold Down | — | Status card | — |
| Back | Up one level | Up one level (draft kept) | Back (draft kept) |

Don't bind long-press Back, because the system reserves it. Every
hold-shortcut also has a visible menu path.

## Navigation

```
Runtimes ─► Root agents ─► Sessions ─► Chat ─┬─► Reader (one turn)
   │                          │              ├─► Compose ─► Voice ─► Review ─► Sending
   │                          └─ New chat ───┘           └► Canned ─► Review
   └─ Needs you (all runtimes) ─────────────► Chat (attention card on top)
```

On launch, the app restores the last chat only after the phone confirms the
runtime is signed in (or `open`) and confirms the agent (`/api/agents`) and
the session. Token runtimes always need sign-in after a relaunch, so restore
stops at Runtimes and shows that runtime's sign-in card. While checking, the
app shows the Runtimes list with a "Restoring…" row. If any of the three fails, the app
stops at the level that failed and shows a state card there. For example:
"planner isn't on Home lab anymore · Pick an agent".

## Visual system

**Canvas.** 200×228. Margins are 6 px horizontal and 4 px vertical, and row
insets are 8 px. Leave room for `ACTION_BAR_WIDTH` where an action bar is
used. Content surfaces are white for daylight legibility on the reflective
display. Black is used only for the offline band and overlays.

**Type.** System fonts only:

| Role | Default | Large content size* |
| --- | --- | --- |
| Screen title / band | `GOTHIC_18_BOLD` | `GOTHIC_24_BOLD` |
| Body / transcript | `GOTHIC_18` | `GOTHIC_24` |
| Card headline (status) | `GOTHIC_24_BOLD` | `GOTHIC_28_BOLD` |
| Meta (time, agent, age) | `GOTHIC_14` | `GOTHIC_18` |

\* From `preferred_content_size()` when it is Large or larger. No text is
ever smaller than Gothic 14. Keep content to 2–3 lines per list row, and
truncate with "…" on a word boundary.

**Colour tokens.** Every pair below is ≥ 6.7:1, except the accent at 5.6:1,
which is used only with bold text.

| Token | GColor | Text on it | Use |
| --- | --- | --- | --- |
| `surface` | White | Black | Content |
| `meta` | DarkGray (555555) | on white | Times, agent names, tool lines |
| `user-turn` | LightGray (AAAAAA) | Black | Your messages (fill) |
| `working` | CobaltBlue (0055AA) | White | Running band, highlights |
| `attention` | ChromeYellow (FFAA00) | Black | Needs-you band, badges |
| `ok` | IslamicGreen (00AA00) | Black | Accepted, healthy dot |
| `error` | DarkCandyAppleRed (AA0000) | White | Failed, rejected |
| `offline` | Black | White | Phone/runtime unreachable |
| `stale` | DarkGray | White | Cached, reconnecting |
| `accent` | BlueMoon (0055FF) | White, bold only | Menu highlight |

Agents get a deterministic hue from an 8-colour subset (PictonBlue,
Malachite, Orange, Magenta, VividCerulean, Yellow, Purpureus, Red), assigned
by name. The hue is used only for a 3 px left gutter and a 6 px dot, never
behind text. Status colour always comes with a glyph and a word:
`● Working`, `! Needs you`, `× Failed`, `✓ Done`, `○ Idle`, `⌁ Offline`.
The hues need a hardware check, because saturated colours may render darker
on the reflective panel.

**Glyphs** are 12–16 px icon resources, not font characters, because the
system Gothic fonts can't be relied on for symbols or emoji. **Meta text on
a filled row or band** takes that fill's text colour (black or white), never
`meta` grey; the mockups showed grey disappearing on blue and yellow.

**Status band** (top 20 px, replaces the system status bar inside chat):
`[glyph] State · elapsed      14:02`. The runtime/agent label sits directly
under the band in the chat header.

## Screens

Wireframes are about 26 characters wide and are not to scale. Approximate
native-size mockups are in [`assets/screens.svg`](assets/screens.svg) and
`assets/screens.png` (rendered at 2×). They show Chat, Voice unavailable,
Review and Offline. They use stand-in fonts and glyphs, so they check
layout and contrast only. Hardware screenshots should replace them.

### 1. Runtimes (home)

```
┌──────────────────────────┐
│ docker-agent        14:02│
│ NEEDS YOU                │
│▌! planner · Release prep │  yellow badge, sorted newest
│▌  approve shell · 3m     │
│ RUNTIMES                 │
│ ● Home lab        2 run  │  green dot = healthy (last poll)
│ ⚿ Work VM   sign in ›    │  configured, needs sign-in on phone
│ ● Pi        open · idle  │  deliberate no-auth runtime
│ ＋ Add on phone          │
└──────────────────────────┘
```

- The **Needs you** section shows only when it has items. It covers
  runtimes polled in this app run. The header says "as of 14:01" when the
  newest data is older than 60 s.
- **Add on phone** opens an instruction card: "Open the Pebble app ›
  docker-agent › Settings to add a runtime." There is no fake "open phone"
  action.
- A **Sign in on phone** row opens a card: "Sign in to Work VM in the Pebble
  app › docker-agent › Settings. Sign-in lasts only while docker-agent
  stays open on the watch." These runtimes aren't polled and are left out of Needs you,
  which adds a footer such as "1 runtime needs sign-in".
- Empty state (no runtimes) is a single card with the same instruction and
  a phone glyph.
- Selecting a row that is offline or unauthorized opens that runtime's state
  card ([States](#connection-and-error-states)), not an empty list.

### 2. Root agents

Header: runtime name. Rows show `● agent name` (agent hue) and a one-line
description from `/api/agents`. Every agent is picked explicitly, even when
there is only one: the screen still shows it, with the row pre-highlighted.
There is no auto-skip, so the user always sees who they'll talk to. Empty
state: "No agents on this runtime".

### 3. Sessions (for runtime + agent)

```
┌──────────────────────────┐
│ planner · Home lab       │
│ ＋ New chat              │
│▌! Release prep       3m  │  yellow gutter = needs you
│▌● Fix flaky test  ·  run │  blue = running
│  ✓ Docs pass        2h   │
│  ⧗ Old audit   history   │  history-only, grey
└──────────────────────────┘
```

Groups are ordered Needs you → Running → Recent, with up to 10 recent rows.
The last row is "Older sessions on phone or desktop" (non-selectable meta
text), unless paging gets built. Titles come from the server; until one
exists, the row shows the first words of the first message.

### 4. Chat

```
┌──────────────────────────┐
│● Working · shell · 1m 14:02│  status band (blue)
│ planner · Home lab       │
│▌You                      │  grey fill
│ ship the release notes   │
│▌planner                  │  agent gutter
│ Drafted notes for 1.4.   │
│ Two items need a version…│
│  ⚙ shell · ok · 4s       │  meta line
│  ↳ via coder             │  subagent ownership
│ ─────────────────────────│
│ ◉ Reply…  hold ◉ = voice │  pinned last row
└──────────────────────────┘
```

- The first (oldest-end) row repeats the state as `ⓘ Status · Working`
  and opens the Status card. This is the visible path for Hold Down.
- The chat opens at the newest content with the **Reply** row
  highlighted. Up walks back through turns. Older turns load lazily
  ([Resource budget](#resource-budget)), up to 30 turns back, then
  "Earlier turns on phone or desktop".
- Turn bodies are capped at 6 lines in the list. Select opens the **Reader**
  (a scroll view of up to about 2 KB, then "Continues on phone").
- **Ownership** is shown as an agent header only when the owner changes.
  Subagent work reads `↳ via coder`, and tools are one meta line each.
  Reasoning is not shown.
- **Attention card** is pinned under the band while open, in yellow:
  `! Needs approval: shell "rm -rf build"` / `Answer on phone or desktop`.
  It has no buttons. Questions, max-iterations and failures use the same
  card with their own wording. The card disappears when the interaction
  resolves, with a brief "Answered elsewhere" if it was answered elsewhere.
- **Reply row** variants are honest about what's possible:
  - `◉ Reply…`
  - `◉ Reply…` with a meta second line, "Queues after current turn"
    (while running; it doesn't fit on one line)
  - `History only · <history_reason>` (non-selectable, when
    `execution_available` is false)
  - `Can't send · phone offline`
  - `Draft ready · Review` (a kept draft)
- **Live update keeps the reading position stable.** The highlight and
  scroll offset never move on their own.
  - If the Reply row is highlighted, new turns are inserted above it and
    the view stays bottom-anchored.
  - Otherwise a `▼ 1 new` chip appears at the right of the band. Down, or
    tapping the chip, jumps to it.
  - Text changes once per poll batch, never per token. Nothing animates.

### 5. Status card (Hold Down, or Select on the chat's top row)

A full-screen colour card showing the state word in `GOTHIC_24_BOLD`, the
current activity ("shell · 1m 12s"), active subagents ("+2 agents"),
pending inputs ("1 queued"), the last error (2 lines max), and "Updated
14:01:52". It is read-only. Back closes it.

### 6. Compose chooser

A MenuLayer titled "Reply to planner":

1. `🎤 Voice` — always listed. When it's known to be unavailable, the row
   reads `🎤 Voice · unavailable` in meta grey. Select (or Hold Select in
   Chat) then opens the **Voice unavailable** card instead of dictation.
2. `💬 Canned replies`

The Voice unavailable card has a red band and a concrete reason, with
**Use canned reply** as the first action and **Try voice again** where
retrying can help:

| Known cause | Reason text | Try again? |
| --- | --- | --- |
| Phone link down | "Voice needs your phone connected" | Shown after reconnect |
| `FailureDisabled` on an attempt in this run | "Voice isn't set up or is off in the Pebble app" | Yes |
| `FailureConnectivityError` | "Voice needs Internet on your phone" | Yes |
| Platform has no microphone | "This watch has no microphone" | No |

Readiness is never inferred before an attempt. On first use, Voice opens
dictation and the outcome decides its state.

### 7. Voice flow (state machine)

```
Ready ─Hold Select / Voice─► Listening* ─► Transcribing* ─success─► Review ─Send─► Sending ─► Accepted | Queued
                                │                │                    │  └─Re-record─► Listening
                              Back*            error ──────────────► Voice error ─Retry─► Listening
                                ▼                                     └─Discard─► Chat
                             Cancelled (back in Chat, no draft)
```

\* **Listening and Transcribing are system UI** (`dictation_session_start`).
The app doesn't draw them.

- Keep the system confirmation and error dialogs **on** (SDK defaults). The
  system step checks what was heard. The app's Review then names the target
  and requires an explicit Send. A success callback never submits anything.
- After a failure status, the app's card follows any system dialog. It
  offers the next step (Retry, Canned, Back) rather than repeating the
  system message.
- **Cancelled** (user Back in system UI, or `FailureSystemAborted`): return
  to Chat silently, with the previous draft (if any) untouched.
- **Voice error card** (red band):

  | Status | Headline | Actions |
  | --- | --- | --- |
  | `FailureNoSpeechDetected` | "Didn't hear anything" | Retry · Canned · Back |
  | `FailureTranscriptionRejected*` | "Couldn't transcribe" | Retry · Canned · Back |
  | `FailureConnectivityError` | "Voice needs phone Internet" | Retry · Canned · Back |
  | `FailureDisabled` | "Voice isn't set up or is off in the Pebble app" | Canned · Try again · Back (Voice row marked unavailable) |
  | `FailureInternalError` / `FailureRecognizerError` | "Voice failed" | Retry · Canned · Back |

  Retry is offered only for statuses where a retry can succeed.
- Before starting, if the phone link is down, show "Phone disconnected"
  directly instead of opening dictation.

### 8. Review (mandatory for voice and canned)

```
┌──────────────────────────┐
│ Review · to planner      │  accent band
│ Home lab · Release prep  │  meta
│                          │
│ Ship it once the version │  GOTHIC_24 transcript,
│ table matches 1.4.       │  scrolls with Up/Down
│                          │
│        ◉ Send options    │  bottom hint
└──────────────────────────┘
```

- Select opens an `ActionMenu` with `Send to planner` first, then
  `Re-record` (or `Choose another` for canned), then `Discard`. **Sending
  takes two deliberate presses and names the target.** There is no
  countdown, auto-send or send-on-hold.
- For a new chat, the target line reads "New chat · planner · Home lab".
- Back returns to Chat and keeps the draft (one draft per session, held in
  memory for this app run). Discard is the only path that deletes it.
- Transcripts are shown verbatim. They can't be edited on the watch; to
  change one, the user re-records.
- If the target becomes invalid while reviewing (offline, history-only,
  agent gone), the menu's Send item is replaced by a meta line giving the
  reason, and the draft is kept.

### 9. Sending

- **Sending**: the band reads "Sending…" (static, not animated) and the
  transcript dims. Back is disabled for at most 10 s.
- **Accepted** (`disposition` empty): back to Chat. The turn appears with
  `✓` in meta, and the draft is cleared.
- **Queued** (`disposition: "queued"`): the turn appears with
  `⧗ Queued · after current turn`.
- **Failed** (an error response): red card "Not sent · <short reason>" with
  Retry (same `request_id`) or Keep draft.
- **Unconfirmed** (timeout or link lost after the request left the phone):
  stale card "Not confirmed yet". Its action is "Check / Retry", which uses
  the same `request_id` and is therefore safe. The draft is kept until the
  send is confirmed.

### 10. Canned replies

Up to 12 entries from the phone settings, each 80 characters max and shown
on one line in the list. Defaults:

1. "Status update?"
2. "Continue."
3. "Summarize where you are."
4. "Pause after this step and wait for me."
5. "Looks good, thanks."

Choosing one goes to Review. None of them is an approval. Wording that reads
like a yes/no to a pending tool ("Yes, run it") is avoided in the defaults.
While an attention card is open, the chooser header adds: "Approvals can't
be answered by message".

### 11. Alerts preference (phone settings; watch shows current value in Status card)

The options are `Haptic` (default), `Sound + haptic`, and `Silent`.
`Sound + haptic` is offered only on `PBL_SPEAKER` builds and is never on by
default.

- Sounds are short tones only: two rising notes for attention, one note for
  a completed reply, and one low tone for a failed send.
- If `speaker_is_muted()` is true, the app plays haptics only, and the Status
  card shows "Sound: watch muted".

- Alerts fire for two events only: **new attention** (a new
  `interaction_id` or failure) and **reply completed** (a turn the user sent
  from this watch, or in the open chat, goes from running to idle or done).
- **Dedup** by interaction or turn ID for the app run. **Throttle** to at
  most one alert per 30 s per session and one per poll batch.
- No alerts on launch, restore, reconnect, gap re-snapshot or history
  backfill. A baseline is captured first, and only later changes alert.
- **Quiet**: skip all tones and vibration while `quiet_time_is_active()`
  (and DND, if exposed); the visual badge still updates.
- Patterns: attention uses a double short pulse; reply completion uses one
  short pulse; send failure uses one long pulse. Successful sends are
  silent, because the user is already looking.
- Alerts only happen while the watchapp is open. The settings page says this
  plainly.

## Connection and error states

Each state is a band in Chat and a full card when it blocks a whole level.
Cached content stays visible, and the age is always shown.

| State | Band / card text | Action |
| --- | --- | --- |
| Phone disconnected (`connection_service`) | `⌁ Phone disconnected · as of 14:01` (black) | Auto-resumes on reconnect. The Reply row shows the reason. |
| Bridge not ready (JS not running or settings never opened) | "Open docker-agent settings in the Pebble app" | — |
| Needs sign-in (configured, no token since this app launch) | `⚿ Sign in on phone` (grey, not red) | — |
| Sign-in unsupported (probe failed) | "Sign-in isn't supported by this Pebble app" | — |
| Reconnecting (1–3 tries, backoff 2/5/10 s) | `↻ Reconnecting… · as of 14:01` (grey) | — |
| Runtime unreachable | `⌁ Home lab unreachable` | Select: Retry now |
| Unauthorized (401/403) | "Token rejected · Sign in again on phone" (red). PKJS drops the token. | — |
| Polling paused (bound reached) | `Ⅱ Paused · as of 14:01` (grey) | Select: Refresh |
| Unsupported server (no v2 sessions, or version below the minimum) | "Home lab needs a newer docker-agent" | — |
| Insecure URL | "Blocked: runtime needs HTTPS" | — |
| Agent missing | "planner isn't on Home lab anymore" | Pick an agent |
| Session deleted | "Session was deleted" (terminal) | Back |
| History only | Reply row reads `History only · reason` | — |
| Loading | 3 grey skeleton rows (no full-screen spinner); "Still loading…" after 5 s | — |

Polling is finite (see Liveness below). When it stops, the bands say "as
of" instead of claiming to be live.

## Touch

Touch is used on `PBL_TOUCH` builds. Button behaviour stays the same, and
every touch action has a button equivalent.

- Use `app_touch_navigation_enable` for system components (menus and
  ActionMenu). Custom layers (band, chip, Review pill, cards) use
  `touch_service_subscribe` with hit-testing on full-width rects.
- Taps:
  - A row = highlight it, then Select.
  - The band = Status card.
  - The `▼ new` chip = jump to the new turn.
  - Review's "Send options" pill = open the menu (it doesn't send).
  - A vertical drag scrolls.
- **Sending still takes two taps** and the second names the target
  ("Send to planner"). No swipe, hold or gesture sends. Back stays a
  physical button. There are no touch-only or hold-touch actions.
- Targets are full width.
  - List rows are ≥ 36 px tall; the Reply row, Review pill and menu items
    are ≥ 44 px.
  - The band is 20 px tall, but its tap zone extends to 28 px.
  - The chip's tap zone is ≥ 44×28 px.
- **Tap guard**: if a row's position moved in the last 300 ms (new content
  or a layout change), ignore taps on it. This prevents taps landing on
  shifted rows.

## Accessibility

- Contrast tokens are listed above. State always has a glyph plus a word,
  never colour alone.
- `preferred_content_size()` drives the type scale. Rows grow, and truncation
  shrinks content rather than type.
- Haptic patterns are distinct per event, and Silent mode loses no
  information.
- The backlight follows the system (button presses). The app doesn't force
  it on.

## Resource budget

The display is 200×228. Below the 20 px band and 14 px header there are
about 190 px of content: 3–4 chat turns or 4 rows at 44 px. Only what's
visible plus one page in each direction needs to be in memory. Engineer
measures the actual binary size, static RAM and peak heap
(`heap_bytes_free` on each screen and in the worst-case chat). Treat a peak
above half the available heap as a defect.

| Item | Budget |
| --- | --- |
| Chat turns in RAM | ≤ 12, as two pages of 6. Turn preview text ≤ 512 B. |
| Older history | Fetched lazily, 6 turns per page, when the highlight reaches the top. Pages beyond ±1 from the view are dropped. Hard stop at 30 turns back. |
| Reader | One turn, ≤ 2 KB, freed when it closes |
| Lists | Runtimes ≤ 8, agents ≤ 12, sessions ≤ 10 + New chat, canned replies 12 × 80 B |
| Persisted on watch | ≤ 2 KB: last selection plus the last 3 turn previews, with `as_of` |
| AppMessage | JS sends only changed turns (`id` + revision). An unchanged poll sends nothing. Turn ages tick on the watch. |
| Redraw | Only changed layers are marked dirty, at most once per poll batch. Row heights are cached per turn revision, not measured during draw. The clock and elapsed time tick per minute (`MINUTE_UNIT`), with no seconds. Nothing animates. |

## Implementation suggestions (for Engineer)

- **Windows**: Runtimes, Agents and Sessions are `MenuLayer`s, each with a
  custom row `draw` for the gutter and badge. Chat is a `MenuLayer` with
  variable row heights from `graphics_text_layout_get_content_size`, plus a
  pinned Reply row and a custom 20 px band layer. Reader and Review use a
  `ScrollLayer` + `TextLayer` with paging. Review actions use `ActionMenu`.
  Status and error cards are a simple `Layer` with a colour fill. The window
  stack is at most 6 deep.
- **View models over AppMessage** are pre-projected by JS. A turn is
  `{id, owner, via, kind: user|agent|tool|attention, text≤512, meta}`, and
  long text is fetched on demand for the Reader. Use one outstanding request
  and chunk lists into `app_message_outbox_size_maximum()`-sized pages.
  Include `as_of` on every payload.
- **Liveness: finite, bounded polling. No streaming.** The compact route
  is pending backend work; until then use `/status` plus snapshot or cursor
  catch-up after a gap or epoch change.
  - Intervals: Chat every 4 s while running and 15 s while idle; session
    lists every 20 s; Runtimes and Needs you every 30 s.
  - Bounds: Chat polling stops after 10 min with no button press. Lists
    stop after 5 min. Errors back off 2/5/10 s, and after 3 failures the
    state becomes Unreachable. All polling stops on `app_focus` loss and
    resumes only on user action or focus return.
  - Paused shows its own state; Select refreshes.
- **Drafts and request IDs** live on the watch, keyed by
  runtime+agent+session (or `new`). The `request_id` is generated when Review
  is first shown and reused for every retry.
- **Persisted on watch**: only the last selection
  (runtime ID, agent name, session ID) and cached display text from the last
  open session for instant paint. These always carry their age. No tokens or
  URLs.
- **Fixtures**: build a preview for every state in this doc (runtime empty,
  offline, unauthorized, agent missing, history-only, working, needs-you,
  each voice error, queued, unconfirmed) before wiring the network.

## Open dependencies (coordinate with dev-team)

1. **Session sign-in is experimental.** It depends on a companion release
   that ships the bridge with plugins enabled. Until one is verified, expect
   most users to see "Sign-in isn't supported".
2. **Dictation readiness**: speech provider activation in the current
   official app.
3. **App glance** on PT2 firmware.
4. **Compact polling route** (backend) and the **minimum server version**
   for the "unsupported server" check.
