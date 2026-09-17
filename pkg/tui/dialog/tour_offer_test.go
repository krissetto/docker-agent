package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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

func TestTourOfferDialogBodyUsesReservedScrollbarWidth(t *testing.T) {
	t.Parallel()
	for _, notice := range []bool{false, true} {
		d := NewTourOfferDialog(notice).(*tourOfferDialog)
		d.SetSize(120, 40)
		view := d.View()
		bodyX, bodyY, bodyWidth, bodyHeight := d.BodyScrollBounds()
		row, col := d.Position()
		lines := strings.Split(ansi.Strip(view), "\n")
		var bodyLines []string
		for _, line := range lines[bodyY-row : bodyY-row+bodyHeight] {
			bodyLines = append(bodyLines, ansi.Cut(line, bodyX-col, bodyX-col+max(1, bodyWidth-2)))
		}
		body := strings.Join(bodyLines, "\n")
		for line := range strings.SplitSeq(body, "\n") {
			assert.NotEqual(t, "s-", strings.Trim(line, " │"), "reserved scrollbar columns must not hard-wrap the already wrapped prose")
		}
		assert.Contains(t, strings.Join(strings.Fields(body), " "), "First time here? Learn docker agent by doing: a hands-on tour, right in this chat. Takes two minutes.")
		if notice {
			assert.Contains(t, strings.Join(strings.Fields(body), " "), "Anonymous usage data helps improve docker agent. Opt out with TELEMETRY_ENABLED=false.")
		}
		assert.LessOrEqual(t, lipgloss.Height(view), 40)
		assert.LessOrEqual(t, lipgloss.Width(view), 120)
	}
}

func TestTourOfferDialogUsesVerticalPolicyChoices(t *testing.T) {
	d := NewTourOfferDialog(false).(*tourOfferDialog)
	d.SetSize(100, 40)
	view := ansi.Strip(d.View())
	rows := make(map[string]int)
	for row, line := range strings.Split(view, "\n") {
		for _, label := range []string{"Never", "Not now", "Take the tour"} {
			if strings.Contains(line, label) {
				rows[label] = row
			}
		}
	}
	require.Len(t, rows, 3)
	assert.Less(t, rows["Never"], rows["Not now"])
	assert.Less(t, rows["Not now"], rows["Take the tour"])
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	assert.Nil(t, cmd)
	key, ok := d.SelectedActionKey()
	require.True(t, ok)
	assert.Equal(t, 'n', key.Code)
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	result, ok := findMsg[TourOfferResultMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, TourOfferLater, result.Choice)
}
