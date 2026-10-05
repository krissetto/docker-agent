package runtime

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

var errSubagentsDisabled = errors.New("autonomous subagent delegation is disabled; complete the task directly")

// WithUseSubagents controls new autonomous delegation. It defaults to true.
func WithUseSubagents(enabled bool) Opt {
	return func(r *LocalRuntime) { r.SetUseSubagents(enabled) }
}

// UseSubagents reports whether new autonomous delegation is enabled. The zero
// value is enabled, including runtimes constructed without NewLocalRuntime.
func (r *LocalRuntime) UseSubagents() bool { return !r.subagentsDisabled.Load() }

// SetUseSubagents changes admission of new autonomous work, not the lifecycle
// of existing sessions. Accepted input, completion reports and manual user
// interaction continue; no child is canceled, deleted or revived by this call.
func (r *LocalRuntime) SetUseSubagents(enabled bool) {
	r.subagentAdmissionMu.Lock()
	defer r.subagentAdmissionMu.Unlock()
	r.subagentsDisabled.Store(!enabled)
}

func (r *LocalRuntime) acceptSessionDelegation(sess *session.Session) bool {
	r.subagentAdmissionMu.RLock()
	defer r.subagentAdmissionMu.RUnlock()
	return r.sessionDelegationEnabled(sess)
}

func (r *LocalRuntime) filterDelegationTools(agentTools []tools.Tool) []tools.Tool {
	return r.filterSessionDelegationTools(nil, agentTools)
}

func (r *LocalRuntime) filterSessionDelegationTools(sess *session.Session, agentTools []tools.Tool) []tools.Tool {
	if r.sessionDelegationEnabled(sess) {
		return agentTools
	}
	return filterExcludedTools(agentTools, []string{
		subagent.ToolSpawnSubagent,
		agenttool.ToolNameRunBackgroundAgent,
		transfertask.ToolNameTransferTask,
	})
}

// Session assembly owns the generated instruction prefix. Remove only exact
// generated messages from that prefix, never user/skill text containing a tool
// name, and never mutate the shared agent or persisted session instructions.
func (r *LocalRuntime) filterDelegationMessages(a *agent.Agent, sess *session.Session, messages []chat.Message) []chat.Message {
	if r.sessionDelegationEnabled(sess) {
		return messages
	}
	generated := map[string]bool{}
	if a.HasAsyncSubagents() && a.AsyncHarnessPrompt() != "" {
		generated[a.AsyncHarnessPrompt()] = true
	}
	if a.HasSubAgents() {
		var text strings.Builder
		for _, child := range a.SubAgents() {
			fmt.Fprintf(&text, "Name: %s | Description: %s\n", child.Name(), child.Description())
		}
		generated["Use transfer_task only with one of the listed agent IDs: "+strings.Join(agentNames(a.SubAgents()), ", ")+". Delegate when another listed agent is best suited, or answer directly when you are. When delegating, emit only the transfer_task tool call. In task, directly include the relevant context, constraints, absolute file paths, and expected output.\n\nAvailable agents:\n"+text.String()] = true
	}
	backgroundInstructions := tools.GetInstructions(agenttool.New())
	for _, ts := range a.ToolSets() {
		if tools.GetInstructions(ts) == backgroundInstructions {
			generated[backgroundInstructions] = true
		}
	}
	filtered := make([]chat.Message, 0, len(messages)+1)
	prefix := true
	for _, message := range messages {
		if message.Role != chat.MessageRoleSystem {
			prefix = false
		}
		if prefix && generated[message.Content] {
			delete(generated, message.Content)
			continue
		}
		filtered = append(filtered, message)
	}
	// Keep this live policy note out of the frozen instruction context. It also
	// clarifies child messaging without removing send_message's parent route.
	note := "Autonomous subagent delegation is disabled. Do not spawn subagents, transfer tasks, run background agents, or send new work to children. Complete work directly. You may inspect or stop existing children; already accepted work and completion reports continue."
	if sess.ParentID != "" {
		note += " You may still use send_message to contact your parent."
	}
	for _, message := range filtered {
		if message.Role == chat.MessageRoleSystem && message.Content == note {
			return filtered
		}
	}
	// Insert before conversation history, preserving the order of all existing
	// messages and their cache-control flags.
	index := 0
	for index < len(filtered) && filtered[index].Role == chat.MessageRoleSystem {
		index++
	}
	return slices.Insert(filtered, index, chat.Message{Role: chat.MessageRoleSystem, Content: note})
}
