package chat

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type referenceHoverScheduler struct{ now time.Time }

func (s *referenceHoverScheduler) Now() time.Time { return s.now }
func (s *referenceHoverScheduler) Tick(_ time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { s.now = s.now.Add(50 * time.Millisecond); return create(s.now) }
}
func TestMessageReferenceHoverChatPresentationRouting(t *testing.T) {
	sess := session.New()
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "abcde-canonical", SessionID: "child-session", Agent: "worker", Name: "Worker 界"}}}}
	sess.SetSubagentTree(&tree)
	a, _ := newSessionTestApp(t, sess, nil, nil)
	ar := animation.NewRuntimeWithScheduler(&referenceHoverScheduler{now: time.Unix(1, 0)})
	p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p); ar.Stop() })
	p.SetSize(160, 40)
	p.resetProjection(runtime.SessionSnapshot{Session: sess})
	input := session.UserMessage("unchanged payload")
	input.InputOrigin, input.SenderID = session.InputOriginRuntime, "child-session"
	p.messages.AddInputMessage(input, 0)
	p.handleRuntimeEvent(&runtime.SubagentTreeEvent{Snapshot: tree})
	tick := func() {
		t.Helper()
		cmd := ar.Continue()
		require.NotNil(t, cmd)
		accepted, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		p.Update(accepted)
	}
	hover := func() {
		t.Helper()
		frame := p.messages.View()
		x, y := styles.AppPadding+p.computeSidebarLayout().chatStartX, 0
		if p.splitPresentation != nil {
			x, y = p.splitPresentation.Transcript.X, p.splitPresentation.Transcript.Y
		}
		for row, line := range strings.Split(frame, "\n") {
			before, _, found := strings.Cut(ansi.Strip(line), "(abcde)")
			if found {
				p.handleMouseMotion(tea.MouseMotionMsg{X: x + ansi.StringWidth(before), Y: y + row})
				return
			}
		}
		t.Fatalf("reference missing: %q", ansi.Strip(frame))
	}
	require.False(t, ar.HasActive())
	hover()
	require.True(t, ar.HasActive())
	tick()
	ClearSidebarHover(p)
	tick()
	require.False(t, ar.HasActive(), "modal/input occlusion fades the message reference")
	hover()
	tick()
	p.routeMouseEvent(tea.MouseMotionMsg{X: 159, Y: 0}, 0)
	tick()
	require.False(t, ar.HasActive(), "sidebar entry clears transcript hover")
	hover()
	tick()
	CancelSidebarPresentation(p)
	require.False(t, ar.HasActive(), "hidden page releases presentation immediately")
	SetSidebarPresentationActive(p, false)
	hover()
	require.False(t, ar.HasActive(), "disabled presentation ignores entry")
	SetSidebarPresentationActive(p, true)
	hover()
	require.True(t, ar.HasActive())
	p.isDraggingSidebar = true
	p.handleMouseMotion(tea.MouseMotionMsg{X: 100, Y: 0})
	require.False(t, ar.HasActive(), "sidebar drag cancels transcript presentation")
	p.isDraggingSidebar = false
	g := SplitPresentationGeometry{Transcript: PresentationRect{X: 10, Y: 8, Width: 60, Height: 15}, Shell: p.MeasureSplitShell(160, 40), ShowSidebar: false}
	p.SetSplitPresentation(&g)
	hover()
	require.True(t, ar.HasActive(), "a visible transcript works without its sidebar")
	tick()
	p.routeMouseEvent(tea.MouseMotionMsg{X: 1, Y: 1}, 1)
	tick()
	require.False(t, ar.HasActive(), "leaving a split transcript clears its reference")
	hover()
	require.True(t, ar.HasActive())
	p.SetPresentationVisible(false)
	require.False(t, ar.HasActive())
	p.SetPresentationVisible(true)
	hover()
	require.True(t, ar.HasActive(), "visible pane re-enables message presentation")
}
