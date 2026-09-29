package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestRunningPaneRequiresPaintedAnimatedHeader(t *testing.T) {
	for _, mode := range []string{"split", "single", "collapsed", "lean", "hidden", "attention", "paused", "pausing", "dormant"} {
		t.Run(mode, func(t *testing.T) {
			root := splitTestRoot(t)
			root.splitPane("second", "profile", splitRight)
			root.tabInfos = []messages.TabInfo{{SessionID: "profile", Activity: messages.TabActivityRunning}}
			switch mode {
			case "single":
				root.singlePane()
			case "collapsed":
				root.handleWindowResize(1, 1)
			case "lean":
				root.leanMode = true
			case "hidden":
				root.tabInfos[0].SessionID = "third"
			case "attention":
				root.tabInfos[0].NeedsAttention = true
			case "paused":
				root.sessionStates["profile"].SetPauseState(service.PausePaused)
			case "pausing":
				root.sessionStates["profile"].SetPauseState(service.PausePausing)
			case "dormant":
				root.composingPaneStatuses = map[string]runtime.SessionStatus{"profile": {Dormant: true}}
			}
			require.Equal(t, mode == "split", root.hasRunningPane())
			running := root.hasRunningPane()
			if running {
				require.NotEmpty(t, root.paneActivityFrame())
			} else {
				require.Empty(t, root.paneActivityFrame())
			}
		})
	}
}

func TestPaneActivityCardTicksMatchCompleteFreshView(t *testing.T) {
	for _, single := range []bool{true, false} {
		t.Run(map[bool]string{true: "single", false: "split"}[single], func(t *testing.T) {
			root, _, scheduler := paneReplayRoot(t)
			if single {
				root.singlePane()
			}
			root.Update(runtime.StreamStarted("profile", "root"))
			tabs, active := root.supervisor.GetTabs()
			for i := range tabs {
				if tabs[i].SessionID == "profile" {
					tabs[i].Activity = messages.TabActivityRunning
				}
			}
			root.Update(messages.TabsUpdatedMsg{Tabs: tabs, ActiveIdx: active})
			for range 240 {
				require.NotEmpty(t, scheduler.pending)
				cmd := scheduler.pending[0]
				scheduler.pending = scheduler.pending[1:]
				root.Update(cmd())
				root.View()
			}
			scheduler.step = 16666667 * time.Nanosecond
			cardOnly := 0
			for range 120 {
				beforeTime, before := root.ar.Now(), root.View()
				require.NotEmpty(t, scheduler.pending)
				cmd := scheduler.pending[0]
				scheduler.pending = scheduler.pending[1:]
				msg := cmd()
				require.IsType(t, animation.TickMsg{}, msg)
				root.Update(msg)
				afterTime := root.ar.Now()
				isCardOnly := animation.Card.FrameIndexAt(beforeTime) != animation.Card.FrameIndexAt(afterTime) && animation.Chat.FrameIndexAt(beforeTime) == animation.Chat.FrameIndexAt(afterTime)
				if isCardOnly {
					cardOnly++
					require.Equal(t, single, root.viewCacheValid, "only a painted split header invalidates Card-only ticks")
				}
				cached := root.View()
				root.viewCacheValid = false
				root.paneRenderCache = paneRenderCache{}
				root.paneTitleCache = nil
				fresh := root.View()
				require.Equal(t, fresh, cached, "complete tea.View includes cursor, colors, and terminal metadata")
				if isCardOnly {
					if single {
						require.Equal(t, before, cached)
					} else {
						require.NotEqual(t, before.Content, cached.Content)
					}
				}
			}
			require.Positive(t, cardOnly)
		})
	}
}
