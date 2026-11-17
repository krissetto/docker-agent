package tui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
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
	return nil, errors.New("metadata-only test provider does not support chat completion")
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

// filterMetadataRefresh pins the background metadata refresh committed by the
// Update that started at generation gen. The generation delta is the exact
// expected count (0 or 1), and every refresh must carry the committed generation
// and the application it was scheduled for. Every other batched message is
// returned to the caller; nothing is dropped by kind.
func filterMetadataRefresh(t *testing.T, m *appModel, gen uint64, msgs []tea.Msg) []tea.Msg {
	t.Helper()
	delta := m.metadataGeneration - gen
	require.LessOrEqual(t, delta, uint64(1), "one Update schedules at most one metadata refresh")
	var semantic []tea.Msg
	refreshes := 0
	for _, msg := range msgs {
		meta, ok := msg.(capabilityResult)
		if !ok || !meta.refresh {
			semantic = append(semantic, msg)
			continue
		}
		refreshes++
		require.Equal(t, m.metadataGeneration, meta.metadataGeneration, "metadata refresh must carry the committed generation")
		require.Same(t, m.application, meta.scope.application, "metadata refresh must target the committed application")
	}
	require.Equal(t, int(delta), refreshes, "background metadata refresh count must equal the committed generation delta")
	return semantic
}

// requireOnlyMetadataRefresh fails on any batched message other than the
// committed background metadata refresh.
func requireOnlyMetadataRefresh(t *testing.T, m *appModel, gen uint64, cmd tea.Cmd) {
	t.Helper()
	require.Empty(t, filterMetadataRefresh(t, m, gen, collectMsgs(cmd)), "only the background metadata refresh may be batched")
}

// sessionMetadataOf reads metadata only when a session handle is attached; a
// nil handle is a valid state and its absence is pinned by the zero value.
func sessionMetadataOf(application *app.App) runtime.SessionMetadata {
	if handle := application.SessionHandle(); handle != nil {
		return handle.Metadata()
	}
	return runtime.SessionMetadata{}
}

// rootOwnerState pins everything a stale picker result could mutate.
type rootOwnerState struct {
	application    *app.App
	modelPickerApp *app.App
	generation     uint64
	dialogOpen     bool
	metadata       runtime.SessionMetadata
}

func captureRootOwnerState(m *appModel) rootOwnerState {
	return rootOwnerState{application: m.application, modelPickerApp: m.modelPickerApp, generation: m.modelPickerGeneration, dialogOpen: m.dialogMgr.Open(), metadata: sessionMetadataOf(m.application)}
}

func requireRootOwnerState(t *testing.T, m *appModel, want rootOwnerState) {
	t.Helper()
	require.Equal(t, want, captureRootOwnerState(m), "stale result must not mutate the owner, model, metadata, or dialog state")
}

func TestModelPickerLoadedDispatchRejectsStaleOriginAndGeneration(t *testing.T) {
	m, _ := newModelSwitchHandlerTest(t)
	t.Cleanup(m.dialogMgr.Cleanup)
	_, oldCmd := m.handleOpenModelPicker()
	_, currentCmd := m.handleOpenModelPicker()
	old := oldCmd().(messages.ModelPickerLoadedMsg)
	current := currentCmd().(messages.ModelPickerLoadedMsg)

	before, gen := captureRootOwnerState(m), m.metadataGeneration
	_, cmd := m.Update(old)
	requireOnlyMetadataRefresh(t, m, gen, cmd)
	requireRootOwnerState(t, m, before)
	require.False(t, m.dialogMgr.Open())

	wrong := current
	wrong.SessionID = "other-session"
	before, gen = captureRootOwnerState(m), m.metadataGeneration
	_, cmd = m.Update(wrong)
	requireOnlyMetadataRefresh(t, m, gen, cmd)
	requireRootOwnerState(t, m, before)
	require.False(t, m.dialogMgr.Open())

	_, _ = m.Update(current)
	require.True(t, m.dialogMgr.HasDialog(func(d dialog.Dialog) bool { return d != nil }))
	m.dialogMgr.Cleanup()
	m.modelPickerGeneration++
	before, gen = captureRootOwnerState(m), m.metadataGeneration
	_, cmd = m.Update(current)
	requireOnlyMetadataRefresh(t, m, gen, cmd)
	requireRootOwnerState(t, m, before)
	require.False(t, m.dialogMgr.Open())
}
