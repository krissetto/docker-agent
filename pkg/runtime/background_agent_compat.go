package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

func (r *LocalRuntime) handleBackgroundRun(_ context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args agenttool.RunBackgroundAgentArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Task) == "" {
		return tools.ResultError("task must not be empty"), nil
	}
	caller := r.resolveSessionAgent(sess)
	if caller == nil {
		return tools.ResultError("no agent resolved for the calling session"), nil
	}
	if denied := validateAgentInList(caller.Name(), args.Agent, "delegate to", "sub-agents list", caller.SubAgents()); denied != nil {
		return denied, nil
	}
	_, guard := validateDelegation(sess, caller.Name(), args.Agent)
	if guard != "" {
		return tools.ResultError(guard), nil
	}
	task := args.Task
	if args.ExpectedOutput != "" {
		task += "\n\nExpected output: " + args.ExpectedOutput
	}
	id, err := r.subagents.Spawn(sess, caller.Name(), subagent.AllowedSubagent{Agent: args.Agent}, task)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Background agent task started with ID: %s\nAgent: %s\nTask: %s", id, args.Agent, args.Task)), nil
}

func (r *LocalRuntime) handleBackgroundList(_ context.Context, sess *session.Session, _ tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var out strings.Builder
	out.WriteString("Background Agent Tasks:\n\n")
	r.subagents.mu.Lock()
	root := r.subagents.rootSessionLocked(sess.ID)
	var ids []subagent.NodeID
	for id, rec := range r.subagents.children {
		if r.subagents.rootSessionLocked(rec.sessionID) == root {
			ids = append(ids, id)
		}
	}
	r.subagents.mu.Unlock()
	for _, id := range ids {
		if rec, ok := r.subagents.Read(id); ok {
			fmt.Fprintf(&out, "ID: %s\n  Agent: %s\n  Status: %s\n\n", id, rec.name, rec.state)
		}
	}
	if len(ids) == 0 {
		out.WriteString("No background agent tasks found.\n")
	}
	return tools.ResultSuccess(out.String()), nil
}

func (r *LocalRuntime) handleBackgroundView(_ context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args agenttool.ViewBackgroundAgentArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	id := strings.TrimSpace(args.TaskID)
	if id == "" {
		return tools.ResultError("subagent_id is required"), nil
	}
	rec, err := r.subagents.readRootChild(sess.ID, subagent.NodeID(id))
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return readSubagentResult(rec, id, subagent.ReadArgs{Full: true}), nil
}

func (r *LocalRuntime) handleBackgroundStop(_ context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args agenttool.StopBackgroundAgentArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	id := strings.TrimSpace(args.TaskID)
	if id == "" {
		return tools.ResultError("subagent_id is required"), nil
	}
	name, err := r.subagents.stopChildContext(r.subagents.ctx, sess.ID, subagent.NodeID(id))
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Stopped subagent %q (%s).", name, id)), nil
}
