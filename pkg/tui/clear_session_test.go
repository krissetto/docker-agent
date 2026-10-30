package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestClearSessionPersistsNewConversation(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() {
		paths.SetDataDir("")
		paths.SetConfigDir("")
	})
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{RestoreTabs: new(false), ShowBanner: new(false)}
		return nil
	}))

	workingDir := t.TempDir()
	sess := session.New(session.WithID("old"), session.WithAgentName("root"), session.WithWorkingDir(workingDir))
	application := app.New(t.Context(), nil, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(stubRuntime{}))
	m := New(t.Context(), nil, application, workingDir, nil, WithSupervisor(supervisor.New(nil))).(*appModel)

	store := m.tuiStore
	require.NotNil(t, store)
	tabID := m.supervisor.ActiveID()
	oldID := m.application.Session().ID

	_, _ = m.handleClearSession()

	newID := m.application.Session().ID
	require.NotEqual(t, oldID, newID)
	assert.Equal(t, tabID, m.supervisor.ActiveID(), "clearing preserves the routing identity")
	tabs, activeID, err := store.GetTabs(t.Context())
	require.NoError(t, err)
	require.Len(t, tabs, 1)
	assert.Equal(t, newID, tabs[0].SessionID)
	assert.Equal(t, newID, activeID)
}
