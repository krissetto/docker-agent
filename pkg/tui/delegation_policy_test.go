package tui

import (
	"context"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// delegationHandle holds a canonical session-tree "Use subagents" policy.
type delegationHandle struct {
	*lifecycleHandle

	enabled bool
	sets    []bool
}

func (h *delegationHandle) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: h.id, AgentName: "root", Capabilities: runtime.SessionCapabilities{DelegationPolicy: true}}
}

func (h *delegationHandle) DelegationPolicy(context.Context) (bool, error) { return h.enabled, nil }
func (h *delegationHandle) SetDelegationPolicy(_ context.Context, enabled bool) error {
	h.sets = append(h.sets, enabled)
	h.enabled = enabled
	return nil
}

type delegationSessions struct{ openSubagentSessions }

func (delegationSessions) SessionByID(string) (runtime.SessionHandle, error) { return nil, nil }

func TestUseSubagentsChangesSessionTreePolicyNotSavedDefault(t *testing.T) {
	setupSettingsConfigTest(t)
	t.Setenv("HOME", t.TempDir())
	handle := &delegationHandle{lifecycleHandle: &lifecycleHandle{id: "tree-root"}, enabled: true}
	sess := session.New(session.WithID("tree-root"), session.WithAgentName("root"))
	a, err := app.NewResolved(t.Context(), &delegationSessions{}, runtime.CommittedSessionView{SessionHandle: handle, Info: runtime.PreparedSessionViewInfo{SessionID: sess.ID, Session: sess, Binding: runtime.SessionBinding{AgentName: "root"}}}, app.WithRuntimeServices(stubRuntime{}))
	require.NoError(t, err)
	require.True(t, a.CanSetDelegationPolicy())
	d := dialog.NewSubagentsDialog(nil, nil)
	d.SetSize(100, 25)
	d.Update(dialog.SubagentsPolicyMsg{SessionTree: true, Pending: true})
	require.Contains(t, ansi.Strip(d.View()), "Use subagents (this session tree): …", "the effective value is read asynchronously")
	m := &appModel{ctx: t.Context, application: a, panelData: map[string]*panelSessionData{"tree-root": {application: a, treeDialog: d}}}

	read, ok := m.queryDelegationPolicy(a)().(delegationPolicyMsg)
	require.True(t, ok)
	_ = collectMsgs(m.applyDelegationPolicy(read))
	assert.Contains(t, ansi.Strip(d.View()), "Use subagents (this session tree): ON")

	set, ok := m.setUseSubagents(false)().(delegationPolicyMsg)
	require.True(t, ok)
	notice, ok := firstOfType[notification.ShowMsg](collectMsgs(m.applyDelegationPolicy(set)))
	require.True(t, ok)
	assert.Equal(t, "Use subagents for this session tree: OFF (new delegation only; existing work continues)", notice.Text)
	assert.Equal(t, []bool{false}, handle.sets)
	assert.Contains(t, ansi.Strip(d.View()), "Use subagents (this session tree): OFF")
	assert.True(t, userconfig.Get().GetUseSubagents(), "a session-tree change never rewrites the saved local default")
}
