package ui

import (
	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// Screen aggregates the lean TUI presentation models and lays out a full frame.
type Screen struct {
	TransientNotice string
	Transcript      *Transcript
	Editor          *Editor
	Autocomplete    *Autocomplete
	Status          StatusModel
	Confirm         *ConfirmModel
}

func NewScreen(workingDir, branch, editorPlaceholder string, historyStore ...*history.History) *Screen {
	return &Screen{
		Transcript:   NewTranscript(),
		Editor:       NewEditor(editorPlaceholder, historyStore...),
		Autocomplete: NewAutocomplete(),
		Status:       StatusModel{WorkingDir: workingDir, Branch: branch},
	}
}

// Frame produces the full terminal frame and cursor position.
func (s *Screen) Frame(width, height, spinnerFrame int, busy bool, sessionState service.SessionStateReader, pendingUsers []PendingUserMessage) (lines []string, cursorLine, cursorCol int) {
	width, height = max(1, width), max(1, height)
	lines = s.Transcript.Lines(width, spinnerFrame, busy, sessionState, pendingUsers)

	// The normal-screen transcript may grow into scrollback; the editable
	// tail may not. Allocate footer/popup rows before windowing editor rows.
	status := s.Status
	status.Active = busy
	footer := RenderStatus(status, width)
	footer = footer[:min(len(footer), max(0, height-1))]
	if status.Dormant {
		explanation := WrapANSI(StMuted().Render("Any queued work or reports will wait until you resume this session."), width)
		if len(footer)+len(explanation) < height {
			footer = append(footer, explanation...)
		}
	}
	if s.TransientNotice != "" && len(footer) < height-1 {
		footer = append([]string{Truncate(StMuted().Render(s.TransientNotice), width)}, footer...)
	}
	if len(footer) < height-1 {
		footer = append([]string{""}, footer...)
	}
	inputBudget := max(1, height-len(footer))
	var input []string
	var row, col int
	if s.Confirm != nil {
		input = s.Confirm.Render(width)
		row = max(0, len(input)-1)
		if len(input) > 0 {
			col = min(DisplayWidth(input[row]), width-1)
		}
	} else {
		input, row, col = s.Editor.Layout(width)
	}
	start := max(0, row-inputBudget+1)
	end := min(len(input), start+inputBudget)
	input = input[start:end]
	row -= start
	popup := s.Autocomplete.RenderRows(width, max(0, inputBudget-len(input)))
	lines = append(lines, popup...)
	cursorLine, cursorCol = len(lines)+row, col
	lines = append(lines, input...)
	lines = append(lines, footer...)
	return lines, cursorLine, cursorCol
}

// ConfirmModel holds a pending tool-approval prompt.
type ConfirmModel struct {
	Rejecting    bool
	RejectReason string
	Tool         string
	View         ToolView
	SessionID    string
	RequestID    string
}

func (c *ConfirmModel) Render(width int) []string {
	if c.Rejecting {
		return WrapANSI("Reject reason (Enter submits, Esc returns): "+c.RejectReason, width)
	}
	lines := []string{Truncate(StWarning().Render("● Approve tool call"), width)}
	lines = append(lines, RenderTool(c.View, width)...)
	lines = append(lines, Truncate(StMuted().Render("[y] yes   [a] always this tool   [b] auto-approve safe   [s] whole session   [n] no   [r] reject with reason"), width))
	return lines
}
