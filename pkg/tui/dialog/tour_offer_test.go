package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tourOfferChoice runs a key press through the dialog and returns the
// reported choice, requiring that the dialog also closes.
func tourOfferChoice(t *testing.T, key tea.KeyPressMsg) TourOfferChoice {
	t.Helper()

	d := NewTourOfferDialog(false)
	d.SetSize(100, 40)

	_, cmd := d.Update(key)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2)
	assert.Equal(t, CloseDialogMsg{}, msgs[0])

	result, ok := msgs[1].(TourOfferResultMsg)
	require.True(t, ok, "expected TourOfferResultMsg, got %T", msgs[1])
	return result.Choice
}

func TestTourOfferDialog_Choices(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  tea.KeyPressMsg
		want TourOfferChoice
	}{
		{"enter accepts", tea.KeyPressMsg{Code: tea.KeyEnter}, TourOfferAccepted},
		{"y accepts", tea.KeyPressMsg{Code: 'y', Text: "y"}, TourOfferAccepted},
		{"n declines for now", tea.KeyPressMsg{Code: 'n', Text: "n"}, TourOfferLater},
		{"esc declines for now", tea.KeyPressMsg{Code: tea.KeyEscape}, TourOfferLater},
		{"d declines forever", tea.KeyPressMsg{Code: 'd', Text: "d"}, TourOfferNever},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tourOfferChoice(t, tt.key))
		})
	}
}

func TestTourOfferDialog_IgnoresOtherKeys(t *testing.T) {
	t.Parallel()

	d := NewTourOfferDialog(false)
	d.SetSize(100, 40)

	_, cmd := d.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	assert.Nil(t, cmd)
}

func TestTourOfferDialog_View(t *testing.T) {
	t.Parallel()

	d := NewTourOfferDialog(false)
	d.SetSize(100, 40)

	view := d.View()
	assert.Contains(t, view, "Welcome to docker agent")
	assert.Contains(t, view, "Take the tour")
	assert.NotContains(t, view, "usage data")

	withNotice := NewTourOfferDialog(true)
	withNotice.SetSize(100, 40)
	assert.Contains(t, withNotice.View(), "usage data")
}

func TestIsCommandPalette(t *testing.T) {
	t.Parallel()

	assert.True(t, IsCommandPalette(NewCommandPaletteDialog(nil)))
	assert.False(t, IsCommandPalette(NewTourOfferDialog(false)))
}
