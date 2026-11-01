package dialog

import (
	"fmt"
	"math"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func newInactivePaneDimDialog(t *testing.T, dim float64) *settingsDialog {
	t.Helper()
	d, ok := NewSettingsDialog(messages.Preferences{SendMode: messages.SendModeSteer, SplitDiffView: true, RenderImages: true, ShowBanner: true, TabTitleMaxLength: 20, SoundThreshold: 10, DimInactivePanes: true, InactivePaneDim: dim}, true).(*settingsDialog)
	require.True(t, ok)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	d.tab = tabAppearance
	d.selected[tabAppearance] = rowInactivePaneDim
	return d
}

func TestInactivePaneDimKeyboardStepsInFivePercentSteps(t *testing.T) {
	d := newInactivePaneDimDialog(t, 0.5)
	msgs := collectMsgs(func() tea.Cmd { _, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight}); return cmd }())
	assert.True(t, hasMsg[messages.PreviewSettingsMsg](msgs))
	assert.InDelta(t, 0.55, d.current.InactivePaneDim, 1e-12)
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.InDelta(t, 0.45, d.current.InactivePaneDim, 1e-12)
}

func TestInactivePaneDimClampsAtBounds(t *testing.T) {
	d := newInactivePaneDimDialog(t, 0.98)
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.InDelta(t, 1, d.current.InactivePaneDim, 1e-12)
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.InDelta(t, 1, d.current.InactivePaneDim, 1e-12)

	d = newInactivePaneDimDialog(t, 0.02)
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.InDelta(t, 0, d.current.InactivePaneDim, 1e-12)
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.InDelta(t, 0, d.current.InactivePaneDim, 1e-12)
}

func TestInactivePaneDimHandEditedValuesSnapToWholePercent(t *testing.T) {
	d := newInactivePaneDimDialog(t, 0.37)
	assert.Contains(t, ansi.Strip(d.View()), "37%")
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.InDelta(t, 0.42, d.current.InactivePaneDim, 1e-12)
}

func TestInactivePaneDimNonFiniteAndOutOfRangeNormalizeOnOpen(t *testing.T) {
	for input, want := range map[float64]float64{math.NaN(): 0.5, math.Inf(1): 0.5, -3: 0, 9: 1} {
		d, ok := NewSettingsDialog(messages.Preferences{DimInactivePanes: true, InactivePaneDim: input}, true).(*settingsDialog)
		require.True(t, ok)
		assert.InDelta(t, want, d.current.InactivePaneDim, 1e-12, "input %v", input)
	}
}

func TestInactivePaneDimStaysAdjustableWhileDimmingOff(t *testing.T) {
	d := newInactivePaneDimDialog(t, 0.5)
	d.current.DimInactivePanes = false
	d.prepareBody()
	_, visible := d.rowLines[rowInactivePaneDim]
	assert.True(t, visible)
	assert.True(t, d.selectable(tabAppearance, rowInactivePaneDim))
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.InDelta(t, 0.55, d.current.InactivePaneDim, 1e-12)
}

func TestInactivePaneDimMouseArrowsAdjustAndPersistDraft(t *testing.T) {
	for _, delta := range []int{-1, 1} {
		d := newInactivePaneDimDialog(t, 0.5)
		d.SetSize(100, 50)
		d.prepareBody()
		found := false
		for _, hit := range d.rowHits {
			if hit.row == rowInactivePaneDim && hit.kind == settingsAdjust && hit.delta == delta {
				msgs := collectMsgs(clickSettingsHit(t, d, hit))
				assert.True(t, hasMsg[messages.PreviewSettingsMsg](msgs))
				found = true
				break
			}
		}
		require.True(t, found, "arrow hit for delta %d", delta)
		assert.InDelta(t, 0.5+0.05*float64(delta), d.current.InactivePaneDim, 1e-12)
		applied, ok := findMsg[messages.ApplySettingsMsg](collectMsgs(d.apply()))
		require.True(t, ok)
		assert.InDelta(t, 0.5+0.05*float64(delta), applied.Preferences.InactivePaneDim, 1e-12)
	}
}

func TestInactivePaneDimMouseRowClickFocusesWithoutChangingValue(t *testing.T) {
	d := newInactivePaneDimDialog(t, 0.5)
	d.SetSize(100, 50)
	d.prepareBody()
	for _, hit := range d.rowHits {
		if hit.row == rowInactivePaneDim && hit.kind == settingsFocusRow {
			assert.Nil(t, clickSettingsHit(t, d, hit))
			assert.InDelta(t, 0.5, d.current.InactivePaneDim, 1e-12)
			return
		}
	}
	t.Fatal("expected a focus hit for the inactive pane dim row")
}

func TestInactivePaneDimMouseArrowsStopAtBounds(t *testing.T) {
	cases := []struct {
		start float64
		delta int
		want  float64
	}{
		{0, -1, 0},
		{0, 1, 0.05},
		{0.02, -1, 0},
		{0.98, 1, 1},
		{1, 1, 1},
		{1, -1, 0.95},
	}
	for _, tc := range cases {
		d := newInactivePaneDimDialog(t, tc.start)
		d.SetSize(100, 50)
		d.prepareBody()
		found := false
		for _, hit := range d.rowHits {
			if hit.row == rowInactivePaneDim && hit.kind == settingsAdjust && hit.delta == tc.delta {
				clickSettingsHit(t, d, hit)
				found = true
				break
			}
		}
		require.True(t, found, "arrow hit for start %v delta %d", tc.start, tc.delta)
		assert.InDelta(t, tc.want, d.current.InactivePaneDim, 1e-12, "start %v delta %d", tc.start, tc.delta)
		assert.Contains(t, ansi.Strip(d.View()), fmt.Sprintf("%d%%", int(math.Round(tc.want*100))))
	}
}

// Zero is an explicit fade amount, distinct from the default. Fixtures built
// with zero-value Preferences therefore show 0%; the app always fills the field
// from userconfig, which resolves an absent key to DefaultInactivePaneDim.
func TestInactivePaneDimZeroValuePreferencesIsExplicitZero(t *testing.T) {
	d, ok := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
	require.True(t, ok)
	assert.InDelta(t, 0, d.current.InactivePaneDim, 0)
}
