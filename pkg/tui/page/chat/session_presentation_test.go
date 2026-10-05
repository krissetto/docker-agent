package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSessionPresentationPreturnResetAndTitleTerminalRouting(t *testing.T) {
	sess := session.New(session.WithID("root-session"), session.WithAgentName("root"))
	application, _ := newSessionTestApp(t, sess, nil, nil)
	page := New(animation.NewRuntime(), t.Context(), application, service.NewSessionState(sess)).(*chatPage)
	page.sidebar.SetSize(70, 25)
	snapshot := runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: sess.ID, AgentName: "root", State: runtime.SessionStateSettled}, Presentation: []runtime.Event{
		runtime.AgentInfo("root", "test/model", "description", "", 100000),
		runtime.TeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "test", ModelID: "model", ModelName: "Friendly full model", ThinkingMode: "effort", ThinkingLevel: "high"}}, "root"),
	}}
	page.applyProjection(snapshot, true)
	before := ansi.Strip(page.sidebar.View())
	require.Contains(t, before, "root")
	require.Contains(t, before, "Friendly full model")
	require.Contains(t, before, "high")
	page.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
	require.NotContains(t, ansi.Strip(page.sidebar.View()), "Generating title")
	started := runtime.SessionTitle(sess.ID, "").(*runtime.SessionTitleEvent)
	started.Status = "started"
	page.handleRuntimeEvent(started)
	require.Contains(t, ansi.Strip(page.sidebar.View()), "Generating title")
	child := runtime.SessionTitle("child", "Foreign title").(*runtime.SessionTitleEvent)
	child.Status = "completed"
	page.handleRuntimeEvent(child)
	require.Contains(t, ansi.Strip(page.sidebar.View()), "Generating title")
	require.NotContains(t, ansi.Strip(page.sidebar.View()), "Foreign title")
	for _, terminal := range []string{"failed", "canceled", "completed"} {
		page.handleRuntimeEvent(started)
		event := runtime.SessionTitle(sess.ID, "").(*runtime.SessionTitleEvent)
		event.Status = terminal
		if terminal == "completed" {
			event.Title = "Successful title"
		}
		page.handleRuntimeEvent(event)
		require.NotContains(t, ansi.Strip(page.sidebar.View()), "Generating title")
	}
	snapshot.TitleStatus = "started"
	page.applyProjection(snapshot, true)
	require.Contains(t, ansi.Strip(page.sidebar.View()), "Generating title")
	snapshot.TitleStatus = "failed"
	page.applyProjection(snapshot, true)
	require.NotContains(t, ansi.Strip(page.sidebar.View()), "Generating title")
}
