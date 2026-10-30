---
title: "Terminal UI (TUI)"
description: "Docker Agent's default interface is a rich, interactive terminal UI with file attachments, themes, session management, and more."
keywords: docker agent, ai agents, features, terminal ui (tui)
linkTitle: "Terminal UI"
weight: 10
canonical: https://docs.docker.com/ai/docker-agent/features/tui/
---

_Docker Agent's default interface is a rich, interactive terminal UI with file attachments, themes, session management, and more._

![Docker Agent TUI in action showing an interactive agent session](../../demo.gif)

## Launching the TUI

Both the full TUI and the lean TUI display a centered ASCII art Docker Agent banner on startup when the chat is empty. The banner is automatically hidden once the agent starts responding or when a custom welcome message is configured. You can disable the banner entirely from the **Appearance** tab of `/settings`, or by setting `show_banner: false` in your [user settings](../../configuration/user-settings/index.md).

```bash
# Launch with a config
$ docker agent run agent.yaml

# Start with an initial message
$ docker agent run agent.yaml "Help me refactor this code"

# Auto-approve all tool calls
$ docker agent run agent.yaml --yolo

# Enable debug logging
$ docker agent run agent.yaml --debug

# Override the application name shown in the status bar and window title
$ docker agent run agent.yaml --app-name "My Project"

# Preselect a color theme
$ docker agent run agent.yaml --theme dracula

# Hide the sidebar (cannot be re-enabled via Ctrl+B)
$ docker agent run agent.yaml --sidebar=false

# Disable specific slash commands
$ docker agent run agent.yaml --disable-commands="/cost,/eval,/model"

# Open in read-only mode to review a past session without sending new messages
$ docker agent run agent.yaml --session -1 --session-read-only

# Use the lean TUI for this run
$ docker agent run agent.yaml --lean
```

### Lean TUI

The lean TUI uses a simplified terminal interface with minimal chrome. To make it the default for interactive runs, set `lean` in your user config:

```yaml
# ~/.config/cagent/config.yaml
settings:
  lean: true
```

Omit `lean` or set it to `false` to keep the full TUI as the default. You can still use `--lean` for a single run, or `--lean=false` to use the full TUI when `settings.lean` is enabled. See [User Settings](../../configuration/user-settings/index.md) for the full precedence rules between flags and user config.

The lean TUI supports **steering** and **follow-ups** while the agent is running. Press <kbd>Enter</kbd> to steer the message into the active turn (or queue it when `busy_send_mode` is `queue`), or <kbd>Alt</kbd>+<kbd>Enter</kbd> to queue it as a separate turn after the current one finishes. Pending messages appear with muted styling at the end of the live stream, labeled `Steering:` or `Follow-up:` so it's clear which turn each one will land on. Press <kbd>Alt</kbd>+<kbd>Up</kbd> to withdraw every still-pending steer/follow-up (and any locally queued message) back into the editor, concatenated in the order they were sent, so you can edit and resend them; messages the agent has already picked up are left alone.

The lean TUI supports a focused set of slash commands: `/new`, `/sessions [session-id]`, `/load <session-id>`, `/delete <session-id>`, `/star [session-id] [off]`, `/compact`, `/model`, `/effort`, `/copy`, `/clear`, `/help`, `/exit` (alias: `/quit`), plus any agent-defined commands. `/sessions` fuzzy-browses past sessions from the current working directory, while the ID-oriented commands use session/server-owned catalog and mutation paths so remote sessions remain supported. Type `/model` (or `/model <provider/model>`) to open a fuzzy-searchable model list or switch inline. Type `/copy` to copy the last assistant response to the clipboard.

Prefix a message with `!` to run it as a shell command directly, without going through the agent — for example `!git status`. The command runs in your default shell and its output is shown inline in the transcript. Bang commands are disabled in read-only sessions.

## Slash Commands

Type `/` during a session to see available commands, or press <kbd>Ctrl</kbd>+<kbd>K</kbd> for the command palette:

| Command            | Description                                                                          |
| ------------------ | ------------------------------------------------------------------------------------ |
| `/new`             | Start a new conversation (usage: `/new [dir]`). With a directory — `~` and environment variables expand, relative paths resolve from the current session's working dir — the new session starts there; without one it reuses an explicit `--working-dir` default or opens the directory picker |
| `/clear`           | Clear the current conversation (keep session, drop messages)                         |
| `/compact`         | Summarize and compact the conversation history                                       |
| `/fork`            | Fork the current session into a new branch                                           |
| `/copy`            | Copy the entire conversation to clipboard                                            |
| `/copy-last`       | Copy only the last assistant message to clipboard                                    |
| `/undo`            | Restore file changes from the latest snapshot (only when snapshots are enabled)      |
| `/snapshots`       | List captured snapshots (only when snapshots are enabled)                            |
| `/export`          | Export the session as HTML                                                           |
| `/sessions`        | Browse and load past sessions                                                        |
| `/plans`           | Browse and manage every [shared plan](../../tools/plan/index.md). Filter, open a detail view, refresh, export to a file, set status, edit/create in `$VISUAL`/`$EDITOR`, and delete, all guarded against concurrent edits. Not available in the lean TUI |
| `/model`           | Change the model for the current session. Refreshing the picker's catalog is a separate server capability; older servers still reload their existing model list. |
| `/effort`          | Set the current model's reasoning-effort level (`/effort <none\|minimal\|low\|medium\|high\|xhigh\|max>`, or `/effort` alone to pick from the supported levels; reasoning models and `claude-code` / `command-code` harnesses only). Press <kbd>Tab</kbd> after `/effort` and a space to complete a level the current model supports |
| `/settings`        | Manage appearance, behavior, and notification preferences                           |
| `/yolo`            | Toggle automatic tool call approval                                                  |
| `/title`           | Set or regenerate session title                                                      |
| `/attach`          | Attach a file to your message                                                        |
| `/shell`           | Open a shell                                                                         |
| `/star`            | Star/unstar the current session                                                      |
| `/context`         | Show a context-window breakdown: estimated tokens per category (system prompt, tool definitions, prompt files, messages, tool results, compaction summary), a team-level **Live sessions** view (the current session plus every running sub-agent session with its agent, short session ID, and context budget), plus a per-file inventory of attached files and prompt files. When a compaction has occurred, the dialog also displays the verbatim text of the most recent compaction summary. When a dedicated `compaction_model` caps the effective limit below the primary model's own window, a second line reads "compaction cap: `<model>` • `<N>` tokens" — Live-sessions rows are silent on the cap, and the sidebar shows a minimal ⚠ capped marker without repeating the model or the figure (the `/context` header is the authoritative source of the model + number). Use the arrow keys to select a row: press <kbd>Enter</kbd> on a live session to explicitly compact it, or <kbd>d</kbd> on an attached file to drop it |
| `/drop`            | Remove an attached file from the session context (`/drop <path>`, or `/drop` alone to review and drop from the `/context` dialog). Press <kbd>Tab</kbd> after `/drop` and a space to complete an attached file's path |
| `/cost`            | Show cost breakdown for this session. Includes a **By Agent** section (alongside **By Model**) showing cumulative cost per agent; unattributed usage and compaction spend appear in their own buckets. |
| `/eval`            | Create an evaluation report                                                          |
| `/pause`           | Pause/resume only the current session at an iteration boundary. Other sessions continue independently. While this session is mid-request, the resize handle shows "Pausing…" until the in-flight request completes; once blocked it changes to "⏸ Paused". Run `/pause` again to resume. |
| `/tools`           | Show every toolset (with lifecycle state) and the tools they expose                  |
| `/skills`          | List skills available to the current agent                                           |
| `/toolset-restart` | Force a supervisor-driven reconnect of the named toolset (`/toolset-restart <name>`). Press <kbd>Tab</kbd> after `/toolset-restart` and a space to complete a toolset name; non-restartable toolsets are shown dimmed and cannot be selected. |
| `/permissions`     | Inspect and edit tool permission rules                                               |
| `/speak`           | Voice input via system speech-to-text (macOS only)                                   |
| `/exit`            | Exit the application (aliases: `/quit`, `/q`)                                        |

Slash commands (both built-in and named) execute immediately when entered. While the agent is working, <kbd>Enter</kbd> steers a regular chat message into the active turn, where it appears in the transcript when the agent picks it up (or queues it for the next turn when `busy_send_mode` is `queue`); <kbd>Alt</kbd>+<kbd>Enter</kbd> queues it for the next turn. Slash commands still execute immediately.

Selecting another top-level agent creates a fresh session identity bound to that agent and clones the current conversation into it. The original session and history remain available, and observers attached to it are not retargeted. Switching is rejected while the source session is active and from an attached sub-agent view.

Agent-defined commands (prompts, URL links, agent-switching shortcuts) are configured under `commands:` in the agent YAML — see [Custom Commands](../../configuration/commands/index.md) for the full reference, including how to hide commands with `--disable-commands`.

### Plans Sidebar

The optional **Plans** section is off by default. Enable it under `/settings` → **Appearance** → **Sidebar sections**, or in your global [user settings](../../configuration/user-settings/index.md):

```yaml
# ~/.config/cagent/config.yaml
settings:
  layout:
    show_plans: true
```

Plans are shared documents from the same store used by `/plans`, the [plan tools](../../tools/plan/index.md), and `docker agent plans` — not plans attached to the current session. A full left/right sidebar shows up to **five** plans, ordered by last update (newest first, unknown timestamps last, with name as the tie-breaker). Status is shown as free-form text; there is no active/completed classification or status filter.

- **Single left-click a plan row** to open its content directly in `$VISUAL`/`$EDITOR`, guarded by the displayed revision. A stale revision is rejected rather than overwriting newer content.
- **All plans** opens the shared plan browser. `/plans` and the <kbd>Ctrl</kbd>+<kbd>K</kbd> command palette remain the keyboard routes.
- Top/bottom layouts and narrow or collapsed sidebar bands show only a compact `Plans (N) - open /plans` count and browser shortcut, not individual plan rows. Lean mode and `--sidebar=false` never show the section.
- The bottom panel can show the same shared plans as a count, **N plans**, under **Settings → Panel → Plans** (off by default). Clicking it, or pressing Enter on it, opens the shared plan browser. It needs no sidebar, so it still works with `--sidebar=false`; lean mode never loads plan data.
- Changes from plan events in the current process and local edits refresh the shared metadata. To pick up changes from another process, use the sidebar's **Refresh plans** action or press <kbd>r</kbd> in the plan browser or detail view. There is no automatic polling or file watcher.

Editing changes only the plan document. It does not approve a plan, execute its steps, or authorize tool writes.

### Agents Panel

The sidebar's **Agents** section lists every agent in the team and has two display modes selectable via **Sidebar info mode** in `/settings`:

- **Compact** (default) — The current agent is shown as a focus **card** (rendered in place at its position in the list) with its name, a wrapped description, its full `provider/model`, and a thinking line. Every other agent is shown as a compact **two-line row** — line 1 is the shortcut/spinner, the agent name (in its accent color), and a right-aligned thinking **gauge**; line 2 is the indented full `provider/model` and, once the agent has run, its latest **context usage** as a right-aligned percentage of its context window.
- **Detailed** — Each agent is shown as a responsive card with labeled **Effort**, **Context**, and **Cost** metrics on a single line (or split across lines at narrow sidebar widths), making cumulative per-agent cost visible at a glance across the team.

Agents are separated by a blank line so rows stay visually distinct. The effort **gauge** is the only visual language for thinking; the focus card and the Agent Inspector spell out the exact level alongside it. Left-click any agent to switch to it.

On large teams the roster can be trimmed to the agents that matter right now: enable **Active agents only** (nested under **Agents** in `/settings` → Appearance → Sidebar sections, off by default) to list only agents active in the current session — the selected or working agent, participants of an in-flight transfer, and any agent with recorded participation (usage or attributed cost, including agents restored with a reloaded session). The filter also applies to the top/bottom band, is purely presentational — <kbd>Ctrl</kbd>+<kbd>number</kbd> shortcuts keep their original team positions and agent cycling still walks the whole team — and is unavailable while the Agents section itself is hidden.

#### Agent inspector

Open a read-only **Agent Inspector** to inspect any agent's full configuration combined with its live state. The instruction/system prompt is deliberately omitted; everything else the agent declares is shown:

- **Right-click any agent** (card or row) to open the inspector without switching to it.
- **<kbd>Ctrl</kbd>+left-click any agent** does the same — a fallback for terminals that don't forward right-clicks.
- **Left-click** always switches to the agent.

The title is rendered in the agent's accent color. Sections appear in this order, and any empty section is omitted:

- **Description** — the agent's wrapped description.
- **Live state** — a `● current agent` line when the inspected agent is the one currently running.
- **Model / Fallback / Thinking** — the `provider/model`, any fallback models, and the gauge + value thinking line (omitted for models with no selectable thinking, e.g. harness-backed agents).
- **Context** — the agent's latest known context usage, e.g. `Context: 12.8K of 128.0K tokens (10%)` (a bare token count when the context limit is unknown; omitted until the agent has run). Sub-agent and background-agent runs are accounted for. When a dedicated `compaction_model` caps the effective limit below the primary model's own window, the token-usage line also shows a short "⚠ capped" marker (see `/context` for the model and figure).
- **Cost** — the agent's cumulative cost across all runs in the session tree. Repeated session snapshots are not double-counted. Omitted until the agent has run.
- **Sub-agents (N) / Handoffs (N) / Skills (N)** — compact, inline, comma-separated lists wrapped to the dialog width.
- **Limits** — the configured per-agent limits that are set, e.g. `Limits: max-iter 50 · history 40 · max-tool-calls 5`.
- **Options** — the enabled option flags, e.g. `Options: add-date · add-environment-info · redact-secrets`.
- **Toolsets (N)** — one line per toolset with a status marker, its name, kind, and tool count, followed by the indented tool names.
- **Commands (N)** — the slash commands the agent defines, each with its description.

Each toolset carries a single-width status marker reflecting its **live** lifecycle: `●` started (serving), `○` stopped (not yet started), or `⚠` error. The tools listed under a toolset are the **live** tool names when it has started; for a toolset that has not started, the inspector instead shows its declared `tools:` allow-list prefixed with `declared:` (and shows nothing when the toolset declares no allow-list and therefore serves every tool). This lets you see both what an agent is configured with and what is actually running, even before the agent has been used.

The dialog scrolls when the content is long; press <kbd>Esc</kbd> to close it. Remote runtimes (which hold no local team config) degrade gracefully — the config-derived sections are simply omitted.

Model identifiers on line 2 are truncated **from the left** (e.g. `…claude-sonnet-4-6`) only when they overflow, so the informative tail (variant/version) is preserved. As the sidebar narrows the model keeps its own line, and near the minimum width line 1's gauge collapses to a single cell to keep the name readable.

The thinking state of each model is shown with a gauge + value on the card and a gauge or badge on the row (no `✻` glyph):

| Model state            | Card line                      | Row badge              |
| ---------------------- | ------------------------------ | ---------------------- |
| Effort level           | `thinking ▰▰▰▰▱▱ high`         | `▰▰▰▰▱▱` (effort gauge)  |
| Adaptive budget        | `thinking auto adaptive`       | `auto`                 |
| Token budget           | `thinking ◉ 8.2K tokens`        | `◉ 8.2K`               |
| Disabled (capable)     | `thinking ▱▱▱▱▱▱ off` (dimmed)  | `▱▱▱▱▱▱` (empty gauge)  |
| Not reasoning-capable  | _(omitted)_                    | _(omitted)_            |

The **effort gauge** is a fixed-width six-cell indicator (`▰` filled, `▱` empty) so the badge column stays aligned. It maps the six selectable levels one-to-one onto filled-cell counts — `minimal` → `▰▱▱▱▱▱`, `low` → `▰▰▱▱▱▱`, `medium` → `▰▰▰▱▱▱`, `high` → `▰▰▰▰▱▱`, `xhigh` → `▰▰▰▰▰▱`, `max` → `▰▰▰▰▰▰` — so the cell count alone is lossless, with a low→high color ramp as a secondary cue. A capable-but-disabled model shows a dim empty gauge (`▱▱▱▱▱▱` `off`), adaptive budgets show `auto`, and token budgets keep `◉ <count>`. The same gauge + value renders on the focus card, the Agent Inspector, and the row.

Harness-backed agents (e.g. `claude-code`) show the harness type as their model. `claude-code` and `command-code` harnesses show their `effort` on the gauge, or `auto` while the CLI picks it; other harnesses show no thinking gauge. Press **Shift+Tab** to cycle the current model's thinking-effort level; a `✻ Thinking: <level>` toast confirms the change (useful when the sidebar is hidden).

### Agent Delegation Feedback

When a parent agent calls `transfer_task` to delegate work to a sub-agent, the TUI provides live visual feedback in both the sidebar and the chat.

**Sidebar — Transfer box:** As soon as the delegation starts, an animated **Transfer** box appears below the agent roster, showing the direction of the handoff with a traveling dot:

```text
╭─ Transfer ─────────────────╮
│  parent  ●──────►  child   │
╰────────────────────────────╯
```

The box stays visible for at least 1.5 seconds. Once the sub-agent produces its first message, reasoning, or tool output, the box hides (still honoring the minimum window) to keep the sidebar focused on the active agent. If the sub-agent is slow or silent, the box hides after a 3-second maximum cutoff. The header shows a `↔` marker while any delegation is still in flight, even after the box hides.

**Sidebar — Return box:** When the sub-agent finishes and control returns to the parent, a brief **Return** box animates the reverse direction for up to 1.5 seconds, then disappears:

```text
╭─ Return ───────────────────╮
│  child  ●──────►  parent   │
╰────────────────────────────╯
```

**Chat — return transition:** Alongside the sidebar Return animation, the chat shows a one-line static transition naming both agents:

```text
[child]  returned control to  [parent]
```

This transition is not persisted — it does not reappear when you reload the session.

### Sidebar hover behavior

Pointer-addressable sidebar labels use a shared hover animation: muted at rest, emphasized while hovered,
and back to their resting color when the pointer leaves. This applies to the Files/Git,
Subagents, Todos, and Background summaries, model metadata, context usage, and cost.
Semantic warning, error, activity, and identity colors remain distinct. Clicking a
section does not latch its header into a bright or bold state; keyboard navigation
keeps its separate disclosure/cursor indication. The narrow collapsed sidebar band
has no individual todo-summary hover target and remains static.

### Context-Usage Gauge

The context percentage shown in the sidebar token-usage section, and the fill bar in the lean TUI status line, both color-escalate as the active session approaches the auto-compaction threshold:

| State   | Color  | Trigger |
| ------- | ------ | ------- |
| Normal  | (default) | Usage below 75% of the compaction threshold |
| Warning | Orange | Usage at or above 75% of the compaction threshold |
| Critical | Red   | Usage at or above 95% of the compaction threshold |

While a compaction is running the percentage is replaced by a **"compacting…"** indicator; token counts remain visible in the lean TUI status line.

The thresholds are proportional to the agent's configured `compaction_threshold` (default `0.9`), so a custom value keeps a predictable visual runway. See [Compaction Threshold](../../configuration/models/index.md#delegating-session-compaction) for configuration details.

Clicking the token/context part of the sidebar's usage reading (the glyph, token count, context percentage — or the "compacting…" marker — and the `⚠ capped` marker) opens the `/context` dialog with the full breakdown. Clicking the cost part (the `$` figure and the sub-session count) opens the `/cost` dialog instead. The "Token Usage" section title itself is not clickable.

Message **Copy** and **Edit** controls fade in and out within reserved space, without changing message height. Moving the pointer into the input, sidebar, or other UI clears their hover targets immediately while the fade finishes. Keyboard-selected message actions remain available. Fenced-code Copy controls are always visible and are unchanged.

### Thinking and Tool Details

The full TUI groups intermediate replies, reasoning, tool activity, and coordination notices into one expandable **Work** block per human-input exchange. The last three regular assistant replies stay visible by default; notices alone do not fold them. Expand Work to inspect earlier activity without losing transcript history. The header uses bold italic text in the current theme’s accent color, with `[+]` to expand and `[-]` to collapse. While active, a spinner appears before **Working**.

The session’s opening public message remains outside Work, including an initial
assignment from another agent, and appears only once. That initial assignment’s
body opens by default in compact mode; an explicit collapse or reopen is remembered.
Later agent updates can still fold into Work. The opener is not counted against the visible-reply limit; pending
receipts, internal runtime notices, and empty assistant prefixes are not openers.
Source order is preserved if work already precedes the first public message.

When an agent reply or background completion restarts the same exchange, the existing Work header becomes active again without adding a separate startup spinner. The standalone startup spinner is reserved for a fresh user message before the model begins producing work.

The header reports completed tool calls as `(1 tool)` or `(N tools)`, including failed calls and tools inside thinking blocks; outgoing reply calls count once, while incoming agent replies, background-completion notices, reasoning, and pending or running calls do not increase the count.

Expanded Work content is inset on both sides, with a blank row below the header and consistent spacing between reasoning, replies, and tool activity. This changes only the rendering bounds: nested thinking blocks, tool cards, and other expandable content retain their existing disclosure controls and behavior. Intermediate replies keep their original content without repeated “Earlier reply” headings. Assistant responses and thinking blocks do not repeat agent-name badges; incoming messages still identify their sender.

**Visible replies** under `/settings` → **Appearance** selects **Latest only**, **Last 3** (default), **Last 5**, or **All**. It counts logical assistant prose messages within each human-input exchange, not streaming chunks, tools, reasoning, or coordination notices. Older prose remains accessible inside Work without duplicating the visible replies. When a reply moves into Work, it fades out and the remaining text moves smoothly into place before the incoming reply fades in. A streaming reply fades as one component: new chunks render at its current fade level without restarting the animation. Set `settings.visible_replies` to `latest`, `"3"`, `"5"`, or `all`; this preference has no effect while Compact work log is off.

To restore the previous layout, turn off **Compact work log** under `/settings` → **Appearance**, or set `settings.compact_work_log: false`. The setting is on by default, previews immediately, and does not affect the lean TUI.

Expanded shell tool cards keep output in a bounded scroll area with the shared scrollbar and edge fades. Scroll over the output to move within that card; reaching its top or bottom does not scroll the conversation behind it. Move the pointer outside the output to scroll the conversation instead. This also applies to shell cards inside expanded Work and Thinking blocks.

Reasoning/thinking blocks are collapsed by default and carry a `Thinking` header badge. When collapsed, the TUI shows a short preview and compact tool summaries. Expand a block to see the full thinking content and the real tool renderers, including detailed tool output such as file edit diffs.

To start new sessions with thinking/tool blocks expanded by default, set `expand_thinking` in your user config:

```yaml
# ~/.config/cagent/config.yaml
settings:
  expand_thinking: true
```

Set it to `false` or omit it to keep the default collapsed behavior. See [User Settings](../../configuration/user-settings/index.md) for the full settings reference.

### Mermaid Diagrams

The TUI renders Mermaid diagram blocks inline rather than displaying raw syntax. When an assistant message contains a fenced code block tagged ` ```mermaid `, the TUI parses the diagram and draws an ASCII representation directly in the conversation:

| Diagram type                                   | Support                                           |
| ---------------------------------------------- | ------------------------------------------------- |
| `graph` / `flowchart`                          | ✅ Rendered inline                                 |
| `sequenceDiagram`                              | ✅ Rendered inline                                 |
| `stateDiagram` / `stateDiagram-v2`             | ✅ Rendered inline (supports `direction TD/TB/BT/LR/RL`) |
| Other types (`classDiagram`, `erDiagram`, …) | Falls back to a syntax-highlighted code block     |

Mermaid rendering works in both the full TUI and the lean TUI. Unsupported or syntactically invalid diagram blocks are displayed as ordinary fenced code blocks — no configuration is required and there is no way to disable it.

### LaTeX Math Rendering

The TUI renders LaTeX math expressions as terminal-friendly Unicode text. When an assistant message contains inline math (delimited by `$…$`) or display math (delimited by `$$…$$`), the TUI converts supported LaTeX commands to their Unicode equivalents and displays them directly in the conversation.

Supported features include:

- Greek letters (`\alpha`, `\beta`, `\gamma`, …)
- Mathematical operators (`\times`, `\div`, `\pm`, `\oplus`, …)
- Relations (`\le`, `\ge`, `\approx`, `\equiv`, …)
- Set operators (`\cap`, `\cup`, `\subset`, `\in`, …)
- Calculus symbols (`\int`, `\sum`, `\prod`, `\partial`, `\nabla`, …)
- Arrows and logic (`\to`, `\implies`, `\forall`, `\exists`, …)
- Superscripts and subscripts (e.g., `x^2`, `a_i`)
- Fractions (`\frac{a}{b}`), square roots (`\sqrt{x}`), and matrices
- Common functions (`\sin`, `\cos`, `\log`, `\lim`, …)

Unsupported or syntactically invalid LaTeX expressions fall back to displaying the raw source. LaTeX rendering works in both the full TUI and the lean TUI, requires no configuration, and cannot be disabled.

### Markdown Images

The TUI fetches and renders images referenced in agent responses using the Kitty graphics protocol. When an assistant message contains a standard Markdown image reference, the TUI downloads the image in the background and displays it inline at the point of the reference. While the image is loading a placeholder is shown; once loaded, the message re-renders with the image in place.

Only `http://`, `https://`, and `data:image/…;base64,…` URIs are resolved. `file://`, `sandbox://`, and any other URI scheme are rejected as a security measure against prompt-injection attacks that could otherwise read local files. Bare relative paths (e.g. `./output.png`, used for agent-generated images) are read via the local filesystem. Images that fail to load are silently dropped — the surrounding message text is unaffected. Image rendering requires a terminal that supports the Kitty graphics protocol; it is automatically disabled when the terminal does not support it. You can also disable it explicitly via `render_images: false` in `~/.config/cagent/config.yaml` or the **Render images** toggle in `/settings`.

### Snapshots, `/undo`, and `/snapshots`

Enable shadow-git snapshots globally in `~/.config/cagent/config.yaml`:

```yaml
settings:
  snapshot: true
```

When enabled, Docker Agent records filesystem snapshots at turn boundaries. The TUI exposes two slash commands that operate on those snapshots:

- **`/undo`** restores files from the most recent snapshot (one step back).
- **`/snapshots`** opens a dialog showing how many snapshots have been captured and the number of files in each one. Use <kbd>↑</kbd>/<kbd>↓</kbd> (or <kbd>j</kbd>/<kbd>k</kbd>) to highlight an entry, then press <kbd>r</kbd> to reset the workspace to that point. Pick `<original>` to revert every snapshot and bring the workspace back to its pre-agent state. <kbd>Esc</kbd> closes the dialog without changing anything.

Neither command removes messages from the session transcript — they only touch files on disk. Both commands (and the matching command-palette entries) are hidden when snapshots are turned off. Omit `snapshot` or set it to `false` to leave automatic snapshots off; agents can still configure snapshot hooks manually.

See [Snapshots](../snapshots/index.md) for how the shadow-git machinery works and how to wire it per-agent.

## File Attachments

Attach file contents to your messages using the `@` trigger:

1. Type `@` to open the file completion menu
2. Start typing to filter files (respects `.gitignore`)
3. Select a file to insert the reference

```bash
# In the chat input:
Explain what the code in @pkg/agent/agent.go does
```

The agent receives the attached contents alongside your message. In the full TUI, identifiable attachment references appear inline as numbered markers such as **📎1**, matched to filename rows below the message. A blank row separates the attachment list from the text; MIME/type labels are omitted. Messages without an identifiable inline reference still show the numbered filename rows. Click a row to open a read-only preview, or select the message and press <kbd>p</kbd> for the first attachment or <kbd>1</kbd>–<kbd>9</kbd> for a specific attachment. Image previews use the same terminal support as attachments in the input area.

Previews use the content saved with the message, not a fresh read of the original path. Unsupported or unavailable content gets an explicit fallback; remote paths are never opened against the client filesystem. The original message and stored attachment data are preserved. Legacy path-only or URL-only attachments cannot be previewed without stored content; ambiguous path references remain literal rather than being guessed.

For large or frequently-reused documents, or for getting content to an agent over the API or chat server instead of the TUI, see [Choosing a Large-Input Strategy](../../guides/headless/index.md#choosing-a-large-input-strategy).

Attached files are also recorded on the session so sub-agents spawned by task transfer can read them. To review what is attached, open `/context`: the dialog lists every attached file (and resolved prompt file) with a per-file token estimate and, when a compaction has occurred, displays the verbatim text of the most recent compaction summary. Use <kbd>↑</kbd>/<kbd>↓</kbd> to select an attached file and press <kbd>d</kbd> (or <kbd>x</kbd>/<kbd>Del</kbd>) to drop it, or run `/drop <path>` directly — press <kbd>Tab</kbd> after `/drop` and a space to complete the path from the currently attached files. Dropping stops sharing the file with sub-agents and skills; content already inlined in earlier messages stays in the conversation until compaction, and the file can always be re-attached with `@` or `/attach`.

## Generated Media

Some models (e.g. Gemini image-output models like `gemini-2.5-flash-image`)
are designed to generate an image directly as part of their reply, not just
describe one. When models.dev reports that a model can generate images, or
[`output_capabilities.image: true`](../../configuration/models/index.md#output-capabilities)
explicitly enables it, Docker Agent asks it for text *and* image output on the
models gateway, direct Gemini API, and Vertex AI. An explicit `false` disables
this behavior. See
[Google Gemini: Generated Images](../../providers/google/index.md#generated-images)
for exact configuration and limitations — ordinary image-output requests with
custom tools or structured output are rejected locally before any request is
sent. Session-title and compaction requests omit image response modalities and
skip this guard; they do not explicitly force TEXT-only output. Google
Search, Maps, and code-execution built-ins remain available. For custom tools,
models.dev metadata lets the error distinguish
a model that cannot call tools from an image-output request shape that cannot
combine both capabilities; unknown metadata keeps a conservative generic
message.

At a text-only stop, Docker Agent checks the last user prompt for phrases
such as "generate an image" or "draw a picture". A match preserves the reply
and adds this nonfatal warning: `The model returned text but no image for this
image-generation request. Try rephrasing the request.` Prompts without a
matching phrase do not trigger it. This is phrase matching, not semantic
intent detection: negated or quoted phrases can match, other wording can be
missed, and the check does not require an image-output-capable model. It does
not track a whole submission across tool calls, steering, stop hooks, or
handoffs. A terminal provider error skips this check, as does structured
output on the current agent model; the check does not inspect every override
or parse the reply to determine whether it is structured.

**Where images land.** Docker Agent attempts to save each generated image
as an ordinary workspace file and record it in the session manifest. After
both steps succeed, it stores a complete portable copy in the session database.
A failed portable-copy write does not remove the saved workspace file and
produces a per-item warning.
Database upgrades are in-place and older binaries may not understand the
upgraded schema — see [Sessions](../sessions/index.md#generated-media-files).
Generated files are untracked workspace files, yours to edit, commit, move,
or delete. A remote runtime writes to its own workspace; the local TUI does
not receive a remote binary-rendering path from this feature.

**Naming.** The model is instructed to honor an explicit prompt filename
such as `assets/red-panda.jpg` in a private naming marker. This is a request,
not a guarantee: emitted markers take precedence. When exactly one image is
returned without a marker, a single explicit filename found by a conservative
prompt parser is used. An unmarked item among several returned images does
not qualify. Otherwise names come from the provider, then `generated-1`,
`generated-2`, and so on. Parent directories are created when the validated
save succeeds. Two rules always apply:

- **The extension matches the data.** The image format is decided by the
  provider (typically PNG) — asking for `sunshine.gif` or `diagram.svg`
  does not transcode anything. If the model returns PNG data, the file is
  saved as `sunshine.png` and a notice tells you so.
- **Existing files are never overwritten.** A name collision gets a dash
  suffix instead: a second `red-panda.jpg` is saved as `red-panda-1.jpg`.
  Publication requires hard-link support; filesystems without it fail safely
  with a save warning instead of using a replacing rename.

**Paths stay in the workspace.** A prompt-directed target that is absolute,
`~`-rooted, or climbs above the workspace with `..` is not written outside the
owning session's workspace. Docker Agent discards the directory portion,
sanitizes the basename, saves it at the workspace root, and adds a bounded
warning to the turn. This does not prompt or wait for confirmation, including
over ACP and other interfaces without an elicitation consumer. General MCP and
tool elicitation is unaffected. If the owning session has no workspace root,
the save fails instead of falling back to the data directory. Relative
subdirectories remain supported after containment and symlink checks. An unusable
basename falls back to `generated-N`; a redirected save can still fail and warn
without discarding successful siblings or assistant text.

**Rendering.** Successfully resolved images can appear inline in the same
assistant turn, using Kitty-graphics support and `render_images` as described
under [Markdown Images](#markdown-images). Graphics-disabled or unsupported
terminals show a filename/path fallback. Resolution checks the owning session's
manifest before preferring its portable database copy; saved bytes can survive
workspace edits, deletion, or missing provenance. Without workspace provenance,
the label uses the recorded relative path rather than a verified absolute file. It falls back to the manifest-gated workspace file only when the
session store has no blob interface or the blob is not found. Other blob errors
fail closed. Historical manifest entries that identify an external root are
rejected. Stores without blob support and sessions created before portable
blobs were introduced continue to use legacy workspace files.
Generated-media resolution has no byte cap.

Ordinary outgoing history replaces generated-media parts with metadata
placeholders, so follow-up turns do not resend stored bytes. Explicitly
attaching a generated file or asking a tool to read it can send its contents
to a model. Legacy files still require manifest authorization, containment,
regular-file and symlink checks. These are not content-integrity checks: an
ordinary file's bytes may have changed. Failed resolution shows an unavailable
label rather than reading an unauthorized fallback.
This generated-media behavior is separate from the existing input bound for
ordinary Markdown images rendered from assistant text.

If a save fails (unwritable directory, full disk, …), only that image is
dropped, with a concise warning — the reply text and any sibling images in
the same turn are kept. If all saves in a media-only reply fail, an empty
assistant record may remain alongside the warnings. Disk-full, quota, and
unclassified failures currently use generic retry/debug advice.
Note also that an image-capable model can answer
with text only and generate no image at all; that is provider behavior, so
reword or repeat the prompt.

Inline image rendering in the TUI also covers a tool/MCP result that
returns an image, or a Markdown image reference to a file a tool actually
writes to disk — see [Markdown Images](#markdown-images) above.

### Team Context Budgets and Targeted Compaction

The `/context` dialog also shows a **Live sessions** section: the current session plus every currently running sub-agent session (foreground children spawned by task transfer and long-running `run_background_agent` tasks). Each row shows the agent name, a short session ID (so two concurrent runs of the same agent stay distinguishable), and that session's context budget: used tokens, context limit, and percentage, or an explicit "limit unknown" reading when the model's window cannot be resolved. Live-sessions rows do not repeat the compaction-cap wording themselves — the dialog's header line is the sole authority on which model, if any, caps the effective limit.

When a compaction has occurred, the dialog displays the verbatim text of the most recent compaction summary below the file inventory, under a "Latest compaction summary" section. This shows exactly what was summarized, preserving hard newlines and soft-wrapping long lines to the dialog width.

Select a live session with <kbd>↑</kbd>/<kbd>↓</kbd> and press <kbd>Enter</kbd> to explicitly compact it. Cross-agent compaction happens only on this explicit request: no idle-triggered automatic compaction is added, and the existing automatic threshold and overflow-recovery compaction of sub-agent sessions is unchanged. The request is queued onto the target session's own run loop and executes at the next safe point between model turns, so it cannot corrupt an in-flight turn. The dialog closes and a notification confirms the request; a second notification reports the outcome (compacted, skipped, or failed) with the agent's name. Selecting the main row runs the same compaction as `/compact`. `/compact` itself keeps compacting the current root session. Remote runtimes do not expose live-session tracking, so the section is omitted there.

## Runtime Model Switching

Change the AI model during a session with `/model` or <kbd>Ctrl</kbd>+<kbd>M</kbd>. Model switching works in both the full TUI and the lean TUI.

1. Press <kbd>Ctrl</kbd>+<kbd>M</kbd> (full TUI) or type `/model` (both TUIs)
2. Select from config models or type a custom `provider/model`
3. The model switch is saved with the session and restored on reload

When a models gateway is configured (`--models-gateway`) and it exposes an OpenAI-style `/v1/models` endpoint, the picker lists the models actually served by the gateway (merged with the models defined in the agent config). When the gateway doesn't expose `/v1/models`, the picker falls back to the regular catalog.

The picker's catalog entries come from [models.dev](https://models.dev) and are cached locally for a day. Press <kbd>Ctrl</kbd>+<kbd>R</kbd> in the picker to force model discovery to run again, including a refetch of the models.dev catalog.

> [!TIP]
> Use model switching to try a more capable model for complex tasks, or a cheaper one for simple queries — without modifying your YAML config.

## Editable Messages

Edit any previous user message to branch the conversation. Hover a past user message and click **✎ edit** (or select it with the keyboard and press <kbd>e</kbd>) to modify it — the agent will re-process from that point, while the original session history is preserved. This is great for exploring alternative approaches without losing your work.

Hovering a user or assistant message also reveals a **⎘ copy** button that copies the message text to the clipboard (<kbd>c</kbd> when a message is selected).

## Error Recovery

When an agent turn fails (fatal model error, hook block, loop detection, tool-setup failure), the TUI displays the error in the message stream and persists it to the session store. Errors survive a reload and are shown exactly where they occurred, making them visible in shared or remote sessions.

Each error message includes a clickable **↻ retry** button. Clicking it resumes the conversation from the point of failure — without retyping your last message. This lets you recover from transient failures (rate limits, network blips, model API errors) in one click.

## Session Management

Docker Agent automatically saves your sessions. Use `/sessions` to browse past conversations:

- **Browse** past sessions with search and filtering. The search matches against session **titles** and **session IDs** (full UUIDs, dash-less variants, and partial fragments all resolve correctly — useful when jumping back to a session from a copied ID or log).
- **Workspace grouping**: sessions are grouped by **git repository root** (worktree-aware) — sessions from any subdirectory or linked worktree of the same repository are grouped together under "This workspace", and the header shows the repository root path. Sessions outside the current repository appear under "Other locations" with their originating directory. Press <kbd>Ctrl</kbd>+<kbd>G</kbd> in the browser to cycle between all, current-workspace only, and other-directory views. Restoring a session reopens it in its original directory, so the label always matches where a restore will land.
- **Star** important sessions with `/star`
- **Branch** conversations by editing any previous user message — preserving the original session history
- **Resume** sessions with `docker agent run config.yaml --session <id>`
- **Relative refs**: `--session -1` for the last session, `-2` for the one before

### Session Title Editing

Customize session titles to make them more meaningful and easier to find. By default, Docker Agent auto-generates titles based on your first message, but you can override or regenerate them at any time.

**Using the `/title` command:**

```bash
/title                     # Regenerate title using AI (based on recent messages)
/title My Custom Title     # Set a specific title
```

**Using the sidebar:**

1. Click the pencil icon (✎) next to the session title in the sidebar
2. Type your new title
3. Press <kbd>Enter</kbd> to save, or <kbd>Escape</kbd> to cancel

> [!NOTE]
> Manually set titles are preserved and won’t be overwritten by auto-generation. Title changes are persisted immediately to the session.

## Keyboard Shortcuts

| Shortcut   | Action                                          |
| ---------- | ----------------------------------------------- |
| Ctrl+K     | Open command palette                            |
| Ctrl+M     | Switch model                                    |
| Ctrl+R     | Reverse history search (search previous inputs) |
| Ctrl+G     | Cancel reverse history search                   |
| Ctrl+S     | Cycle to next agent in the team                 |
| Shift+Tab  | Cycle the current model's thinking-effort level (shows a `✻ Thinking: <level>` toast) |
| Ctrl+1 – 9 | Switch directly to agent _N_ in the team list   |
| Ctrl+T     | Open a new tab (additional agent session)       |
| Ctrl+W     | Close the current tab                           |
| Ctrl+N     | Next tab                                        |
| Ctrl+P     | Previous tab                                    |
| Ctrl+B     | Toggle the sidebar (full-UI mode only; disabled when --sidebar=false) |
| Ctrl+Y     | Toggle YOLO mode (auto-approve tool calls)      |
| Ctrl+O     | Toggle hide tool results                        |
| Ctrl+Z     | Suspend TUI to background (resume with `fg`)    |
| Escape     | Cancel current operation                        |
| Enter      | Send message; steer it into the active turn while the agent is running (queues instead when `busy_send_mode` is `queue`) |
| Alt+Enter  | Queue it for the next turn while the agent is running |
| Alt+Up     | Recall all pending (steered/follow-up/queued) messages back into the editor for editing |
| Shift+Enter | Insert a newline |
| Up/Down    | Navigate message history                        |

Press <kbd>Ctrl</kbd>+<kbd>H</kbd> to view the complete list of all available keyboard shortcuts.

### Custom Keybindings

You can remap the shortcuts above by adding a `keybindings` list to the `settings` block of your `~/.config/cagent/config.yaml` (see [User Settings](../../configuration/user-settings/index.md#settings-reference) for the field reference). Each entry maps an action to one or more key combinations (for example `ctrl+q`, `alt+enter`, `f2`). Unlisted actions keep their defaults.

This is the recommended way to replace the `Ctrl+J` newline fallback, which conflicts with common editor/terminal shortcuts (for example inside VS Code).

```yaml
settings:
  keybindings:
    # Replace the Ctrl+J newline fallback. Shift+Enter still works automatically
    # on terminals that report it. Alt+Enter is reserved for queueing.
    - action: "editor_newline"
      keys: ["alt+j"]
    # Allow several keys for one action.
    - action: "commands"
      keys: ["f2", "ctrl+k"]
    - action: "quit"
      keys: ["ctrl+q"]
```

**Valid actions:**

| Action                     | Default      | Description                            |
| -------------------------- | ------------ | -------------------------------------- |
| `editor_send`              | `enter`      | Send the current message               |
| `editor_newline`           | `ctrl+j`     | Insert a newline in the input          |
| `quit`                     | `ctrl+c`     | Quit (opens the exit confirmation)     |
| `switch_focus`             | `tab`        | Switch focus between panels            |
| `commands`                 | `ctrl+k`     | Open the command palette               |
| `help`                     | `ctrl+h`     | Show the help dialog                   |
| `toggle_yolo`              | `ctrl+y`     | Toggle YOLO mode                       |
| `toggle_hide_tool_results` | `ctrl+o`     | Toggle hiding tool results             |
| `cycle_agent`              | `ctrl+s`     | Cycle to the next agent                |
| `model_picker`             | `ctrl+m`     | Open the model picker                  |
| `suspend`                  | `ctrl+z`     | Suspend the TUI                        |
| `toggle_sidebar`           | `ctrl+b`     | Toggle the sidebar                     |
| `edit_external`            | `ctrl+g`     | Edit input in an external editor       |
| `history_search`           | `ctrl+r`     | Incremental history search             |
| `close_tab`                | `ctrl+w`    | Close the current tab (disabled while the editor is focused, so Ctrl+W deletes a word)|

`ctrl+w` is reserved for `close_tab`: other actions can't bind it, so it keeps deleting a word in the editor.

`Shift+Enter` for newline is detected from your terminal's capabilities and is always available where supported, independent of `editor_newline`.

Invalid entries are ignored with a warning (visible with `--debug`) so a bad config never breaks the TUI: unknown actions, empty or malformed keys, and keys that would collide with another action are dropped while every other binding keeps working.

## History Search

Press <kbd>Ctrl</kbd>+<kbd>R</kbd> to enter incremental history search mode. Start typing to filter through your previous inputs. Press <kbd>Enter</kbd> to select a match, or <kbd>Escape</kbd> to cancel.

## Settings

Run `/settings` to open the settings dialog. Use <kbd>Tab</kbd> to switch between **Appearance**, **Behavior**, and **Notifications**.

> [!TIP]
> **Full settings reference**
>
> This section covers the `/settings` dialog. For the complete list of `settings:` fields (including ones with no dialog UI, like `permissions`, `hooks`, and `keybindings`) and how they interact with CLI flags and aliases, see [User Settings](../../configuration/user-settings/index.md).

The full TUI reserves one blank row above its main content. A left-docked sidebar keeps a one-cell internal gap beside the messages and left-aligns its bottom status badges, including YOLO. The right dock retains its existing badge alignment.

The **Appearance** tab selects the theme and customizes the layout. Layout changes show a live schematic preview and apply immediately to the UI behind the dialog:

- **Sidebar position**: `Right` (default), `Left`, `Top`, or `Bottom`. Left/right keep the full vertical sidebar next to the chat; top/bottom render it as a compact horizontal band above or below the chat (session title, working directory, token usage, plus a one-line summary of the current agent and its model; in multi-agent configurations all team agents are listed by name after the current agent).
- **Agent context %**: off by default. Turn it on to show each agent's context-window fill in the sidebar: subagent rows show it directly after the agent name, with the state still right-aligned, and participant lines and agent cards show it too. A subagent row reads its own session's usage, so same-named subagents each show their own value; a row drops the reading before it truncates a name or ID. The main context bar is unaffected. Set `settings.show_agent_context: true` to enable it.
- **Align input with messages**: off by default. Turn it on to align the entire input dock—tabs, editor, and context-usage bar—with the messages column. These child components share one dock’s bounds, while a left/right sidebar extends uninterrupted beside the whole dock, down to the full-width global footer. With a hidden or top/bottom sidebar, the dock remains full width. This setting does not affect the lean TUI.
- **Sidebar info mode**: `Compact` (default) or `Detailed`. Controls how the Agents panel renders agent rows — see [Agents Panel](#agents-panel) for details. Persisted as `settings.layout.sidebar_info_mode: detailed`; compact is the default and omitted from the config.
- **Section spacing**: `Compact`, `Normal` (default), or `Relaxed`, the number of blank lines between the sidebar sections (1, 2, or 3).
- **Sidebar sections**: toggle the visibility of the **Session path** (the working directory line, including its git branch) and the **Token usage**, **Agents**, **Tools**, **Plans**, and **Todos** sections. All are visible by default except [Plans](#plans-sidebar), which is opt-in. The session title is always shown.

Appearance also controls **Compact work log**, **Visible replies**, split-diff rendering, expanded thinking, whether tool results are hidden by default, and **Show startup banner** — the ASCII-art banner drawn on an empty conversation (persisted as `settings.show_banner: false` when turned off, and honored by the lean TUI too). Select **Theme** to open the theme picker.

The **Behavior** tab shows the busy-message keys (Enter steers; Alt+Enter queues) and controls the auto-approve default, tab restoration, automatic snapshots, lean UI, and the maximum tab-title length. Restore-tabs and lean-UI changes take effect on the next launch. Enabling auto-approve requires confirmation.

The **Notifications** tab enables completion sounds and sets the minimum task duration before a sound plays.

Press <kbd>Enter</kbd> to apply and persist, or <kbd>Escape</kbd> to cancel and restore the previous layout. The settings are saved globally in `~/.config/cagent/config.yaml`:

```yaml
# ~/.config/cagent/config.yaml
settings:
  layout:
    sidebar_position: left # right (default), left, top, bottom
    align_input_with_messages: true # false (default): full-width input
    sidebar_info_mode: detailed # compact (default, omitted), detailed
    section_spacing: compact # normal (default), compact, relaxed
    hide_session_path: false
    hide_usage: true
    hide_agents: false
    active_agents_only: false # true to filter to session-active agents
    hide_tools: false
    hide_todos: false
```

## Theming

Customize the TUI appearance with built-in or custom themes:

```bash
# Open Settings and select Theme under Appearance
/settings
```

### Built-in Themes

`docker-next-dark` (default), `docker-next-light`, `default` (Old School, the previous default), `black-amber`, `black-cyan`, `black-violet`, `calm-roots`, `catppuccin-latte`, `catppuccin-mocha`, `default-light`, `dracula`, `gruvbox-dark`, `gruvbox-light`, `neon-pink`, `nord`, `one-dark`, `solarized-dark`, `tokyo-night`, `white-cobalt`, `white-rose`, `white-teal`.

The Docker Next themes change colors only. Typography is chosen by your terminal emulator. For an optional dedicated font, see [Custom terminal fonts](../../terminal-fonts.md) (a host-side launcher; it does not change fonts in your existing terminal or Docker Desktop).

#### Docker Next palettes

The palettes are derived from a small set of brand anchors. Each color in the theme files has a trailing comment with its anchor or derivation. Channels are sRGB, rounded to 8 bits:

- `X +p% toward Y` = `X + p·(Y − X)`
- `X p% over Y` = `Y + p·(X − Y)`
- `hue h/s/l` = the HSL color with hue `h`, saturation `s`, lightness `l`

| Anchor | Exact reference | Theme roles |
| --- | --- | --- |
| Base | `#475BFF` | Brand fills; dark accent text is lifted toward Zero, light accent text is darkened toward Console |
| Canvas | `#F5F2EE` | Light raised surfaces |
| Zero | `#FFFDFA` | Dark bright text; light page background |
| Ether | `#101010` | Dark page background; light primary and bright text |
| Ink | `#1D1D1D` | Dark raised surfaces; light code punctuation |
| Launch | `#FF6A3D` | Busy and syntax accent; light variant `#AB4A2D` is Launch +35% toward Ether |
| Sand | `#FFCEA5` | Dark warning; light warning `#9E4800` uses its hue (HSL 27/100/31) |
| Neutral | `#DDD4C9` | Dark primary text; muted text and borders blend toward Ether |
| Air | `#D1E4FF` | Dark info; light selection and badge tint |
| Bloom | `#F3E5F6` | Highlight and badge hue, adapted to HSL 290/45/76 (dark) and 290/35/40 (light) |
| Flux | `#00E476` | Dark success; light success `#096B3C` is Flux +57% toward Ether |
| Trigger | `#FF3D40` | Dark error; light error `#AE2324` is Trigger +50% toward Root |
| Console | `#3345D6` | Dark drag highlight; light info and links |
| Root | `#5C0909` | Error background base; code and numeric literals |

Contrast notes: Base is a brand fill, not body text. On Zero it reaches 4.91:1, but on Canvas it falls to 4.47:1, so light-theme accent text uses the darker `#4457F8` (5.22:1 on Zero, 4.75:1 on Canvas). Raised Base surfaces are limited to roles that only need 3:1. The ratios assume the page background is painted by the theme; transparent cells over your terminal's own background depend on that background matching the selected polarity. Primary body text also meets 4.5:1 in the existing inactive-pane dim state; secondary, accent, and status text retain the existing dimming behavior and can fall below that threshold. This is not an all-text accessibility guarantee.

### Auto Theme (match the terminal)

The special theme `auto` follows the terminal's light/dark background instead of naming a fixed theme. Select **Auto (match terminal)** from **Settings → Appearance → Theme**, pass `--theme auto`, or set it in your user config:

```yaml
settings:
  theme: auto
  theme_dark: docker-next-dark # optional, theme used on dark backgrounds (default: docker-next-dark)
  theme_light: docker-next-light # optional, theme used on light backgrounds (default: docker-next-light)
```

At startup the terminal background is queried (OSC 11) to pick the dark or light theme of the pair; non-interactive runs (pipes, CI) fall back to the dark theme. In terminals that report appearance changes (DEC mode 2031 — Ghostty, kitty, contour, …), flipping the OS or terminal appearance while Docker Agent is running switches the theme live. Terminals without that mode re-sync when the window regains focus.

### Custom Themes

Create theme files in `~/.cagent/themes/` as YAML. Theme files are **partial overrides** — you only need to specify the colors you want to change. Any omitted keys fall back to the Old School (`default`) theme values, not the Docker Next defaults.

```yaml
# ~/.cagent/themes/my-theme.yaml
name: "My Custom Theme"

colors:
  # Backgrounds
  background: "#1a1a2e"
  background_alt: "#16213e"

  # Text colors
  text_bright: "#ffffff"
  text_primary: "#e8e8e8"
  text_secondary: "#b0b0b0"
  text_muted: "#707070"

  # Accent colors
  accent: "#4fc3f7"
  brand: "#1d96f3"

  # Status colors
  success: "#4caf50"
  error: "#f44336"
  warning: "#ff9800"
  info: "#00bcd4"

# Optional: Customize syntax highlighting colors
chroma:
  comment: "#6a9955"
  keyword: "#569cd6"
  literal_string: "#ce9178"

# Optional: Customize markdown rendering colors
markdown:
  heading: "#4fc3f7"
  link: "#569cd6"
  code: "#ce9178"
```

### Applying Themes

**In user config** (`~/.config/cagent/config.yaml`, see [User Settings](../../configuration/user-settings/index.md) for the full reference):

```yaml
settings:
  theme: my-theme # References ~/.cagent/themes/my-theme.yaml
```

**At launch:** Pass `--theme <name>` to `docker agent run` to preselect a theme for that session. This overrides `settings.theme` in your config but is not saved. Invalid theme names print an error at startup listing the available options. Has no effect in `--exec` mode. `--theme auto` enables the [auto theme](#auto-theme-match-the-terminal) for the session.

**At runtime:** Open `/settings`, select **Theme** on the Appearance tab, and choose from the available themes. Your selection is saved globally in `~/.config/cagent/config.yaml` under `settings.theme` and persists across sessions.

> [!TIP]
> **Hot Reload**
>
> Custom themes auto-reload when you save changes to the file — no restart needed. This makes it easy to tweak colors in real-time.

> [!WARNING]
> **Partial overrides**
>
> All user themes are applied on top of the Old School (`default`) theme, so omitted values use its colors rather than Docker Next. If you want to customize a built-in theme (e.g., `dracula`), copy its full YAML from the [built-in themes on GitHub](https://github.com/docker/docker-agent/tree/main/pkg/tui/styles/themes) into `~/.cagent/themes/` and edit the copy. Otherwise, omitted values will use Old School colors, not the original theme's colors.

## Tool Permissions

When an agent calls a tool, Docker Agent shows a confirmation dialog by default. You can:

- **Approve once** — Allow this specific call
- **Always allow** — Permanently approve this tool/command for the session
- **Deny** — Reject the tool call

**Granular permissions:** The permission system supports pattern-based matching. When you “Always allow” a specific tool command, only that exact pattern is auto-approved — other commands from the same tool still require confirmation. This lets you auto-approve safe, read-only operations while maintaining control over destructive ones.

> [!TIP]
> **YOLO mode**
>
> Use `--yolo` or the `/yolo` command to auto-approve all tool calls. You can also toggle this mid-session. For aliases, set `--yolo` when creating the alias: `docker agent alias add fast myorg/coder --yolo`.

## Notifications

The TUI displays transient notification banners for agent warnings, errors, and other runtime events. Notifications auto-dismiss after a short delay unless the mouse is hovering over them — hovering pauses the timer so you have time to read the message.

| Interaction | Behaviour |
| ----------- | --------- |
| Hover       | Pauses auto-dismiss; the notification stays visible until the mouse moves away |
| Click       | Copies the notification text to the clipboard |
| × (close)   | Dismisses immediately; the glyph turns red when hovered |

Hint text in the top-left corner of the notification border shows the available actions at a glance.

## Subagents

`/subagents` opens the existing subagent tree in both full and lean interfaces.
Use the arrow keys to navigate or fold branches and **Attach** (or Enter) to
open the selected session. The bottom **Use subagents: ON/OFF** control saves
the delegation preference, which defaults to **ON**. Use Tab to focus actions
and Enter to activate them, press `u` to toggle, or click the control in the
full TUI.

- `/subagents on` enables new subagent delegation.
- `/subagents off` disables new subagent delegation.

The preference is saved in your user configuration and applies to existing
local runtimes and new or restored sessions. Disabling it does not cancel
already accepted work, delete the tree, or prevent attaching to existing
sessions. Changes are applied only after a successful save. Remote runtimes
that cannot enforce this preference reject the change without saving it.

In the full-TUI sidebar, narrow subagent rows preserve tree connectors before truncating names. Hovered IDs take precedence over names and remain visible after their reveal animation settles. Automatically opened team branches collapse when their work is stopped; explicit manual expansion or collapse choices are preserved.

### Background shell processes

The sidebar's **Background** section is separate from **Subagents**: it lists only shell processes the owning session actually adopted, not transcript history or long-running child agents. When a process is adopted while the session is idle, the section opens automatically; it starts collapsed for an active stream, and you can collapse or reopen it manually. The manual collapsed/expanded choice is retained across empty process lists and subsequent adoptions for the same session. Running rows update live, and completed, stopped, or failed rows remain briefly before expiring.

A parent’s Subagents row also shows the number of currently running background shell processes owned by that child session, without marking the child itself as busy. Each tab shows detailed processes owned by its own session. Selecting a process row navigates to its original shell tool call when that call is still uniquely present in the session transcript; otherwise the row is informational and cannot navigate. The TUI does not provide a process-kill action here. Completion output arrives automatically and can resume an idle agent; agents do not need to poll process status or keep issuing tools while waiting. Process inspection is for a specific diagnostic need, not routine completion checks.

### Git changes and code review

The sidebar uses one **Files - Git** header with a repository count. The collapsed header rests muted and highlights as one row on hover; when expanded, the active mode retains its distinct color. Keyboard focus does not latch the label bright or bold. Click the active mode label to collapse the section, or the other label to switch modes. From a collapsed header, either label opens its mode. **Files** starts with the working directory’s contents, not an extra row for the working directory itself. Folders start collapsed and load their contents when opened; explicit expansion choices survive refreshes and mode changes. **Git** shows changed paths grouped by repository, with worktrees nested beneath their repository. Green `+N` and red `−N` line counts appear inline beside each changed filename; repository totals stay separate. Binary or unavailable counts are omitted. Select a path to inspect a diff or whole-file view; press <kbd>f</kbd> to switch between them and <kbd>o</kbd> to open a local whole file in the configured external editor. Remote workspaces never fall back to local filesystem access.

Opening **Files** bootstraps only the working-directory scope, then lists that directory.
Opening a folder lists only that folder; it does not trigger nearby-repository discovery
or load sibling and descendant folders. Git discovery warnings do not apply to Files,
but errors and truncation of the requested directory remain visible. Remote Files
requires a server with the root-only browse endpoint; older servers report browsing
unavailable rather than falling back to a recursive scan or the client filesystem.

Within one TUI, attached agents and subagents in the same authorized workspace reuse
Files/Git results and join matching requests already in progress instead of starting
another scan. Refresh results are shared with every attached view of that workspace;
selection, expanded folders, and review drafts remain independent. Opening Files still
does not start Git discovery. Opening Git reuses an existing full or partial inventory,
or discovers repositories if only a Files browse root has been loaded. Explicit refresh
requests fresh data; merely switching or attaching does not.

This cache is bounded and in-memory, not persistent across application restarts.
Sharing respects workspace, backend, source, and permission boundaries: identical path
text on different servers is not the same workspace. Remote paths are resolved only
by the server, never against the client's filesystem. Separate remote connection
instances conservatively keep separate client caches. Updates propagate between views
within the same TUI, not as live push notifications to independently launched clients.
Leaving one view does not cancel a matching request while another view still needs it.
The client checks session policy changes before sharing updates. A server-side team
policy change without updated session metadata is not pushed into the client cache;
rebuilding the attachment revalidates its effective permissions. The server still
checks the current policy on every Files/Git request.

**Git** discovery searches up to four directory levels, prioritizing shallow candidates
and reading same-depth containers in bounded batches. It stops descending into discovered
checkouts. Ordinary files do not consume the directory-discovery budget. Hidden and
dependency directories are skipped; declared submodules and Git worktree metadata supply
nested checkouts without walking every tracked file. The initial scan remains bounded
to 100 distinct repositories (plus the current checkout or workspace row), so larger or deeper workspaces can still show
**Incomplete**. **Continue scan** is available only for supported local providers;
remote discovery currently has no continuation endpoint. Continuing can raise the
depth limit to at most eight levels; discovery never deepens automatically. Remote sessions discover repositories on the server.

Press <kbd>c</kbd> on a diff line, or drag text to anchor an inline review comment. Use the composer while a review inspector is open for an optional general comment; press <kbd>Enter</kbd> to send the general comment and any inline comments together to the currently active session, or <kbd>Escape</kbd> to return to review while keeping the draft. The normal chat composer draft is restored afterward. <kbd>R</kbd> opens the review draft and <kbd>s</kbd> previews its send action.

Git comments include the selected change context and relevant line ranges. Files comments are plain snippet references—file path, selected lines, and your comment—without a review preamble. Submitted comments remain user messages and render as compact reference cards in the full TUI; the original snippet and comment remain available in the sent content.
