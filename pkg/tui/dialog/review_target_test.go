package dialog

import (
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reviewTargetDialogForTest() *reviewTargetDialog {
	d := NewReviewTargetDialog(ReviewTargetConfig{
		DialogID: "t", Title: "Send review to…", Default: "b", More: true,
		Targets: []ReviewTarget{
			{ID: "a", Label: "◆ alpha", Detail: "root"},
			{ID: "b", Label: "◆ beta", Detail: "this workspace"},
			{ID: "c", Label: "◇ gamma", Detail: "reviewer", Disabled: "open it to send"},
		},
	}).(*reviewTargetDialog)
	d.SetSize(120, 40)
	return d
}

func press(d *reviewTargetDialog, keys ...string) []tea.Msg {
	var out []tea.Msg
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		default:
			r, _ := utf8.DecodeRuneInString(k)
			msg = tea.KeyPressMsg{Code: r, Text: k}
		}
		_, cmd := d.Update(msg)
		out = append(out, collectMsgs(cmd)...)
	}
	return out
}

func resultOf(msgs []tea.Msg) (ReviewTargetResultMsg, bool) {
	for _, m := range msgs {
		if r, ok := m.(ReviewTargetResultMsg); ok {
			return r, true
		}
	}
	return ReviewTargetResultMsg{}, false
}

func TestReviewTargetDefaultsToWorkspaceAndSearches(t *testing.T) {
	d := reviewTargetDialogForTest()
	r, ok := resultOf(press(d, "enter"))
	require.True(t, ok)
	assert.Equal(t, "b", r.Target.ID, "the workspace's own session is preselected")

	d = reviewTargetDialogForTest()
	press(d, "a", "l", "p")
	assert.Contains(t, ansi.Strip(d.View()), "alpha")
	assert.NotContains(t, ansi.Strip(d.View()), "beta", "search filters by label and detail")
	r, ok = resultOf(press(d, "enter"))
	require.True(t, ok)
	assert.Equal(t, "a", r.Target.ID)
}

func TestReviewTargetDisabledCancelAndDuplicate(t *testing.T) {
	d := reviewTargetDialogForTest()
	press(d, "g", "a", "m")
	_, chosen := resultOf(press(d, "enter"))
	assert.False(t, chosen, "ineligible recipients can't be chosen")
	assert.Contains(t, ansi.Strip(d.View()), "open it to send", "and say why")

	d = reviewTargetDialogForTest()
	r, ok := resultOf(collectMsgs(d.CancelDialogCmd()))
	require.True(t, ok)
	assert.True(t, r.Cancelled)
	_, again := resultOf(press(d, "enter"))
	assert.False(t, again, "one response per dialog")
}

func TestReviewTargetLoadsMoreInPages(t *testing.T) {
	d := reviewTargetDialogForTest()
	msgs := press(d, "down", "down", "down", "enter")
	var more ReviewTargetMoreMsg
	for _, m := range msgs {
		if x, ok := m.(ReviewTargetMoreMsg); ok {
			more = x
		}
	}
	assert.Equal(t, 1, more.Page)
	_, dup := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Empty(t, collectMsgs(dup), "a page request in flight is not repeated")
	d.Update(ReviewTargetPageMsg{DialogID: "t", Targets: []ReviewTarget{{ID: "d", Label: "◆ delta"}}, Partial: "Showing the first 200 sessions"})
	view := ansi.Strip(d.View())
	assert.Contains(t, view, "delta")
	assert.Contains(t, view, "Showing the first 200 sessions")
	assert.NotContains(t, view, "Load more")
}
