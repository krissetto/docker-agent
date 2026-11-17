package messages

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// A collapsed Work block previews the newest streamed reasoning lines and keeps
// refreshing them as reasoning arrives.
func TestCollapsedWorkPreviewFollowsStreamedReasoning(t *testing.T) {
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 100, 80, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetPosition(0, 0)
	m.SetCompactWorkLog(true)
	m.SetWorkStatus(true, 0)
	m.addMessage(types.User("request"))
	m.AppendReasoning("root", "one\n\ntwo\n\nthree")
	call := tools.ToolCall{ID: "preview-tool", Function: tools.FunctionCall{Name: "probe"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "probe"}, types.ToolStatusCompleted)
	m.AppendReasoning("root", "\n\nfour")
	m.workProjectionDirty = true
	m.syncWorkProjection()
	work := firstWorkBlock(m)
	require.NotNil(t, work)
	require.False(t, work.Expanded())

	view := ansi.Strip(work.View())
	require.Contains(t, view, "four")
	require.NotContains(t, view, "one")

	m.AppendReasoning("root", "\n\nfive")
	view = ansi.Strip(work.View())
	require.Contains(t, view, "five", "streamed reasoning refreshes the collapsed preview")
	require.NotContains(t, view, "two")
}
