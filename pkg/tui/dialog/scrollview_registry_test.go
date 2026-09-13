package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/help"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestBaseDialogScrollviewDirtyGeneration(t *testing.T) {
	var base BaseDialog
	view := base.newScrollview(scrollview.WithReserveScrollbarSpace(true))
	view.SetSize(20, 3)
	view.SetContent([]string{"a", "b", "c", "d", "e", "f"}, 6)
	assert.True(t, base.TakeVisualDirty())
	assert.False(t, base.TakeVisualDirty())
	view.ScrollBy(1)
	assert.True(t, base.TakeVisualDirty())
	assert.False(t, base.TakeVisualDirty())
	view.SetScrollOffset(view.MaxScrollOffset())
	base.TakeVisualDirty()
	view.ScrollBy(1)
	assert.False(t, base.TakeVisualDirty(), "boundary wheel preserves cached frame")
	view.SetPosition(3, 4)
	assert.False(t, base.TakeVisualDirty(), "hit geometry alone does not alter output")
}

type registryDialog struct {
	BaseDialog

	viewport *scrollview.Model
}

func (d *registryDialog) Init() tea.Cmd { return nil }
func (d *registryDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	_, cmd := d.viewport.Update(msg)
	return d, cmd
}
func (d *registryDialog) View() string         { return d.viewport.View() }
func (d *registryDialog) Position() (int, int) { return 0, 0 }

func TestSettledDialogScrollbarDragInvalidatesManager(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	mgr.SetSize(80, 24)
	d := &registryDialog{}
	d.viewport = d.newScrollview(scrollview.WithReserveScrollbarSpace(true))
	d.viewport.SetSize(20, 3)
	d.viewport.SetContent([]string{"a", "b", "c", "d", "e", "f"}, 6)
	mgr.handleOpen(OpenDialogMsg{Model: d})
	settleTestDialog(mgr)
	d.TakeVisualDirty()
	mgr.TakeVisualDirty()
	// Forward directly to the settled child: the fixture's entire short frame is title chrome.
	mgr.forwardToTop(tea.MouseClickMsg{Button: tea.MouseLeft, X: d.viewport.ScrollbarX(), Y: 0})
	require.True(t, d.viewport.IsDragging())
	assert.True(t, mgr.TakeVisualDirty())
	mgr.forwardToTop(tea.MouseMotionMsg{Button: tea.MouseLeft, X: d.viewport.ScrollbarX(), Y: 2})
	assert.True(t, mgr.TakeVisualDirty())
	mgr.forwardToTop(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: d.viewport.ScrollbarX(), Y: 2})
	assert.True(t, mgr.TakeVisualDirty())
	assert.False(t, d.viewport.IsDragging())
	assert.False(t, mgr.TakeVisualDirty())
	mgr.Cleanup()
}

func TestPreparedFamilyViewsPreserveScrollResourcesAndSelection(t *testing.T) {
	fixtures := []struct {
		name string
		new  func() Dialog
	}{
		{"settings", func() Dialog { return NewSettingsDialog(messages.Preferences{}, true) }},
		{"commands", func() Dialog { return NewCommandPaletteDialog(concretePaletteCommands(2, 10)) }},
		{"models", func() Dialog { return NewModelPickerDialog(nil) }},
		{"plans", func() Dialog { return NewPlanBrowserDialog(plans.ListResult{}) }},
		{"readonly", func() Dialog { return NewHelpDialog(help.Document{}) }},
		{"snapshots", func() Dialog { return NewSnapshotsDialog(make([]int, 30)) }},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			d := f.new()
			d.SetSize(40, 12)
			d.View()
			visual := d.(interface{ TakeVisualDirty() bool })
			visual.TakeVisualDirty()
			bounds := d.(interface {
				BodyScrollBounds() (int, int, int, int)
				BodyScrollOffset() int
			})
			x, y, w, h := bounds.BodyScrollBounds()
			offset := bounds.BodyScrollOffset()
			for range 20 {
				d.View()
				require.False(t, visual.TakeVisualDirty(), "View cannot change viewport generation")
				nextX, nextY, nextW, nextH := bounds.BodyScrollBounds()
				require.Equal(t, []int{x, y, w, h, offset}, []int{nextX, nextY, nextW, nextH, bounds.BodyScrollOffset()})
			}
		})
	}
}
