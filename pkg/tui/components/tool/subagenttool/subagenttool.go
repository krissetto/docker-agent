// Package subagenttool renders the async subagent tools (spawn_subagent,
// send_message, read_subagent) as compact one-liners — "Spawned <agent> (id)",
// "Messaged <agent> (id)", "Inspecting <agent> (id)" — with the agent name in
// its accent color. Successful results stay compact; failures expose a bounded
// actionable explanation.
package subagenttool

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/components/agentmessage"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type (
	NameLookup      func(subagent.NodeID) (string, bool)
	ReferenceLookup func(subagent.NodeID) lifecycle.InputReference
)

func NewSpawn(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader, lookup NameLookup, references ...ReferenceLookup) layout.Model {
	return toolcommon.NewBase(ar, msg, sessionState, renderer(newSpawnRenderer(), lookup, references...))
}

func NewSend(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader, lookup NameLookup, references ...ReferenceLookup) layout.Model {
	m := &sendModel{msg: msg, disclosure: agentmessage.New(ar), width: 80, lookup: lookup, references: references}
	m.Base = toolcommon.NewBaseWithCollapsed(ar, msg, sessionState, renderer(func(msg *types.Message, s spinner.Spinner, state service.SessionStateReader, width, height int, lookup NameLookup) string {
		header := renderSendHeader(msg, s, width, lookup, " "+m.disclosure.Chevron())
		m.headerLines = strings.Count(header, "\n") + 1
		return header
	}, lookup, references...), toolcommon.CollapsedRenderer(renderer(renderSend, lookup, references...)))
	return m
}

func NewRead(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader, lookup NameLookup, references ...ReferenceLookup) layout.Model {
	return toolcommon.NewBase(ar, msg, sessionState, renderer(renderRead, lookup, references...))
}

func NewStop(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader, lookup NameLookup, references ...ReferenceLookup) layout.Model {
	return toolcommon.NewBase(ar, msg, sessionState, renderer(renderStop, lookup, references...))
}

type renderFunc func(*types.Message, spinner.Spinner, service.SessionStateReader, int, int, NameLookup) string

func renderer(render renderFunc, lookup NameLookup, references ...ReferenceLookup) toolcommon.Renderer {
	return func(msg *types.Message, s spinner.Spinner, state service.SessionStateReader, width, height int) string {
		projected := *msg
		if id, ok := NodeIDFor(msg); ok && len(references) > 0 && references[0] != nil {
			projected.InputReference = references[0](id)
		}
		return withError(render(&projected, s, state, width, height, lookup), &projected, width)
	}
}

func renderSpawn(msg *types.Message, s spinner.Spinner, _ service.SessionStateReader, width, _ int, lookup NameLookup) string {
	name, id := attribution(msg, "", lookup)
	if name == "" {
		if params, err := toolcommon.ParseArgs[subagent.SpawnArgs](msg.ToolCall.Function.Arguments); err == nil {
			name = params.Agent
		}
	}
	return line(msg, s, verb(msg, "Spawning", "Spawned"), name, id, width)
}

func renderSend(msg *types.Message, s spinner.Spinner, _ service.SessionStateReader, width, _ int, lookup NameLookup) string {
	return renderSendHeader(msg, s, width, lookup, "")
}

func renderSendHeader(msg *types.Message, s spinner.Spinner, width int, lookup NameLookup, suffix string) string {
	v := verb(msg, "Messaging", "Messaged")
	params, err := toolcommon.ParseArgs[subagent.SendArgs](msg.ToolCall.Function.Arguments)
	if err == nil && params.To == subagent.ParentAlias {
		return ansi.Hardwrap(statusIcon(msg, s)+" "+styles.MutedStyle.Render(v+" parent"+suffix), max(1, width), true)
	}
	var argID string
	if err == nil {
		argID = params.To
	}
	name, id := attribution(msg, argID, lookup)
	return identityLine(msg, s, v, name, id, suffix, width)
}

func renderRead(msg *types.Message, s spinner.Spinner, _ service.SessionStateReader, width, _ int, lookup NameLookup) string {
	var argID string
	if params, err := toolcommon.ParseArgs[subagent.ReadArgs](msg.ToolCall.Function.Arguments); err == nil {
		argID = params.SubagentID
	}
	name, id := attribution(msg, argID, lookup)
	return line(msg, s, "Inspecting", name, id, width)
}

func renderStop(msg *types.Message, s spinner.Spinner, _ service.SessionStateReader, width, _ int, lookup NameLookup) string {
	var argID string
	if params, err := toolcommon.ParseArgs[subagent.StopArgs](msg.ToolCall.Function.Arguments); err == nil {
		argID = params.SubagentID
	}
	name, id := attribution(msg, argID, lookup)
	return line(msg, s, verb(msg, "Stopping", "Stopped"), name, id, width)
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
		if n, nid, ok := stampedAttribution(msg.ToolCall.Function.Name, msg.ToolResult.Output); ok {
			return n, string(nid)
		}
	}
	if n, nid, ok := stampedAttribution(msg.ToolCall.Function.Name, msg.Content); ok {
		return n, string(nid)
	}
	if argID != "" && lookup != nil {
		if n, ok := lookup(subagent.NodeID(argID)); ok {
			return n, argID
		}
	}
	return "", argID
}

// line keeps the stamped display name while resolving its canonical agent color.
func line(msg *types.Message, s spinner.Spinner, verb, name, id string, width int) string {
	return identityLine(msg, s, verb, name, id, "", width)
}

func identityLine(msg *types.Message, s spinner.Spinner, verb, name, id, suffix string, width int) string {
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: id, Name: name, Agent: name, DisplayID: subagent.ShortID(id)}
	if id == "" {
		ref.Kind = lifecycle.InputReferenceUnknown
	}
	if msg.InputReference.Kind != lifecycle.InputReferenceUnknown {
		ref.Agent = msg.InputReference.Agent
	}
	return agentidentity.Wrap(statusIcon(msg, s)+" "+styles.MutedStyle.Render(verb)+" ", ref, styles.MutedStyle.Render(suffix), width)
}

// CompletionPresentation uses only the immutable report outcome. Missing legacy
// provenance is neutral; current tree state is deliberately not consulted.
func CompletionPresentation(outcome session.ReportOutcome) (icon, label string) {
	switch outcome {
	case session.ReportOutcomeFinished:
		return styles.ToolCompletedIcon.Render("✓"), "turn finished"
	case session.ReportOutcomeFailed:
		return styles.ToolErrorIcon.Render("✗"), "turn failed"
	default:
		return styles.MutedStyle.Render("·"), "report received"
	}
}

// RenderInput trusts only the resolved typed sender and historical outcome.
func RenderInput(msg *types.Message, width int) string {
	icon, label := CompletionPresentation(msg.ReportOutcome)
	ref := msg.InputReference
	if ref.Name == "" && ref.DisplayID == "" {
		return icon + " " + styles.MutedStyle.Render("Runtime update received")
	}
	if ref.Name == "" {
		ref.Name = "subagent"
	}
	return agentidentity.Wrap(icon+" ", ref, styles.MutedStyle.Render(" · "+label), width)
}

func withError(header string, msg *types.Message, width int) string {
	if msg.ToolStatus != types.ToolStatusError {
		return header
	}
	detail := msg.Content
	if msg.ToolResult != nil && msg.ToolResult.Output != "" {
		detail = msg.ToolResult.Output
	}
	detail = strings.Join(strings.Fields(ansi.Strip(detail)), " ")
	if detail == "" {
		detail = "Subagent operation failed; inspect the request and try again."
	}
	// At most three terminal rows, regardless of result size or embedded controls.
	detail = ansi.Truncate(detail, max(1, width)*3, "…")
	return header + "\n" + styles.MutedStyle.Render(ansi.Hardwrap(detail, max(1, width), true))
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

// Only tool-authored result headers supply display attribution. Preserve the
// complete canonical ID, including when distinct IDs share a visible prefix.
func stampedAttribution(tool, content string) (string, subagent.NodeID, bool) {
	prefix, boundary := "", "."
	switch tool {
	case subagent.ToolSpawnSubagent:
		prefix = "Spawned subagent "
	case subagent.ToolSendMessage:
		prefix = "Message delivered to subagent "
	case subagent.ToolReadSubagent:
		prefix, boundary = "Subagent ", " — "
	case subagent.ToolStopSubagent:
		prefix = "Stopped subagent "
	default:
		return "", "", false
	}
	rest, ok := strings.CutPrefix(content, prefix)
	if !ok {
		return "", "", false
	}
	quoted, err := strconv.QuotedPrefix(rest)
	if err != nil || !strings.HasPrefix(quoted, `"`) {
		return "", "", false
	}
	name, err := strconv.Unquote(quoted)
	if err != nil {
		return "", "", false
	}
	rest, ok = strings.CutPrefix(rest[len(quoted):], " (")
	if !ok {
		return "", "", false
	}
	id, tail, ok := strings.Cut(rest, ")")
	if !ok || id == "" || strings.ContainsAny(id, " \t\r\n()") || !strings.HasPrefix(tail, boundary) {
		return "", "", false
	}
	if boundary == "." && len(tail) > 1 && tail[1] != ' ' {
		return "", "", false
	}
	return name, subagent.NodeID(id), true
}
