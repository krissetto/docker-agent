package messages

import (
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSettingsPreviewInvalidatesTranscriptWithoutTogglingState(t *testing.T) {
	state := &service.SessionState{}
	m := NewScrollableView(animation.NewRuntime(), 40, 5, state).(*model)
	t.Cleanup(m.StopAnimations)
	view := &countedHistoryView{content: "before preview"}
	m.messages = append(m.messages, types.User(view.content))
	m.views = append(m.views, view)
	first := m.View()
	state.SetHideToolResults(true)
	view.content = "after preview"
	m.Update(msgtypes.SessionToggleChangedMsg{})
	require.True(t, state.HideToolResults(), "invalidation must not toggle the new preference")
	require.NotEqual(t, first, m.View())
	require.Contains(t, m.View(), "after preview")
	state.SetHideToolResults(false)
	m.Update(msgtypes.SessionToggleChangedMsg{})
	require.False(t, state.HideToolResults(), "rollback invalidation is non-toggling too")
}
