//nolint:gocritic // Dialog command returns intentionally preserve Bubble Tea evaluation shape.
package dialog

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TourOfferChoice is the user's answer to the first-run tour offer.
type TourOfferChoice int

const (
	// TourOfferAccepted starts the tour now.
	TourOfferAccepted TourOfferChoice = iota
	// TourOfferLater declines for this run; the offer may be shown again.
	TourOfferLater
	// TourOfferNever declines permanently.
	TourOfferNever
)

// TourOfferResultMsg reports the user's choice on the tour offer dialog.
type TourOfferResultMsg struct {
	Choice TourOfferChoice
}

type tourOfferKeyMap struct {
	Yes   key.Binding
	Later key.Binding
	Never key.Binding
}

type tourOfferDialog struct {
	BaseDialog

	keyMap tourOfferKeyMap
	// showTelemetryNotice folds the first-run telemetry banner into the
	// offer so the two never stack.
	showTelemetryNotice bool
}

// NewTourOfferDialog creates the first-run dialog offering the
// getting-started tour.
func NewTourOfferDialog(showTelemetryNotice bool) Dialog {
	return &tourOfferDialog{
		keyMap: tourOfferKeyMap{
			Yes:   key.NewBinding(key.WithKeys("enter", "y", "Y")),
			Later: key.NewBinding(key.WithKeys("n", "N", "esc")),
			Never: key.NewBinding(key.WithKeys("d", "D")),
		},
		showTelemetryNotice: showTelemetryNotice,
	}
}

func (d *tourOfferDialog) Init() tea.Cmd {
	return nil
}

func (d *tourOfferDialog) CancelDialogCmd() tea.Cmd { return tourOfferClose(TourOfferLater) }

func (d *tourOfferDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.KeyPressMsg, tea.MouseClickMsg:
		defer d.prepareLayout()
	}
	if handled, cmd := d.UpdateBodyScroll(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			view := d.View()
			row, col := d.CenterDialog(view)
			dl := NewDialogLayout(view, row, col)
			if d.CloseButtonHit(msg, dl) {
				return d, d.CancelDialogCmd()
			}
			if action, ok := d.ActionKeyAt(msg.X, msg.Y, dl); ok {
				return d.Update(action)
			}
		}
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Yes):
			return d, tourOfferClose(TourOfferAccepted)
		case key.Matches(msg, d.keyMap.Never):
			return d, tourOfferClose(TourOfferNever)
		case key.Matches(msg, d.keyMap.Later):
			return d, d.CancelDialogCmd()
		}
	}

	return d, nil
}

// tourOfferClose closes the dialog and reports the user's choice.
func tourOfferClose(choice TourOfferChoice) tea.Cmd {
	return tea.Sequence(
		core.CmdHandler(CloseDialogMsg{}),
		core.CmdHandler(TourOfferResultMsg{Choice: choice}),
	)
}

func (d *tourOfferDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

func (d *tourOfferDialog) content() (style lipgloss.Style, width int, header, body, footer string) {
	dialogWidth := d.ComputeDialogWidth(50, 46, 60)
	contentWidth := d.ContentWidth(dialogWidth, 2)
	bodyWidth := d.BodyContentWidth(dialogWidth)

	header = RenderTitle("Welcome to docker agent 👋", contentWidth, styles.DialogTitleStyle)
	content := NewContent(bodyWidth).
		AddContent(styles.BaseStyle.Width(bodyWidth).Render(
			"First time here? Learn docker agent by doing: a hands-on tour, right in this chat. Takes two minutes."))

	if d.showTelemetryNotice {
		content = content.
			AddSpace().
			AddContent(styles.MutedStyle.Width(bodyWidth).Render(
				"Anonymous usage data helps improve docker agent. Opt out with TELEMETRY_ENABLED=false.",
			))
	}

	footer = d.RenderActions(contentWidth,
		Action{Label: "Never", Key: tea.KeyPressMsg{Code: 'd', Text: "d"}},
		Action{Label: "Not now", Key: tea.KeyPressMsg{Code: 'n', Text: "n"}},
		Action{Label: "Take the tour", Key: tea.KeyPressMsg{Code: tea.KeyEnter}},
	)
	return styles.DialogStyle.Padding(1, 2), dialogWidth, header, content.Build(), footer
}

func (d *tourOfferDialog) View() string {
	style, width, header, body, footer := d.content()
	return d.RenderScrollableBody(style, width, header, body, footer)
}

func (d *tourOfferDialog) prepareLayout() {
	style, width, header, body, footer := d.content()
	d.PrepareScrollableBody(style, width, header, body, footer)
}

func (d *tourOfferDialog) Bindings() []key.Binding {
	return []key.Binding{}
}

func (d *tourOfferDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.prepareLayout()
	return cmd
}
