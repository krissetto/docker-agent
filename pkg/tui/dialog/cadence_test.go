package dialog

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestConcreteDialogOpeningCadence(t *testing.T) {
	models := make([]runtime.ModelChoice, 18)
	for i := range models {
		models[i] = runtime.ModelChoice{Name: fmt.Sprintf("model-%02d", i), Ref: fmt.Sprintf("provider/model-%02d", i)}
	}
	fixtures := []struct {
		name string
		new  func(*animation.Runtime) Dialog
	}{
		{"tool confirmation", func(r *animation.Runtime) Dialog {
			return NewToolConfirmationDialog(r, realShellLSConfirmationEvent(), &service.SessionState{})
		}},
		{"model", func(*animation.Runtime) Dialog { return NewModelPickerDialog(models) }},
		{"session cost", func(*animation.Runtime) Dialog { return NewCostDialog(session.New()) }},
		{"settings", func(*animation.Runtime) Dialog { return NewSettingsDialog(messages.Preferences{}, true) }},
		{"commands", func(*animation.Runtime) Dialog {
			return NewCommandPaletteDialog([]commands.Category{{Name: "General", Commands: concretePaletteCommands(1, 10)[0].Commands}})
		}},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			const width, height = 120, 40
			r := newDialogRuntime()
			mgr := &manager{runtime: r, width: width, height: height}
			_, cmd := mgr.handleOpen(OpenDialogMsg{Model: fixture.new(r)})
			if fixture.name == "commands" || fixture.name == "model" {
				require.NotNil(t, cmd, "text input retains its cursor initialization")
			} else {
				require.Nil(t, cmd, "opening only registers animation")
			}
			require.Len(t, mgr.stack, 1)
			e := &mgr.stack[0]
			wantWidth, wantHeight := e.targetWidth, e.targetHeight
			require.Greater(t, wantHeight, 1)
			assert.Equal(t, wantWidth, e.renderWidth, "width is final on frame zero")
			assert.Equal(t, 1, e.renderHeight, "frame zero never flashes full content")

			previous := 0
			sequence := []int{e.renderHeight}
			for elapsed := animation.TickRate; e.anim.Running(); elapsed += animation.TickRate {
				mgr.handleTick(advanceDialog(r, r.Continue(), elapsed))
				sequence = append(sequence, e.renderHeight)
				assert.GreaterOrEqual(t, e.renderHeight, previous, "opening progress is consecutive and monotonic")
				assertManagerFrameBounds(t, mgr, wantWidth, e.renderHeight)
				previous = e.renderHeight
			}
			assert.Equal(t, wantHeight, e.renderHeight)
			assert.Equal(t, 1, e.boundsMeasurementCount, "ticks and View do not cause a second target")
			t.Logf("opening sequence %v target=%dx%d", sequence, wantWidth, wantHeight)
		})
	}
}

func TestSettingsFocusAndCategoriesKeepStableBounds(t *testing.T) {
	for _, size := range [][2]int{{165, 47}, {120, 40}, {80, 24}, {30, 8}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			r := newDialogRuntime()
			mgr := &manager{runtime: r, width: size[0], height: size[1]}
			mgr.handleOpen(OpenDialogMsg{Model: NewSettingsDialog(messages.Preferences{}, true)})
			mgr.handleTick(advanceDialog(r, r.Continue(), dialogOpenDuration))
			entry := &mgr.stack[0]
			width, height := entry.targetWidth, entry.targetHeight
			for _, key := range []tea.KeyPressMsg{
				{Code: tea.KeyTab}, {Code: tea.KeyTab}, {Code: tea.KeyRight},
				{Code: tea.KeyRight}, {Code: tea.KeyLeft}, {Code: tea.KeyDown},
				{Code: tea.KeyTab}, {Code: tea.KeyTab, Mod: tea.ModShift},
			} {
				beforeCount := entry.boundsMeasurementCount
				mgr.forwardToTop(key)
				assert.Equal(t, beforeCount+1, entry.boundsMeasurementCount)
				assert.Equal(t, width, entry.targetWidth)
				assert.Equal(t, height, entry.targetHeight)
				assert.False(t, entry.anim.Running(), "focus/category changes never recenter the card")
				assert.Equal(t, int32(0), r.ActiveCount(), "static settings own no animation lease")
			}
		})
	}
}

func TestIdenticalDialogUpdateIsMeasuredButNotRetargeted(t *testing.T) {
	r := newDialogRuntime()
	mgr := &manager{runtime: r, width: 100, height: 30}
	mgr.handleOpen(OpenDialogMsg{Model: NewSettingsDialog(messages.Preferences{}, true)})
	mgr.handleTick(advanceDialog(r, r.Continue(), dialogOpenDuration))
	entry := &mgr.stack[0]

	mgr.forwardToTop(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, 2, entry.boundsMeasurementCount)
	assert.False(t, entry.lastBoundsEvent.Retargeted)
	assert.False(t, entry.anim.Running(), "identical bounds are deduplicated")
	assert.Equal(t, lipgloss.Height(entry.dialog.View()), entry.targetHeight)
}

type measuredSettingsDialog struct {
	*settingsDialog

	views int
}

func (d *measuredSettingsDialog) View() string { d.views++; return d.settingsDialog.View() }
func (d *measuredSettingsDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	_, cmd := d.settingsDialog.Update(msg)
	return d, cmd
}

func TestSettingsUnchangedMotionDoesNotMeasureRenderOrLease(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	mgr.SetSize(100, 40)
	d := &measuredSettingsDialog{settingsDialog: NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)}
	mgr.handleOpen(OpenDialogMsg{Model: d})
	settleTestDialog(mgr)
	mgr.View()
	mgr.takeTopVisualDirty()
	mgr.TakeVisualDirty()
	measurements, views := mgr.stack[0].boundsMeasurementCount, d.views
	active := r.ActiveCount()
	for range 200 {
		_, cmd := mgr.Update(tea.MouseMotionMsg{X: 0, Y: 0})
		require.Nil(t, cmd)
		require.False(t, mgr.TakeVisualDirty())
	}
	assert.Equal(t, measurements, mgr.stack[0].boundsMeasurementCount)
	assert.Equal(t, views, d.views)
	assert.Equal(t, active, r.ActiveCount())
	mgr.Cleanup()
	assert.Zero(t, r.ActiveCount())
}

func TestSharedTicksKeepPreparedTargetLayoutWarm(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	mgr.SetSize(40, 12)
	d := &measuredSettingsDialog{settingsDialog: NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)}
	mgr.handleOpen(OpenDialogMsg{Model: d})
	mgr.View()
	mgr.takeTopVisualDirty()
	mgr.TakeVisualDirty()
	entry := &mgr.stack[0]
	views, measurements := d.views, entry.boundsMeasurementCount
	targetHeight := entry.targetHeight
	generation := d.bodyScroll.VisualGeneration()
	preparations := d.bodyPreparationCount
	gutter := d.bodyScroll.ReservedCols()
	x, y, w, h := d.BodyScrollBounds()
	tickCmd := r.Continue()
	for entry.anim.Running() {
		tick := acceptedDialogTick(r, tickCmd)
		mgr.handleTick(tick)
		tickCmd = r.Continue()
		mgr.View()
		require.Equal(t, views, d.views, "outer transition clips prepared content without full dialog render")
		require.Equal(t, measurements, entry.boundsMeasurementCount)
		require.Equal(t, targetHeight, entry.targetHeight)
		require.Equal(t, generation, d.bodyScroll.VisualGeneration(), "shared ticks never resize/repopulate body")
		require.Equal(t, preparations, d.bodyPreparationCount, "shared ticks never prepare/reflow unchanged target content")
		require.Equal(t, gutter, d.bodyScroll.ReservedCols())
		nextX, nextY, nextW, nextH := d.BodyScrollBounds()
		require.Equal(t, []int{x, y, w, h}, []int{nextX, nextY, nextW, nextH})
	}
	require.Nil(t, tickCmd)
	require.Zero(t, r.ActiveCount())
	mgr.Cleanup()
}

type tickDirtyDialog struct {
	lifecycleDialog

	frame int
}

func (d *tickDirtyDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if tick, ok := msg.(animation.TickMsg); ok {
		d.frame++
		d.view = fmt.Sprintf("frame %d", d.frame)
		tick.MarkDirty()
	}
	return d, nil
}

func TestSharedTickChildDirtyMarkerRefreshesCachedContent(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	mgr.SetSize(40, 12)
	d := &tickDirtyDialog{lifecycleDialog: lifecycleDialog{view: "frame 0"}}
	mgr.handleOpen(OpenDialogMsg{Model: d})
	tickCmd := r.Continue()
	tick := acceptedDialogTick(r, tickCmd)
	mgr.handleTick(tick)
	require.True(t, tick.Dirty())
	require.Equal(t, "frame 1", mgr.stack[0].intrinsicView(), "child dirty output must update even while outer transition also needs paint")
	mgr.Cleanup()
	require.Zero(t, r.ActiveCount())
}
