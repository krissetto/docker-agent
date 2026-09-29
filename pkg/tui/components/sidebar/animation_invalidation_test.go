package sidebar

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func newAnimationInvalidationSidebar(t *testing.T) (*model, *placementClock) {
	t.Helper()
	clock := &placementClock{now: time.Unix(1, 0), step: 10 * time.Millisecond}
	state := &service.SessionState{}
	state.SetCurrentAgentName("root")
	m := New(animation.NewRuntimeWithScheduler(clock), t.Context(), state).(*model)
	t.Cleanup(m.StopAnimation)
	m.SetSize(64, 40)
	m.CancelPresentation()
	return m, clock
}

func acceptSidebarTick(t *testing.T, m *model) animation.TickMsg {
	t.Helper()
	cmd := m.ar.Continue()
	require.NotNil(t, cmd)
	tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	return tick
}

func TestSpinnerInvalidationTracksLocalGlyph(t *testing.T) {
	for _, source := range []string{"title", "tools", "working", "rag", "subagents", "collapsed-subagents"} {
		t.Run(source, func(t *testing.T) {
			m, clock := newAnimationInvalidationSidebar(t)
			switch source {
			case "title":
				m.SetTitleRegenerating(true)
			case "tools":
				m.Update(&runtime.ToolsetInfoEvent{Loading: true})
			case "working":
				m.workingAgent = "root"
				m.startSpinner()
			case "rag":
				m.Update(&runtime.RAGIndexingStartedEvent{RAGName: "docs", StrategyName: "local"})
			default:
				m.rootSessionID = "collapse"
				m.treeCollapsed = source == "collapsed-subagents"
				m.SetSubagentTree(collapseFixture())
			}
			m.ReconcileLayout()
			m.CancelPresentation()
			m.ReconcileLayout()
			previousView := m.View()
			require.True(t, m.ar.HasActive())

			// Messages unrelated to animation must not dirty a warm sidebar just
			// because it has a running spinner.
			for _, msg := range []tea.Msg{struct{}{}, tea.KeyPressMsg{Code: 'x', Text: "x"}} {
				invalidations, renders := m.CacheStats()
				generation := m.VisualGeneration()
				m.Update(msg)
				require.Equal(t, previousView, m.View())
				gotInvalidations, gotRenders := m.CacheStats()
				require.Equal(t, invalidations, gotInvalidations)
				require.Equal(t, renders, gotRenders)
				require.Equal(t, generation, m.VisualGeneration())
			}

			changedTicks := 0
			for i := range 60 {
				tick := acceptSidebarTick(t, m)
				before, after := tick.ElapsedBounds()
				changed := animation.Chat.FrameAt(before) != animation.Chat.FrameAt(after)
				// A different owner can already have dirtied the shared tick. This
				// cannot make an unchanged sidebar spinner invalidate its cache.
				alreadyDirty := i%2 == 0
				if alreadyDirty {
					tick.MarkDirty()
				}
				invalidations, renders := m.CacheStats()
				generation := m.VisualGeneration()
				m.Update(tick)
				require.Equal(t, alreadyDirty || changed, tick.Dirty())
				view := m.View()
				gotInvalidations, gotRenders := m.CacheStats()
				if changed {
					changedTicks++
					require.Equal(t, invalidations+1, gotInvalidations)
					require.Equal(t, generation+1, m.VisualGeneration())
					require.Greater(t, gotRenders, renders)
					if source == "title" || source == "subagents" || source == "collapsed-subagents" {
						require.NotEqual(t, previousView, view, "visible spinner glyph must advance")
					}
					require.False(t, m.layoutDirty, "fixed-width frames preserve geometry")
				} else {
					require.Equal(t, invalidations, gotInvalidations)
					require.Equal(t, renders, gotRenders)
					require.Equal(t, generation, m.VisualGeneration())
					require.Equal(t, previousView, view)
				}
				previousView = view
			}
			// Sixty accepted ticks do not mean sixty visible spinner frames.
			require.Equal(t, 6, changedTicks)

			// A delayed tick can wrap around the entire animation to the same
			// glyph; elapsed time alone is not a visible change either.
			clock.step = time.Duration(animation.Chat.Len()) * animation.Chat.DefaultFrameDuration()
			invalidations, renders := m.CacheStats()
			generation := m.VisualGeneration()
			m.Update(acceptSidebarTick(t, m))
			require.Equal(t, previousView, m.View())
			gotInvalidations, gotRenders := m.CacheStats()
			require.Equal(t, invalidations, gotInvalidations)
			require.Equal(t, renders, gotRenders)
			require.Equal(t, generation, m.VisualGeneration())
		})
	}
}

func TestUnchangedSpinnerDoesNotSuppressOtherPresentation(t *testing.T) {
	for _, channel := range []string{"hover", "placement", "transfer", "theme"} {
		t.Run(channel, func(t *testing.T) {
			m, _ := newAnimationInvalidationSidebar(t)
			m.SetTitleRegenerating(true)
			m.ReconcileLayout()
			m.CancelPresentation()
			m.ReconcileLayout()
			switch channel {
			case "hover":
				m.workingDirectory = "workspace"
				m.invalidateCache()
				m.ReconcileLayout()
				m.CancelPresentation()
				m.setHoverTarget("directory")
			case "placement":
				m.SetQueuedMessages([]QueuedMessage{{ID: "new", Text: "new queued row"}})
				m.ReconcileLayout()
				require.True(t, m.placement.running)
			case "transfer":
				m.treeCollapsed = false
				m.SetAgentSwitching(true, "root", "worker")
				m.ReconcileLayout()
				m.CancelPresentation()
				m.transferAnimationElapsed = transferStepDuration - 10*time.Millisecond
			}
			m.ReconcileLayout()
			beforeView := m.View()
			generation := m.VisualGeneration()
			frame := m.spinner.RawFrame()
			if channel == "theme" {
				invalidations, _ := m.CacheStats()
				m.Update(messages.ThemeChangedMsg{})
				got, _ := m.CacheStats()
				require.Greater(t, got, invalidations, "theme refresh must remain semantic invalidation")
			} else {
				tick := acceptSidebarTick(t, m)
				m.Update(tick)
				require.True(t, tick.Dirty())
				require.NotEqual(t, beforeView, m.View(), "independent presentation channel must repaint")
			}
			require.Equal(t, frame, m.spinner.RawFrame(), "fixture must not advance the spinner glyph")
			require.Greater(t, m.VisualGeneration(), generation)
		})
	}
}
