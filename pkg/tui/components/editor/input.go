package editor

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/components/editor/internal/widget"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/widgets/textarea"
)

// InputConfig configures the shared editing surface, without composer delivery,
// history, completion, attachment, or tab-bar policy.
type InputConfig struct {
	Placeholder string
	NewlineKeys []string
	Unlimited   bool
}

// Input owns the normal composer's editable text and retained presentation.
// It is a single-owner model; hosts must not copy it.
type Input struct {
	*widget.Textarea
	themeGeneration             uint64
	surfaceWidth, surfaceHeight int
}

func NewInput(config InputConfig) *Input {
	input := &Input{Textarea: widget.NewTextarea()}
	input.refreshTheme()
	input.SetPlaceholder(config.Placeholder)
	input.SetPrompt("")
	input.SetCharLimit(-1)
	input.SetWidth(50)
	input.SetHeight(3)
	input.SetShowLineNumbers(false)
	if config.Unlimited {
		input.SetMaxHeight(0)
		input.SetMaxWidth(0)
		input.SetMaxContentHeight(0)
	}
	if len(config.NewlineKeys) > 0 {
		input.SetNewlineKeys(config.NewlineKeys...)
	}
	input.Focus()
	return input
}

func (i *Input) Init() tea.Cmd { return textarea.Blink }

func (i *Input) refreshTheme() {
	i.SetStyles(styles.InputStyle)
	i.themeGeneration = styles.ThemeGeneration()
}

func (i *Input) View() string {
	if i.themeGeneration != styles.ThemeGeneration() {
		i.refreshTheme()
	}
	return i.Textarea.View()
}

func (i *Input) FreshView() string {
	if i.themeGeneration != styles.ThemeGeneration() {
		i.refreshTheme()
	}
	return i.Textarea.FreshView()
}

func (i *Input) Update(msg tea.Msg) (*Input, tea.Cmd) {
	if paste, ok := msg.(tea.PasteMsg); ok {
		paste.Content = normalizePasteNewlines(paste.Content)
		msg = paste
	}
	_, cmd := i.Textarea.Update(msg)
	return i, cmd
}

// PlaceCursor uses coordinates local to the editable surface, including its style.
func (i *Input) PlaceCursor(cell, row int) {
	style := i.Styles().Focused.Base
	cell -= style.GetBorderLeftSize() + style.GetPaddingLeft() + style.GetMarginLeft() + lipgloss.Width(i.Prompt())
	row -= style.GetBorderTopSize() + style.GetPaddingTop() + style.GetMarginTop()
	i.Textarea.PlaceCursor(max(0, cell), max(0, row))
}

func (i *Input) ScrollByWheel(delta int) {
	for range max(delta, -delta) {
		if delta < 0 {
			i.CursorUp()
		} else {
			i.CursorDown()
		}
	}
}

// SetSize allocates the complete normal input surface, retaining a text cell
// before decoration on tiny terminals.
func (i *Input) SetSize(width, height int) {
	i.surfaceWidth, i.surfaceHeight = max(1, width), max(1, height)
	frame := i.Frame()
	i.Normalize(max(1, width-frame.GetHorizontalFrameSize()), max(1, height-frame.GetVerticalFrameSize()))
}

func inputFrame(width, height int) lipgloss.Style {
	frame := styles.EditorStyle
	if width <= frame.GetHorizontalFrameSize() {
		frame = frame.Margin(0).PaddingLeft(0).PaddingRight(0)
	}
	if height <= frame.GetVerticalFrameSize() {
		frame = frame.PaddingTop(0).PaddingBottom(0)
	}
	return frame
}

func (i *Input) Frame() lipgloss.Style { return inputFrame(i.surfaceWidth, i.surfaceHeight) }

func renderInputFrame(frame lipgloss.Style, width int, view string) string {
	return styles.RenderComposite(frame.Width(width+frame.GetHorizontalPadding()+frame.GetHorizontalBorderSize()), view)
}

// SurfaceView renders the composer's input area without surrounding tabs or banners.
func (i *Input) SurfaceView() string { return renderInputFrame(i.Frame(), i.Width(), i.View()) }
func (i *Input) SurfaceHeight() int  { return i.Height() + i.Frame().GetVerticalFrameSize() }

func (i *Input) PlaceSurfaceCursor(cell, row int) {
	frame := i.Frame()
	i.PlaceCursor(cell-frame.GetMarginLeft()-frame.GetBorderLeftSize()-frame.GetPaddingLeft(), row-frame.GetMarginTop()-frame.GetBorderTopSize()-frame.GetPaddingTop())
}
