package widget

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// Textinput owns the history-query widget and its last immutable ANSI artifact.
type Textinput struct {
	model   textinput.Model
	valid   bool
	view    string
	renders uint64
}

func NewTextinput() *Textinput { return &Textinput{model: textinput.New()} }
func (w *Textinput) View() string {
	if !w.valid {
		w.view = w.FreshView()
		w.valid = true
	}
	return w.view
}
func (w *Textinput) FreshView() string        { w.renders++; return w.model.View() }
func (w *Textinput) Renders() uint64          { return w.renders }
func (w *Textinput) Value() string            { return w.model.Value() }
func (w *Textinput) Styles() textinput.Styles { return w.model.Styles() }
func (w *Textinput) PrevSuggestionBinding() key.Binding {
	return copyBinding(w.model.KeyMap.PrevSuggestion)
}
func (w *Textinput) NextSuggestionBinding() key.Binding {
	return copyBinding(w.model.KeyMap.NextSuggestion)
}
func (w *Textinput) Update(msg tea.Msg) (*Textinput, tea.Cmd) {
	w.valid = false
	var cmd tea.Cmd
	w.model, cmd = w.model.Update(msg)
	return w, cmd
}
func (w *Textinput) Focus() tea.Cmd    { w.valid = false; return w.model.Focus() }
func (w *Textinput) SetValue(v string) { w.valid = false; w.model.SetValue(v) }
func (w *Textinput) SetWidth(v int) {
	if w.model.Width() != v {
		w.valid = false
		w.model.SetWidth(v)
	}
}
func (w *Textinput) SetStyles(v textinput.Styles) { w.valid = false; w.model.SetStyles(v) }
func (w *Textinput) SetPrompt(v string)           { w.valid = false; w.model.Prompt = v }
func (w *Textinput) SetPlaceholder(v string)      { w.valid = false; w.model.Placeholder = v }
