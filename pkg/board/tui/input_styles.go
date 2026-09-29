package tui

import (
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"

	"github.com/docker/docker-agent/pkg/tui/styles"
	localtextarea "github.com/docker/docker-agent/pkg/tui/widgets/textarea"
	localtextinput "github.com/docker/docker-agent/pkg/tui/widgets/textinput"
)

// The board retains its widgets independently of the main TUI. Keep the concrete
// widget-style conversion here so shared themes depend only on local adapters.
func boardTextareaStyles() textarea.Styles {
	s := styles.InputStyle
	state := func(s localtextarea.StyleState) textarea.StyleState {
		return textarea.StyleState{Base: s.Base, Text: s.Text, LineNumber: s.LineNumber,
			CursorLineNumber: s.CursorLineNumber, CursorLine: s.CursorLine, EndOfBuffer: s.EndOfBuffer,
			Placeholder: s.Placeholder, Prompt: s.Prompt, Selection: s.Selection}
	}
	return textarea.Styles{Focused: state(s.Focused), Blurred: state(s.Blurred),
		Cursor: textarea.CursorStyle{Color: s.Cursor.Color, Shape: s.Cursor.Shape, Blink: s.Cursor.Blink, BlinkSpeed: s.Cursor.BlinkSpeed}}
}

func boardTextinputStyles() textinput.Styles {
	s := styles.DialogInputStyle
	state := func(s localtextinput.StyleState) textinput.StyleState {
		return textinput.StyleState{Text: s.Text, Placeholder: s.Placeholder, Suggestion: s.Suggestion, Prompt: s.Prompt}
	}
	return textinput.Styles{Focused: state(s.Focused), Blurred: state(s.Blurred),
		Cursor: textinput.CursorStyle{Color: s.Cursor.Color, Shape: s.Cursor.Shape, Blink: s.Cursor.Blink, BlinkSpeed: s.Cursor.BlinkSpeed}}
}
