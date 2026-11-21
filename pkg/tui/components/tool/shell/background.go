package shell

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func (m *shellModel) backgroundRunning() bool {
	bg := m.message.Background
	return bg != nil && bg.Authoritative && bg.State == tools.BackgroundProcessRunning
}

func (m *shellModel) shellBackgroundSpinner() spinner.Spinner {
	if m.backgroundSpinner == nil {
		m.backgroundSpinner = spinner.New(m.ar, spinner.ModeSpinnerOnly, styles.SpinnerDotsAccentStyle)
	}
	return m.backgroundSpinner
}

func (m *shellModel) syncBackgroundSpinner() tea.Cmd {
	if m.backgroundRunning() {
		if !m.backgroundSpinnerActive {
			m.backgroundSpinnerActive = true
			return m.shellBackgroundSpinner().Init()
		}
	} else if m.backgroundSpinnerActive {
		m.backgroundSpinner.Stop()
		m.backgroundSpinnerActive = false
	}
	return nil
}

func (m *shellModel) backgroundHeader(now time.Time) (icon, status string) {
	bg := m.message.Background
	icon = styles.ToolCompletedIcon.Render("✓")
	switch bg.State {
	case tools.BackgroundProcessRunning:
		if !bg.Authoritative {
			status = "ended"
			break
		}
		icon = styles.NoStyle.MarginLeft(2).Render(m.shellBackgroundSpinner().View())
		if !bg.StartedAt.IsZero() {
			status = compactElapsed(max(now.Sub(bg.StartedAt), 0))
		}
	case tools.BackgroundProcessCompleted:
		if !bg.StartedAt.IsZero() && !bg.FinishedAt.IsZero() {
			status = compactElapsed(max(bg.FinishedAt.Sub(bg.StartedAt), 0))
		}
	case tools.BackgroundProcessFailed:
		icon = styles.ToolErrorIcon.Render("✗")
		switch {
		case bg.TimedOut:
			status = "timeout"
		case bg.ExitCode >= 0:
			status = fmt.Sprintf("exit %d", bg.ExitCode)
		default:
			status = "failed"
		}
		return icon, styles.ToolErrorMessageStyle.Render(status)
	case tools.BackgroundProcessStopped:
		status = "stopped"
	case "":
		status = "in background"
	default:
		status = "ended"
	}
	return icon, styles.ToolMessageStyle.Render(status)
}

func (m *shellModel) bodyVisibleRows(rows int) int {
	if m.message.Background != nil {
		return min(rows, maxBodyRows-bodyPadRows-hintRows)
	}
	return shellBodyVisibleRows(rows)
}

func (m *shellModel) bodyMaxOffset(rows int) int {
	return max(0, rows-m.bodyVisibleRows(rows))
}

func (m *shellModel) bodyOutputRows(rows int) (first, last int) {
	first = 1 + bodyLeadRows
	return first, first + m.bodyVisibleRows(rows) - 1
}

func (m *shellModel) backgroundFooter(remaining int) string {
	bg := m.message.Background
	label := "end of output"
	switch {
	case bg.State == "" || bg.OutputUnavailable:
		label = "live output unavailable"
	case m.backgroundRunning():
		label = "live · running in background"
	}
	if remaining > 0 {
		label = fmt.Sprintf("… %d more lines · scroll · %s", remaining, label)
	}
	parts := []string{label, sanitizeTerminalText(bg.ProcessID)}
	if !bg.StartedAt.IsZero() {
		parts = append(parts, "started "+bg.StartedAt.Format(time.RFC3339))
	}
	return strings.Join(parts, " · ")
}
