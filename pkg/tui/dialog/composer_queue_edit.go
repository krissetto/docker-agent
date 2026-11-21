package dialog

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type composerQueueEdit struct {
	BaseDialog

	input textarea.Model
	save  func(string) tea.Cmd
}

func NewComposerQueueEdit(text string, save func(string) tea.Cmd) Dialog {
	input := textarea.New()
	input.SetStyles(styles.InputStyle)
	input.ShowLineNumbers = false
	input.Prompt = ""
	input.CharLimit = -1
	input.SetValue(text)
	return &composerQueueEdit{input: input, save: save}
}

func (d *composerQueueEdit) Init() tea.Cmd { return d.input.Focus() }

func (d *composerQueueEdit) SetSize(width, height int) tea.Cmd {
	d.BaseDialog.SetSize(width, height)
	d.input.SetWidth(max(1, min(width, 80)-styles.DialogStyle.GetHorizontalFrameSize()))
	d.input.SetHeight(max(1, min(height-8, 8)))
	return nil
}

func (d *composerQueueEdit) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case messages.ThemeChangedMsg:
		d.input.SetStyles(styles.InputStyle)
		return d, nil
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd
	case tea.KeyPressMsg:
		keys := core.GetKeys()
		if key.Matches(msg, key.NewBinding(key.WithKeys("esc"))) {
			return d, closeDialogCmd()
		}
		if key.Matches(msg, keys.EditorSend) {
			if text := d.input.Value(); strings.TrimSpace(text) != "" {
				return d, tea.Sequence(closeDialogCmd(), d.save(text))
			}
			return d, nil
		}
		d.input.KeyMap.InsertNewline.SetKeys(append([]string{"shift+enter"}, keys.EditorNewline.Keys()...)...)
	}
	var cmd tea.Cmd
	d.input, cmd = d.input.Update(msg)
	return d, cmd
}

func (d *composerQueueEdit) View() string {
	keys := core.GetKeys()
	content := lipgloss.JoinVertical(lipgloss.Left, styles.DialogTitleStyle.Render("Edit queued message"), styles.RenderComposite(styles.BaseStyle.Background(styles.EditorBg), d.input.View()), RenderHelpKeys(d.input.Width(), keys.EditorSend.Help().Key, "save", "Esc", "cancel"))
	return styles.DialogStyle.Render(content)
}

func (d *composerQueueEdit) Position() (row, col int) { return d.CenterDialog(d.View()) }
