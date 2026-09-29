// Package widget owns mutable Charm widgets. No widget model or mutable cursor
// escapes these adapters: readers receive immutable, owner-materialized ANSI.
package widget

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

// Textarea is a single-owner widget, not a copyable snapshot.
type Textarea struct {
	model                           textarea.Model
	generation                      uint64
	valid                           bool
	view                            string
	renders                         uint64
	normalized                      bool
	normalizedWidth, normalizedRows int
}

func NewTextarea() *Textarea           { return &Textarea{model: textarea.New()} }
func (w *Textarea) changed()           { w.generation++; w.valid = false; w.normalized = false }
func (w *Textarea) Generation() uint64 { return w.generation }
func (w *Textarea) Renders() uint64    { return w.renders }
func (w *Textarea) View() string {
	if !w.valid {
		w.view = w.FreshView()
		w.valid = true
	}
	return w.view
}
func (w *Textarea) FreshView() string { w.renders++; return w.model.View() }
func (w *Textarea) Update(msg tea.Msg) (*Textarea, tea.Cmd) {
	w.changed()
	var cmd tea.Cmd
	w.model, cmd = w.model.Update(msg)
	return w, cmd
}
func (w *Textarea) Focus() tea.Cmd { w.changed(); return w.model.Focus() }

// Normalize repairs Bubbles' viewport against real wrapped content, preserving
// the logical-newline policy. View mutates its shared viewport, so perform it
// here before publishing any artifact, never on a retained presentation hit.
func (w *Textarea) Normalize(width, rows int) {
	if w.normalized && w.normalizedWidth == width && w.normalizedRows == rows {
		return
	}
	w.changed()
	dynamic, minHeight, maxHeight := w.model.DynamicHeight, w.model.MinHeight, w.model.MaxHeight
	w.model.DynamicHeight = true
	w.model.MinHeight, w.model.MaxHeight = rows, rows
	w.model.SetWidth(width)
	_ = w.FreshView()
	w.model.SetHeight(rows)
	w.model.DynamicHeight, w.model.MinHeight, w.model.MaxHeight = dynamic, minHeight, maxHeight
	w.normalized, w.normalizedWidth, w.normalizedRows = true, width, rows
}
func (w *Textarea) SetNewlineKeys(keys ...string) {
	w.changed()
	w.model.KeyMap.InsertNewline.SetKeys(keys...)
}
func (w *Textarea) SetNewlineEnabled(enabled bool) {
	w.changed()
	w.model.KeyMap.InsertNewline.SetEnabled(enabled)
}

// Return a detached binding: key.Binding contains a mutable slice.
func copyBinding(b key.Binding) key.Binding {
	b.SetKeys(append([]string(nil), b.Keys()...)...)
	return b
}
func (w *Textarea) NewlineBinding() key.Binding { return copyBinding(w.model.KeyMap.InsertNewline) }
func (w *Textarea) PasteBinding() key.Binding   { return copyBinding(w.model.KeyMap.Paste) }
func (w *Textarea) BackspaceBinding() key.Binding {
	return copyBinding(w.model.KeyMap.DeleteCharacterBackward)
}
func (w *Textarea) Value() string               { return w.model.Value() }
func (w *Textarea) Word() string                { return w.model.Word() }
func (w *Textarea) Width() int                  { return w.model.Width() }
func (w *Textarea) Height() int                 { return w.model.Height() }
func (w *Textarea) Line() int                   { return w.model.Line() }
func (w *Textarea) Column() int                 { return w.model.Column() }
func (w *Textarea) ScrollYOffset() int          { return w.model.ScrollYOffset() }
func (w *Textarea) LineCount() int              { return w.model.LineCount() }
func (w *Textarea) LineInfo() textarea.LineInfo { return w.model.LineInfo() }
func (w *Textarea) SelectedText() string        { return w.model.SelectedText() }
func (w *Textarea) Focused() bool               { return w.model.Focused() }
func (w *Textarea) Styles() textarea.Styles     { return w.model.Styles() }
func (w *Textarea) Placeholder() string         { return w.model.Placeholder }
func (w *Textarea) SetPlaceholder(v string) {
	if w.model.Placeholder != v {
		w.changed()
		w.model.Placeholder = v
	}
}
func (w *Textarea) Prompt() string { return w.model.Prompt }
func (w *Textarea) SetPrompt(v string) {
	if w.model.Prompt != v {
		w.changed()
		w.model.Prompt = v
	}
}
func (w *Textarea) ShowLineNumbers() bool { return w.model.ShowLineNumbers }
func (w *Textarea) SetShowLineNumbers(v bool) {
	if w.model.ShowLineNumbers != v {
		w.changed()
		w.model.ShowLineNumbers = v
	}
}
func (w *Textarea) DynamicHeight() bool { return w.model.DynamicHeight }
func (w *Textarea) SetDynamicHeight(v bool) {
	if w.model.DynamicHeight != v {
		w.changed()
		w.model.DynamicHeight = v
	}
}
func (w *Textarea) MinHeight() int { return w.model.MinHeight }
func (w *Textarea) SetMinHeight(v int) {
	if w.model.MinHeight != v {
		w.changed()
		w.model.MinHeight = v
	}
}
func (w *Textarea) MaxHeight() int { return w.model.MaxHeight }
func (w *Textarea) SetMaxHeight(v int) {
	if w.model.MaxHeight != v {
		w.changed()
		w.model.MaxHeight = v
	}
}
func (w *Textarea) MaxContentHeight() int { return w.model.MaxContentHeight }
func (w *Textarea) SetMaxContentHeight(v int) {
	if w.model.MaxContentHeight != v {
		w.changed()
		w.model.MaxContentHeight = v
	}
}
func (w *Textarea) CharLimit() int { return w.model.CharLimit }
func (w *Textarea) SetCharLimit(v int) {
	if w.model.CharLimit != v {
		w.changed()
		w.model.CharLimit = v
	}
}
func (w *Textarea) Blur()                       { w.changed(); w.model.Blur() }
func (w *Textarea) Reset()                      { w.changed(); w.model.Reset() }
func (w *Textarea) MoveToBegin()                { w.changed(); w.model.MoveToBegin() }
func (w *Textarea) MoveToEnd()                  { w.changed(); w.model.MoveToEnd() }
func (w *Textarea) CursorUp()                   { w.changed(); w.model.CursorUp() }
func (w *Textarea) CursorDown()                 { w.changed(); w.model.CursorDown() }
func (w *Textarea) CursorEnd()                  { w.changed(); w.model.CursorEnd() }
func (w *Textarea) SetValue(v string)           { w.changed(); w.model.SetValue(v) }
func (w *Textarea) InsertString(v string)       { w.changed(); w.model.InsertString(v) }
func (w *Textarea) SetWidth(v int)              { w.changed(); w.model.SetWidth(v) }
func (w *Textarea) SetHeight(v int)             { w.changed(); w.model.SetHeight(v) }
func (w *Textarea) SetCursorColumn(v int)       { w.changed(); w.model.SetCursorColumn(v) }
func (w *Textarea) SetStyles(v textarea.Styles) { w.changed(); w.model.SetStyles(v) }
func (w *Textarea) SetVirtualCursor(v bool)     { w.changed(); w.model.SetVirtualCursor(v) }

// Cursor returns Bubbles' detached public cursor description, not its private
// virtual cursor. Callers cannot mutate the widget through this value.
func (w *Textarea) Cursor() *tea.Cursor { return w.model.Cursor() }
