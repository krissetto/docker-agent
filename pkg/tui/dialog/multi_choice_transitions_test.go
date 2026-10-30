package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The result follows the close message: the dialog closes first, then reports.
func TestMultiChoiceResultSequenceClosesBeforeReporting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		inputs []tea.Msg
		result MultiChoiceResult
	}{
		{"skip", []tea.Msg{tea.KeyPressMsg{Code: tea.KeyEnter}}, MultiChoiceResult{OptionID: "skip", IsSkipped: true}},
		{"option", []tea.Msg{tea.KeyPressMsg{Code: '1', Text: "1"}, tea.KeyPressMsg{Code: tea.KeyEnter}}, MultiChoiceResult{OptionID: "one", Value: "first"}},
		{"paste", []tea.Msg{tea.PasteMsg{Content: "  custom  "}, tea.KeyPressMsg{Code: tea.KeyEnter}}, MultiChoiceResult{OptionID: "custom", Value: "custom", IsCustom: true}},
		{"cancel", []tea.Msg{tea.KeyPressMsg{Code: tea.KeyEscape}}, MultiChoiceResult{IsCancelled: true}},
		{"cancel custom", []tea.Msg{tea.PasteMsg{Content: "discard"}, tea.KeyPressMsg{Code: tea.KeyEscape}}, MultiChoiceResult{IsCancelled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := NewMultiChoiceDialog(MultiChoiceConfig{
				DialogID: "sequence", Title: "Choose",
				Options:     []MultiChoiceOption{{ID: "one", Label: "One", Value: "first"}},
				AllowCustom: true, AllowSecondary: true,
			})
			var cmd tea.Cmd
			for _, input := range tc.inputs {
				_, cmd = d.Update(input)
			}
			msgs := collectMsgs(cmd)
			require.Len(t, msgs, 2)
			assert.Equal(t, CloseDialogMsg{}, msgs[0])
			assert.Equal(t, MultiChoiceResultMsg{DialogID: "sequence", Result: tc.result}, msgs[1])
		})
	}
}

// Ctrl+C cancels the prompt on the branch (upstream quits the program), so the
// waiting runtime receives an explicit cancellation instead of silence.
func TestMultiChoiceCtrlCCancelsWithResult(t *testing.T) {
	t.Parallel()

	d := NewMultiChoiceDialog(MultiChoiceConfig{DialogID: "quit", AllowCustom: true})
	_, _ = d.Update(tea.PasteMsg{Content: "unfinished"})
	_, cmd := d.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	assert.True(t, hasMsg[CloseDialogMsg](msgs))
	result, ok := findMsg[MultiChoiceResultMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, "quit", result.DialogID)
	assert.True(t, result.Result.IsCancelled)
}
