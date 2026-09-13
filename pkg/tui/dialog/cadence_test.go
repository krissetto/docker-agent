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
			require.NotNil(t, cmd)
			require.Len(t, mgr.stack, 1)
			e := &mgr.stack[0]
			wantWidth, wantHeight := e.targetWidth, e.targetHeight
			require.Greater(t, wantHeight, 1)
			assert.Equal(t, wantWidth, e.renderWidth, "width is final on frame zero")
			assert.Equal(t, 1, e.renderHeight, "frame zero never flashes full content")

			previous := 0
			sequence := []int{e.renderHeight}
			for elapsed := animation.TickRate; e.anim.Running(); elapsed += animation.TickRate {
				mgr.handleTick(advanceDialog(r, r.EnsureRunning(), elapsed))
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

func TestSettingsSemanticTabChangeHasOneStableTarget(t *testing.T) {
	r := newDialogRuntime()
	mgr := &manager{runtime: r, width: 120, height: 40}
	d := NewSettingsDialog(messages.Preferences{}, true)
	mgr.handleOpen(OpenDialogMsg{Model: d})
	mgr.handleTick(advanceDialog(r, r.EnsureRunning(), dialogOpenDuration))

	entry := &mgr.stack[0]
	baseline := entry.boundsMeasurementCount
	mgr.forwardToTop(tea.KeyPressMsg{Code: tea.KeyTab}) // Appearance -> Behavior
	require.True(t, entry.anim.Running())
	assert.Equal(t, baseline+1, entry.boundsMeasurementCount)
	target := entry.targetHeight
	for elapsed := dialogOpenDuration + animation.TickRate; entry.anim.Running(); elapsed += animation.TickRate {
		mgr.handleTick(advanceDialog(r, r.EnsureRunning(), elapsed))
		assert.Equal(t, target, entry.targetHeight)
	}
	assert.Equal(t, baseline+1, entry.boundsMeasurementCount, "animation ticks do not repeatedly remeasure or retarget")
}

func TestSettingsBidirectionalRetargetCadence(t *testing.T) {
	for _, size := range [][2]int{{165, 47}, {120, 40}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			r := newDialogRuntime()
			mgr := &manager{runtime: r, width: size[0], height: size[1]}
			mgr.handleOpen(OpenDialogMsg{Model: NewSettingsDialog(messages.Preferences{}, true)})
			mgr.handleTick(advanceDialog(r, r.EnsureRunning(), dialogOpenDuration))
			entry := &mgr.stack[0]

			elapsed := dialogOpenDuration
			for round, keyCode := range []rune{'\t', 0, '\t', 0, '\t'} {
				key := tea.KeyPressMsg{Code: keyCode}
				if keyCode == 0 {
					key = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
				}
				beforeHeight, beforeCount := entry.renderHeight, entry.boundsMeasurementCount
				mgr.forwardToTop(key)
				assert.Equal(t, beforeCount+1, entry.boundsMeasurementCount, "one semantic action measures once")
				assert.Equal(t, beforeHeight, entry.fromHeight, "retarget starts at the currently rendered height")
				target := entry.targetHeight

				previous := beforeHeight
				direction := target - beforeHeight
				for entry.anim.Running() {
					elapsed += animation.TickRate
					mgr.handleTick(advanceDialog(r, r.EnsureRunning(), elapsed))
					assert.Equal(t, target, entry.targetHeight, "target remains stable through the transition")
					if direction >= 0 {
						assert.GreaterOrEqual(t, entry.renderHeight, previous)
					} else {
						assert.LessOrEqual(t, entry.renderHeight, previous)
					}
					row, _ := entry.position(size[0], size[1])
					assert.InDelta(t, float64(size[1]-entry.renderHeight)/2, row, 1, "center remains invariant")
					previous = entry.renderHeight
				}
				assert.Equal(t, target, entry.renderHeight)
				t.Logf("round=%d elapsed=%s target=%d measurements=%d", round, elapsed, target, entry.boundsMeasurementCount)
			}

			// Reverse before the current resize settles: the new transition must
			// continue from the sampled current position without a reset or jump.
			mgr.forwardToTop(tea.KeyPressMsg{Code: tea.KeyTab})
			elapsed += 2 * animation.TickRate
			mgr.handleTick(advanceDialog(r, r.EnsureRunning(), elapsed))
			current := entry.renderHeight
			mgr.forwardToTop(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
			assert.Equal(t, current, entry.fromHeight)
			assert.Equal(t, int32(1), r.ActiveCount(), "reversal reuses the active transition lease")
		})
	}
}

func TestIdenticalDialogUpdateIsMeasuredButNotRetargeted(t *testing.T) {
	r := newDialogRuntime()
	mgr := &manager{runtime: r, width: 100, height: 30}
	mgr.handleOpen(OpenDialogMsg{Model: NewSettingsDialog(messages.Preferences{}, true)})
	mgr.handleTick(advanceDialog(r, r.EnsureRunning(), dialogOpenDuration))
	entry := &mgr.stack[0]

	mgr.forwardToTop(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, 2, entry.boundsMeasurementCount)
	assert.False(t, entry.lastBoundsEvent.Retargeted)
	assert.False(t, entry.anim.Running(), "identical bounds are deduplicated")
	assert.Equal(t, lipgloss.Height(entry.dialog.View()), entry.targetHeight)
}
