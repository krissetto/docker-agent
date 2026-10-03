package leantui

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/userconfig"
)

var elicitationTestSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"name":   map[string]any{"type": "string", "title": "Full name"},
		"age":    map[string]any{"type": "integer", "minimum": float64(0)},
		"agree":  map[string]any{"type": "boolean"},
		"colour": map[string]any{"type": "string", "enum": []any{"red", "blue"}, "default": "blue"},
	},
	"required": []any{"name", "agree"},
}

func TestLeanElicitationTypedAnswers(t *testing.T) {
	content, err := parseElicitationAnswer(elicitationTestSchema, `name="Ada Lovelace" age=36 agree=yes`)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"name": "Ada Lovelace", "age": int64(36), "agree": true, "colour": "blue"}, content, "typed values; shown default fills only the omitted field")

	_, err = parseElicitationAnswer(elicitationTestSchema, `name=Ada age=old colour=green extra=1`)
	require.Error(t, err)
	for _, problem := range []string{"agree: required", "age: Must be a whole number", "colour: Invalid selection", "extra: unknown field"} {
		assert.Contains(t, err.Error(), problem)
	}

	content, err = parseElicitationAnswer(elicitationTestSchema, `{"name":"Ada","agree":false}`)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"name": "Ada", "agree": false}, content, "JSON stays available verbatim")

	content, err = parseElicitationAnswer(nil, "free form answer")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"response": "free form answer"}, content)

	_, err = parseElicitationAnswer(elicitationTestSchema, `name="unterminated`)
	require.Error(t, err)

	lines := strings.Join(elicitationFieldLines(elicitationTestSchema), "\n")
	assert.Contains(t, lines, "name (string, required) Full name")
	assert.Contains(t, lines, "colour (one of red | blue) [default blue]")
	assert.NotContains(t, lines, "{", "fields render as prompts, not raw schema JSON")
}

func TestLeanElicitationRespondSendsTypedContent(t *testing.T) {
	m, handle := sessionModel(t)
	sessionID := m.app.Session().ID
	m.handleEvent(t.Context(), &runtime.ElicitationRequestEvent{SessionID: sessionID, RequestID: "ask", ElicitationID: "elicit", Message: "Who are you?", Schema: elicitationTestSchema})
	text := strings.Join(m.screen.Transcript.Lines(120, 0, false, nil, nil), "\n")
	assert.Contains(t, text, "name=value")
	assert.Contains(t, text, "agree (boolean, required)")

	m.respondInteraction(t.Context(), `ask name=Ada`)
	assert.Empty(t, handle.responses, "missing required field is rejected locally")
	m.respondInteraction(t.Context(), `ask name=Ada agree=no`)
	require.Len(t, handle.responses, 1)
	response := handle.responses[0]
	assert.Equal(t, runtime.InteractionElicitation, response.Kind)
	assert.Equal(t, "elicit", response.ElicitationID)
	assert.Equal(t, map[string]any{"name": "Ada", "agree": false, "colour": "blue"}, response.Elicitation.Content)
}

func TestLeanTreeAttentionNoticeStatusAndCommand(t *testing.T) {
	m, _ := sessionModel(t)
	root := m.app.Session().ID
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: subagent.SessionRootID(root), SessionID: root},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "c0ffe", Parent: subagent.SessionRootID(root), SessionID: "child", Name: "Reviewer", NeedsAttention: true, WaitingOn: "approve tool"}}},
	}}}
	m.handleEvent(t.Context(), &runtime.SubagentTreeEvent{Snapshot: tree})
	m.handleEvent(t.Context(), &runtime.SubagentTreeEvent{Snapshot: tree})
	text := strings.Join(m.screen.Transcript.Lines(120, 0, false, nil, nil), "\n")
	assert.Equal(t, 1, strings.Count(text, "Reviewer is waiting to approve tool"), "repeated topology does not repeat the notice")
	assert.Equal(t, 1, m.status.Attention)
	assert.Contains(t, strings.Join(ui.RenderStatus(m.status, 120), "\n"), "1 waiting · /attention")

	tree.Nodes[0].Children[0].Node.NeedsAttention, tree.Nodes[0].Children[0].Node.WaitingOn = false, ""
	m.handleEvent(t.Context(), &runtime.SubagentTreeEvent{Snapshot: tree})
	assert.Zero(t, m.status.Attention)
}

func TestLeanStatusShowsConnectionAndRecoveryUncertainty(t *testing.T) {
	m, _ := sessionModel(t)
	m.handleEvent(t.Context(), &app.ConnectionStateEvent{State: app.ConnectionReconnecting})
	status := strings.Join(ui.RenderStatus(m.status, 120), "\n")
	assert.Contains(t, status, "Connection reconnecting · work continues on the server")
	m.handleEvent(t.Context(), &app.ConnectionStateEvent{State: app.ConnectionConnected})
	assert.NotContains(t, strings.Join(ui.RenderStatus(m.status, 120), "\n"), "Connection")

	m.handleEvent(t.Context(), &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: m.app.Session(), Status: runtime.SessionStatus{SessionID: m.app.Session().ID, State: runtime.SessionStateSettled, InterruptedTurns: 1}}})
	status = strings.Join(ui.RenderStatus(m.status, 120), "\n")
	assert.Contains(t, status, "recovery uncertain (1 interrupted)")
	assert.NotContains(t, status, "· ready", "uncertain recovery is not presented as a normal idle")
	assert.Contains(t, strings.Join(m.screen.Transcript.Lines(160, 0, false, nil, nil), "\n"), "Recovery uncertain: 1 turn was interrupted")
}

// leanDelegationHandle holds a canonical session-tree "Use subagents" policy.
type leanDelegationHandle struct {
	*leanSession

	enabled bool
	sets    []bool
}

func (h *leanDelegationHandle) Metadata() runtime.SessionMetadata {
	metadata := h.leanSession.Metadata()
	metadata.Capabilities.DelegationPolicy = true
	return metadata
}

func (h *leanDelegationHandle) DelegationPolicy(context.Context) (bool, error) { return h.enabled, nil }

func (h *leanDelegationHandle) SetDelegationPolicy(_ context.Context, enabled bool) error {
	h.sets = append(h.sets, enabled)
	h.enabled = enabled
	return nil
}

func TestLeanUseSubagentsTogglesSessionTreePolicyNotSavedDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })
	m, _ := sessionModel(t)
	handle := &leanDelegationHandle{leanSession: &leanSession{id: "tree-root"}, enabled: false}
	m.app = app.New(t.Context(), &blockingTreeSessions{handles: map[string]runtime.SessionHandle{"tree-root": handle}}, session.New(session.WithID("tree-root")), runtime.SessionBinding{AgentName: "agent"}, app.WithRuntimeServices(m.app.Runtime()))
	require.True(t, m.app.CanSetDelegationPolicy())

	m.openSubagentPicker(t.Context())
	picker := m.screen.Subagents
	require.NotNil(t, picker)
	assert.False(t, picker.PolicyPending)
	assert.False(t, picker.UseSubagents, "the effective tree policy, not the saved default, is shown")
	assert.Contains(t, strings.Join(picker.Render(120, 12), "\n"), "Use subagents (this session tree): OFF")

	m.handleSubagentPickerKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune("u")})
	assert.Equal(t, []bool{true}, handle.sets)
	assert.True(t, picker.UseSubagents)
	assert.Contains(t, strings.Join(m.screen.Transcript.Lines(160, 0, false, nil, nil), "\n"), "Use subagents for this session tree: ON (new delegation only; existing work continues)")
	_, err := os.Stat(userconfig.Path())
	assert.True(t, os.IsNotExist(err), "a session-tree change never writes the saved local default")
}
