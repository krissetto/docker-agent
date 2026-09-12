// Package subagenttool renders the async subagent tools (spawn_subagent,
// send_message, read_subagent) as compact one-liners — "Spawned <agent> (id)",
// "Messaged <agent> (id)", "Inspecting <agent> (id)" — with the agent name in
// its accent color. Tool results are intentionally never rendered: they are
// context for the model, not for the user.
package subagenttool

import (
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type NameLookup func(subagent.NodeID) (string, bool)

func NewSpawn(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader, lookup NameLookup) layout.Model {
	return toolcommon.NewBase(ar, msg, sessionState, renderer(renderSpawn, lookup))
}

func NewSend(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader, lookup NameLookup) layout.Model {
	return toolcommon.NewBase(ar, msg, sessionState, renderer(renderSend, lookup))
}

func NewRead(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader, lookup NameLookup) layout.Model {
	return toolcommon.NewBase(ar, msg, sessionState, renderer(renderRead, lookup))
}

func NewStop(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader, lookup NameLookup) layout.Model {
	return toolcommon.NewBase(ar, msg, sessionState, renderer(renderStop, lookup))
}

type renderFunc func(*types.Message, spinner.Spinner, service.SessionStateReader, int, int, NameLookup) string

func renderer(render renderFunc, lookup NameLookup) toolcommon.Renderer {
	return func(msg *types.Message, s spinner.Spinner, state service.SessionStateReader, width, height int) string {
		return render(msg, s, state, width, height, lookup)
	}
}

func renderSpawn(msg *types.Message, s spinner.Spinner, _ service.SessionStateReader, _, _ int, lookup NameLookup) string {
	name, id := attribution(msg, "", lookup)
	if name == "" {
		if params, err := toolcommon.ParseArgs[subagent.SpawnArgs](msg.ToolCall.Function.Arguments); err == nil {
			name = params.Agent
		}
	}
	return line(msg, s, verb(msg, "Spawning", "Spawned"), name, id)
}

func renderSend(msg *types.Message, s spinner.Spinner, _ service.SessionStateReader, _, _ int, lookup NameLookup) string {
	v := verb(msg, "Messaging", "Messaged")
	params, err := toolcommon.ParseArgs[subagent.SendArgs](msg.ToolCall.Function.Arguments)
	if err == nil && params.To == subagent.ParentAlias {
		return statusIcon(msg, s) + " " + styles.MutedStyle.Render(v+" parent")
	}
	var argID string
	if err == nil {
		argID = params.To
	}
	name, id := attribution(msg, argID, lookup)
	return line(msg, s, v, name, id)
}

func renderRead(msg *types.Message, s spinner.Spinner, _ service.SessionStateReader, _, _ int, lookup NameLookup) string {
	var argID string
	if params, err := toolcommon.ParseArgs[subagent.ReadArgs](msg.ToolCall.Function.Arguments); err == nil {
		argID = params.SubagentID
	}
	name, id := attribution(msg, argID, lookup)
	return line(msg, s, "Inspecting", name, id)
}

func renderStop(msg *types.Message, s spinner.Spinner, _ service.SessionStateReader, _, _ int, lookup NameLookup) string {
	var argID string
	if params, err := toolcommon.ParseArgs[subagent.StopArgs](msg.ToolCall.Function.Arguments); err == nil {
		argID = params.SubagentID
	}
	name, id := attribution(msg, argID, lookup)
	return line(msg, s, verb(msg, "Stopping", "Stopped"), name, id)
}

// NodeIDFor resolves the subagent node id a rendered subagent tool message
// refers to, so the TUI can attach a tab to that subagent on click. ok is
// false for non-subagent tools, parent-directed send_message, and calls whose
// id cannot be determined yet.
func NodeIDFor(msg *types.Message) (subagent.NodeID, bool) {
	if msg == nil || msg.Type != types.MessageTypeToolCall {
		return "", false
	}
	var argID string
	switch msg.ToolCall.Function.Name {
	case subagent.ToolSpawnSubagent:
		// id only exists once the result is stamped; attribution parses it.
	case subagent.ToolSendMessage:
		params, err := toolcommon.ParseArgs[subagent.SendArgs](msg.ToolCall.Function.Arguments)
		if err != nil || params.To == subagent.ParentAlias {
			return "", false
		}
		argID = params.To
	case subagent.ToolReadSubagent:
		params, err := toolcommon.ParseArgs[subagent.ReadArgs](msg.ToolCall.Function.Arguments)
		if err != nil {
			return "", false
		}
		argID = params.SubagentID
	case subagent.ToolStopSubagent:
		params, err := toolcommon.ParseArgs[subagent.StopArgs](msg.ToolCall.Function.Arguments)
		if err != nil {
			return "", false
		}
		argID = params.SubagentID
	default:
		return "", false
	}
	_, id := attribution(msg, argID, nil)
	if id == "" {
		return "", false
	}
	return subagent.NodeID(id), true
}

// attribution resolves the subagent name and id for a tool message. The tool
// result's stamped attribution (`… subagent "name" (id) …`) is authoritative;
// restored transcripts carry that result text in msg.Content instead of
// ToolResult, so it is parsed too. While the call is still running the live
// swarm index covers the id from the call arguments. name is "" when no
// source knows it.
func attribution(msg *types.Message, argID string, lookup NameLookup) (name, id string) {
	if msg.ToolResult != nil {
		if n, nid, ok := subagent.MentionedSubagent(msg.ToolResult.Output); ok {
			return n, string(nid)
		}
	}
	if n, nid, ok := subagent.MentionedSubagent(msg.Content); ok {
		return n, string(nid)
	}
	if argID != "" && lookup != nil {
		if n, ok := lookup(subagent.NodeID(argID)); ok {
			return n, argID
		}
	}
	return "", argID
}

// line assembles `icon <verb> <agent> (id)` with the name accent-colored and
// everything else muted. Unknown parts are simply omitted.
func line(msg *types.Message, s spinner.Spinner, verb, name, id string) string {
	out := statusIcon(msg, s) + " " + styles.MutedStyle.Render(verb)
	if name != "" {
		out += " " + styles.AgentAccentStyleFor(name).Render(name)
	}
	if id != "" {
		out += styles.MutedStyle.Render(" (" + id + ")")
	}
	return out
}

// verb picks the wording from the tool status: the in-progress form while
// running (and on error — the attempt failed), the done form on success.
func verb(msg *types.Message, running, done string) string {
	switch msg.ToolStatus {
	case types.ToolStatusRunning, types.ToolStatusPending, types.ToolStatusConfirmation, types.ToolStatusError:
		return running
	default:
		return done
	}
}

// statusIcon picks the leading glyph: an animated spinner while the tool call
// runs, ✓ on success, ✗ on error.
func statusIcon(msg *types.Message, s spinner.Spinner) string {
	switch msg.ToolStatus {
	case types.ToolStatusRunning, types.ToolStatusPending, types.ToolStatusConfirmation:
		return styles.NoStyle.MarginLeft(2).Render(s.View())
	case types.ToolStatusError:
		return styles.ToolErrorIcon.Render("✗")
	default:
		return styles.ToolCompletedIcon.Render("✓")
	}
}
