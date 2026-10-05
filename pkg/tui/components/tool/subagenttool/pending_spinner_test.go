package subagenttool

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestPendingSendRetainsIdentityWithoutFreezingSpinner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ar := animation.NewRuntime()
		defer ar.Stop()
		msg := testMessage(subagent.ToolSendMessage, `{"to":"child","message":"prompt`, "", types.ToolStatusPending)
		view := NewSend(ar, msg, nil, func(id subagent.NodeID) (string, bool) { return "worker", id == "child" })
		view.Init()
		collapsed := view.(layout.CollapsedViewer)
		first, firstCollapsed := view.View(), collapsed.CollapsedView()
		require.Contains(t, ansi.Strip(first), "Messaging worker")
		msg.ToolCall.Function.Arguments += `\`
		require.Equal(t, first, view.View(), "temporarily unparseable arguments retain identity")
		require.Equal(t, firstCollapsed, collapsed.CollapsedView())
		_, ok := ar.Accept(ar.Continue()().(animation.TickMsg))
		require.True(t, ok)
		time.Sleep(200 * time.Millisecond) //nolint:forbidigo // Advances synctest's fake clock.
		tick, ok := ar.Accept(ar.Continue()().(animation.TickMsg))
		require.True(t, ok)
		view.Update(tick)
		require.NotEqual(t, first, view.View(), "retained header must advance its spinner")
		require.NotEqual(t, firstCollapsed, collapsed.CollapsedView())
		require.Contains(t, ansi.Strip(view.View()), "Messaging worker")
		require.Equal(t, strings.TrimSpace(strings.TrimSuffix(ansi.Strip(view.View()), ">")), strings.TrimSpace(ansi.Strip(collapsed.CollapsedView())))
	})
}
