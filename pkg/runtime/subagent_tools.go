package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

// handleSpawnSubagent starts a declared subagent concurrently and returns
// immediately. The subagent's settled turns are reported to the parent when
// its subtree is quiet; the agent uses read_subagent to inspect details on
// demand.
func (r *LocalRuntime) handleSpawnSubagent(_ context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args subagent.SpawnArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Task) == "" {
		return tools.ResultError("task is required"), nil
	}

	a := r.resolveSessionAgent(sess)
	allowed := r.allowedFromAgent(a)
	ref, ok := subagent.FindAllowed(allowed, strings.TrimSpace(args.Agent))
	if !ok {
		names := make([]string, len(allowed))
		for i, x := range allowed {
			names[i] = x.DisplayName()
		}
		if len(names) == 0 {
			return tools.ResultError(fmt.Sprintf("agent %q is not one of your subagents; you have none configured", args.Agent)), nil
		}
		return tools.ResultError(fmt.Sprintf("agent %q is not one of your subagents. Available: %s", args.Agent, strings.Join(names, ", "))), nil
	}

	id, err := r.subagents.Spawn(sess, a.Name(), ref, args.Task)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf(
		"Spawned subagent %q (%s). It is running concurrently and keeps its session for follow-ups. You will get a status update when it finishes a turn and its subtree is quiet. Continue working or finish your response to wait; do not poll.",
		ref.DisplayName(), id,
	)), nil
}

// handleReadSubagent returns a subagent's status, its latest result (default),
// its last N messages (last_messages), or its full transcript (full).
func (r *LocalRuntime) handleReadSubagent(_ context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args subagent.ReadArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	id := strings.TrimSpace(args.SubagentID)
	if id == "" {
		return tools.ResultError("subagent_id is required"), nil
	}
	rec, err := r.subagents.readChild(sess.ID, subagent.NodeID(id))
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	header := fmt.Sprintf("Subagent %q (%s) — %s", rec.name, id, rec.state)

	// Transcript modes: full or last-N. Available even while the subagent is
	// still running (partial transcript so far).
	if args.Full || args.LastMessages > 0 {
		if rec.session == nil {
			return tools.ResultSuccess(header + "\n\n(no transcript available)"), nil
		}
		limit := 0
		if !args.Full {
			limit = args.LastMessages
		}
		transcript := renderTranscript(rec.session, limit)
		if transcript == "" {
			transcript = "(no messages yet)"
		}
		return tools.ResultSuccess(header + "\n\n" + transcript), nil
	}

	// Default: the latest result (the subagent's most recent finished turn).
	switch {
	case rec.state == subagent.NodeFailed && rec.errMsg != "":
		return tools.ResultSuccess(fmt.Sprintf("%s:\n\n%s", header, rec.errMsg)), nil
	case rec.result != "":
		return tools.ResultSuccess(fmt.Sprintf("%s:\n\n%s", header, rec.result)), nil
	default:
		return tools.ResultSuccess(header + ". No result yet; pass full:true or last_messages:N to see progress."), nil
	}
}

// handleSendMessage delivers an asynchronous message to another agent: the
// reserved target "parent", or one of the caller's running subagents by id.
func (r *LocalRuntime) handleSendMessage(_ context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args subagent.SendArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	to := strings.TrimSpace(args.To)
	if to == "" {
		return tools.ResultError("to is required"), nil
	}

	if to == subagent.ParentAlias {
		if sess.ParentID == "" {
			return tools.ResultError("you have no parent to message"), nil
		}
		// Stamp the sender's node id so consumers (model and TUI) can attribute
		// the message to a specific subagent, matching the turn reports.
		from := fmt.Sprintf("subagent %q", r.resolveSessionAgent(sess).Name())
		if id, ok := r.subagents.nodeForSession(sess.ID); ok {
			from += fmt.Sprintf(" (%s)", id)
		}
		body := systemInfo(fmt.Sprintf("Message from %s:\n\n%s", from, args.Message))
		if !r.subagents.deliverExplicitToParent(sess.ParentID, body) {
			return tools.ResultError("parent is not currently reachable; message was not delivered"), nil
		}
		return tools.ResultSuccess("Message delivered to parent."), nil
	}

	// Parent-to-child messages are ordinary user input: the parent is the
	// child's "user", so no system_info framing — the child's session stays
	// identical in shape to a user-driven session (and viewers render it as
	// such).
	name, err := r.subagents.sendToChild(sess.ID, subagent.NodeID(to), args.Message)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Message delivered to subagent %q (%s).", name, to)), nil
}

// handleStopSubagent explicitly finalizes one of the caller's subagents:
// interrupts any in-flight run and dismisses it (and its own subagents). The
// transcript stays readable via read_subagent.
func (r *LocalRuntime) handleStopSubagent(_ context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args subagent.StopArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	id := strings.TrimSpace(args.SubagentID)
	if id == "" {
		return tools.ResultError("subagent_id is required"), nil
	}
	name, err := r.subagents.stopChild(sess.ID, subagent.NodeID(id))
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Stopped subagent %q (%s).", name, id)), nil
}
