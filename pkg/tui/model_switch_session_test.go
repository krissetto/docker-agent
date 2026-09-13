package tui

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type tuiModelProvider struct{ cfg latest.ModelConfig }

func (p tuiModelProvider) ID() modelsdev.ID        { return modelsdev.NewID(p.cfg.Provider, p.cfg.Model) }
func (p tuiModelProvider) BaseConfig() base.Config { return base.Config{ModelConfig: p.cfg} }
func (tuiModelProvider) MaxTokens() int            { return 0 }
func (tuiModelProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, nil
}

func newModelSwitchHandlerTest(t *testing.T) (*appModel, *agent.Agent) {
	t.Helper()
	worker := agent.New("worker", "prompt", agent.WithModel(tuiModelProvider{cfg: latest.ModelConfig{Provider: "test", Model: "default"}}))
	registry := provider.NewRegistry(map[string]provider.Factory{
		"test": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
			return tuiModelProvider{cfg: *cfg}, nil
		},
	})
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(worker)), runtime.WithModelSwitcherConfig(&runtime.ModelSwitcherConfig{
		Models:             map[string]latest.ModelConfig{"other": {Provider: "test", Model: "other"}},
		AgentDefaultModels: map[string]string{"worker": "default"},
		ProviderRegistry:   registry,
		EnvProvider:        environment.NewMapEnvProvider(nil),
	}))
	require.NoError(t, err)
	sess := session.New(session.WithAgentName("worker"))
	a := app.New(t.Context(), rt, sess, runtime.SessionBinding{}, app.WithRuntimeServices(rt))
	m, _ := newTestModel(t)
	m.application = a
	return m, worker
}

func TestModelSwitchHandlersSupportedOpenSelectReset(t *testing.T) {
	m, worker := newModelSwitchHandlerTest(t)

	_, cmd := m.handleOpenModelPicker()
	loaded, ok := cmd().(messages.ModelPickerLoadedMsg)
	require.True(t, ok)
	_, _ = m.Update(loaded)
	assert.True(t, m.dialogMgr.Open())
	t.Cleanup(m.dialogMgr.Cleanup)

	_, cmd = m.handleChangeModel("other")
	require.NotNil(t, cmd)
	assert.Equal(t, "other", m.application.SessionHandle().Metadata().Model)
	assert.False(t, worker.HasModelOverride())
	assert.Contains(t, notificationTexts(collectMsgs(cmd)), "Model changed to other")

	_, cmd = m.handleChangeModel("")
	assert.Empty(t, m.application.SessionHandle().Metadata().Model)
	assert.False(t, worker.HasModelOverride())
	assert.Contains(t, notificationTexts(collectMsgs(cmd)), "Model reset to default")
}

func TestModelSwitchHandlersUnsupportedNotice(t *testing.T) {
	m, _ := newTestModel(t)
	m.application = app.New(t.Context(), nil, session.New(), runtime.SessionBinding{})

	_, cmd := m.handleOpenModelPicker()
	msgs := collectMsgs(cmd)
	note, ok := firstOfType[notification.ShowMsg](msgs)
	require.True(t, ok)
	assert.Contains(t, note.Text, "unavailable")

	_, cmd = m.handleChangeModel("other")
	note, ok = firstOfType[notification.ShowMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Contains(t, note.Text, "unavailable")
}

func TestModelPickerLoadedDispatchRejectsStaleOriginAndGeneration(t *testing.T) {
	m, _ := newModelSwitchHandlerTest(t)
	t.Cleanup(m.dialogMgr.Cleanup)
	_, oldCmd := m.handleOpenModelPicker()
	_, currentCmd := m.handleOpenModelPicker()
	old := oldCmd().(messages.ModelPickerLoadedMsg)
	current := currentCmd().(messages.ModelPickerLoadedMsg)
	_, cmd := m.Update(old)
	require.Nil(t, cmd)
	require.False(t, m.dialogMgr.Open())
	wrong := current
	wrong.SessionID = "other-session"
	_, cmd = m.Update(wrong)
	require.Nil(t, cmd)
	require.False(t, m.dialogMgr.Open())
	_, _ = m.Update(current)
	require.True(t, m.dialogMgr.HasDialog(func(d dialog.Dialog) bool { return d != nil }))
	m.dialogMgr.Cleanup()
	m.modelPickerGeneration++
	_, cmd = m.Update(current)
	require.Nil(t, cmd)
	require.False(t, m.dialogMgr.Open())
}
