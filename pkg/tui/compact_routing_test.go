package tui

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type compactTestServices struct{}

// stubRuntime is a narrow presentation-services fixture, not a classic runtime.
type stubRuntime = compactTestServices

func (compactTestServices) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}

func (compactTestServices) CurrentAgentTools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (compactTestServices) CurrentAgentToolsetStatuses() []tools.ToolsetStatus { return nil }

func (compactTestServices) RestartToolset(context.Context, string) error                         { return nil }
func (compactTestServices) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {}
func (compactTestServices) EmitAgentInfo(context.Context, runtime.EventSink)                     {}
func (compactTestServices) ResetStartupInfo()                                                    {}
func (compactTestServices) SessionStore() session.Store                                          { return nil }

func (compactTestServices) PermissionsInfo() *runtime.PermissionsInfo { return nil }

func (compactTestServices) CurrentAgentSkillsToolset() *skillstool.ToolSet { return nil }

func (compactTestServices) CurrentMCPPrompts(context.Context) map[string]tools.PromptInfo { return nil }

func (compactTestServices) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (compactTestServices) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return nil
}
func (compactTestServices) OnToolsChanged(func(runtime.Event))    {}
func (compactTestServices) OnBackgroundEvent(func(runtime.Event)) {}

func newCompactTestModel(t *testing.T, args ...any) (*appModel, *mockChatPage) {
	t.Helper()
	var sess *session.Session
	for _, arg := range args {
		if candidate, ok := arg.(*session.Session); ok {
			sess = candidate
		}
	}
	m, _ := newTestModel(t)
	m.application = app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(compactTestServices{}))
	return m, m.chatPage.(*mockChatPage)
}

func TestHandleCompactSession_EmptyTargetUsesRootPath(t *testing.T) {
	t.Parallel()

	m, page := newCompactTestModel(t, session.New())

	_, _ = m.Update(messages.CompactSessionMsg{AdditionalPrompt: "focus on code"})

	assert.Equal(t, []string{"focus on code"}, page.compactCalls,
		"/compact routes through the chat page's root compaction")
}

func TestHandleCompactSession_RootSessionIDUsesRootPath(t *testing.T) {
	t.Parallel()

	sess := session.New()
	m, page := newCompactTestModel(t, sess)

	_, _ = m.Update(messages.CompactSessionMsg{SessionID: sess.ID, AgentName: "root"})

	assert.Len(t, page.compactCalls, 1, "the main /context row routes through the root compaction path")
}

func TestHandleCompactSession_UnsupportedRuntimeNotifies(t *testing.T) {
	t.Parallel()

	m, page := newCompactTestModel(t, session.New())

	_, cmd := m.Update(messages.CompactSessionMsg{SessionID: "child-1"})

	assert.Empty(t, page.compactCalls)
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	note, ok := msgs[0].(notification.ShowMsg)
	require.True(t, ok, "expected a notification, got %T", msgs[0])
	assert.Equal(t, notification.TypeError, note.Type)
	assert.Contains(t, note.Text, "not supported")
}
