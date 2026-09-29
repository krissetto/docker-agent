package notification

import (
	"image"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

type avoidanceClock struct{ now time.Time }

func (c *avoidanceClock) Now() time.Time { return c.now }
func (c *avoidanceClock) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		c.now = c.now.Add(d)
		return f(c.now)
	}
}

func avoidanceManager(t *testing.T) (*Manager, *animation.Runtime) {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&avoidanceClock{now: time.Unix(1, 0)})
	n := New()
	n.SetRuntime(ar)
	n.SetSize(100, 30)
	n.Update(ShowMsg{Text: "Notification with room for hover"})
	return &n, ar
}

func advanceAvoidance(t *testing.T, n *Manager, ar *animation.Runtime) animation.TickMsg {
	t.Helper()
	cmd := ar.Continue()
	require.NotNil(t, cmd)
	msg, accepted := ar.Accept(cmd().(animation.TickMsg))
	require.True(t, accepted)
	n.Update(msg)
	return msg
}

func TestAvoidanceRequiresOccupiedOverlap(t *testing.T) {
	for _, name := range []string{"empty", "inactive", "left", "below", "offscreen", "overlap"} {
		t.Run(name, func(t *testing.T) {
			n, ar := avoidanceManager(t)
			b := n.itemBounds()[0]
			cells := []image.Rectangle{image.Rect(b.col, b.row, b.col+1, b.row+1)}
			active := true
			switch name {
			case "empty":
				cells = nil
			case "inactive":
				active = false
			case "left":
				cells = []image.Rectangle{image.Rect(0, b.row, b.col, b.row+1)}
			case "below":
				cells = []image.Rectangle{image.Rect(b.col, b.row+b.height, b.col+1, b.row+b.height+1)}
			case "offscreen":
				cells = []image.Rectangle{image.Rect(101, b.row, 102, b.row+1)}
			}
			n.SetAvoidance(cells, active)
			if name == "overlap" {
				assert.Equal(t, b.height, n.target)
				assert.True(t, ar.HasActive())
			} else {
				assert.Zero(t, n.target)
				assert.False(t, ar.HasActive())
			}
		})
	}
}

func TestAvoidanceConvergesWithoutRetargetJitterAndReturns(t *testing.T) {
	n, ar := avoidanceManager(t)
	b := n.itemBounds()[0]
	cells := []image.Rectangle{image.Rect(b.col, b.row-4, b.col+1, b.row+1)}
	n.SetAvoidance(cells, true)
	target := n.target
	require.Positive(t, target)
	previous := 0
	for range 30 {
		if !ar.HasActive() {
			break
		}
		advanceAvoidance(t, n, ar)
		n.SetAvoidance(cells, true)
		assert.Equal(t, target, n.target)
		assert.GreaterOrEqual(t, n.lift, previous)
		previous = n.lift
	}
	assert.Equal(t, target, n.lift)
	assert.False(t, ar.HasActive())
	assert.Nil(t, ar.Continue())
	assert.LessOrEqual(t, n.itemBounds()[0].row+b.height, cells[0].Min.Y)

	n.SetAvoidance(cells, false)
	require.True(t, ar.HasActive())
	for range 30 {
		if !ar.HasActive() {
			break
		}
		advanceAvoidance(t, n, ar)
		assert.LessOrEqual(t, n.lift, previous)
		previous = n.lift
	}
	assert.Zero(t, n.lift)
	assert.Equal(t, b.row, n.itemBounds()[0].row)
	assert.False(t, ar.HasActive())
}

func TestAvoidanceAnimatedHitboxesAndHoverFollowLayer(t *testing.T) {
	n, ar := avoidanceManager(t)
	b := n.itemBounds()[0]
	n.SetAvoidance([]image.Rectangle{image.Rect(b.col, b.row-8, b.col+1, b.row+1)}, true)
	n.HandleMouseMotion(b.col, b.row+b.height-1)
	require.Equal(t, b.id, n.hoveredID)
	for range 30 {
		if !ar.HasActive() {
			break
		}
		before := n.lift
		msg := advanceAvoidance(t, n, ar)
		assert.Equal(t, n.lift != before, msg.Dirty())
		moved := n.itemBounds()[0]
		row, col := n.position()
		assert.Equal(t, moved.row, row)
		assert.Equal(t, moved.col, col)
		x, y := closeButtonPosition(moved)
		id, ok := n.CloseButtonHit(x, y)
		require.True(t, ok)
		assert.Equal(t, b.id, id)
		id, text, ok := n.BodyHit(moved.col, moved.row)
		require.True(t, ok)
		assert.Equal(t, b.id, id)
		assert.Equal(t, b.text, text)
	}
	assert.Zero(t, n.hoveredID, "stationary pointer must leave moving notification")
	moved := n.itemBounds()[0]
	n.HandleMouseMotion(moved.col, moved.row)
	id, _, ok := n.CopyHit(moved.col, moved.row)
	assert.True(t, ok)
	assert.Equal(t, b.id, id)
	_, _, ok = n.BodyHit(b.col, b.row+b.height-1)
	assert.False(t, ok)
}

func TestAvoidanceResizeAndRemovalReleaseAnimation(t *testing.T) {
	n, ar := avoidanceManager(t)
	b := n.itemBounds()[0]
	n.SetAvoidance([]image.Rectangle{image.Rect(0, 1, 100, 29)}, true)
	advanceAvoidance(t, n, ar)
	n.SetSize(8, 3)
	assert.GreaterOrEqual(t, n.itemBounds()[0].row, 0)
	for range 30 {
		if !ar.HasActive() {
			break
		}
		advanceAvoidance(t, n, ar)
	}
	assert.False(t, ar.HasActive())
	n.SetSize(100, 30)
	n.SetAvoidance([]image.Rectangle{image.Rect(b.col, b.row, b.col+1, b.row+1)}, true)
	require.True(t, ar.HasActive())
	n.Update(HideMsg{})
	assert.False(t, ar.HasActive())
	assert.Zero(t, n.lift)
	n.Update(ShowMsg{Text: "again"})
	n.Cleanup()
	assert.False(t, ar.HasActive())
}

func TestAvoidanceStackRechecksRowsCrossedDuringAscent(t *testing.T) {
	n, ar := avoidanceManager(t)
	n.Update(ShowMsg{Text: "short"})
	bounds := n.itemBounds()
	require.Len(t, bounds, 2)
	bottom := bounds[1]
	cells := []image.Rectangle{
		image.Rect(bottom.col, bottom.row+bottom.height-1, bottom.col+1, bottom.row+bottom.height),
		image.Rect(bottom.col, bounds[0].row-2, bottom.col+1, bounds[0].row-1),
	}
	n.SetAvoidance(cells, true)
	for range 30 {
		if !ar.HasActive() {
			break
		}
		advanceAvoidance(t, n, ar)
	}
	for _, b := range n.itemBounds() {
		for _, cell := range cells {
			assert.False(t, image.Rect(b.col, b.row, b.col+b.width, b.row+b.height).Overlaps(cell))
		}
	}
	assert.False(t, ar.HasActive())
}

func TestNarrowStackGeometryMatchesRightAlignedLayer(t *testing.T) {
	n, _ := avoidanceManager(t)
	n.Update(ShowMsg{Text: "x"})
	n.SetSize(8, 4)
	bounds := n.itemBounds()
	require.Len(t, bounds, 2)
	_, col := n.position()
	width := max(bounds[0].width, bounds[1].width)
	for _, b := range bounds {
		assert.Equal(t, col+width-b.width, b.col)
	}
	_, _, ok := n.BodyHit(bounds[1].col, bounds[1].row+bounds[1].height-1)
	assert.False(t, ok, "vertically clipped content is not clickable")
	_, ok = n.CloseButtonHit(9, 1)
	assert.False(t, ok, "horizontally clipped content is not clickable")
}
