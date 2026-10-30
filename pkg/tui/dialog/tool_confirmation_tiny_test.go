package dialog

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// Tiny terminals (down to zero columns) must keep every painted decision cell
// clickable to its own decision and keep the cells outside the dialog dead.
// A mis-hit here would silently authorize a different action.
func TestToolConfirmationDialog_TinyWidthsEveryDecisionCellClicks(t *testing.T) {
	t.Parallel()

	want := map[string]runtime.ResumeRequest{"N": runtime.ResumeReject(""), "Y": runtime.ResumeApprove(), "T": runtime.ResumeApproveTool("shell"), "B": runtime.ResumeApproveBalanced(), "A": runtime.ResumeApproveAutonomous()}
	for width := range 60 {
		d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{}).(*toolConfirmationDialog)
		d.SetSize(width, 30)
		for selected := range 6 {
			d.FocusDefaultAction()
			d.focusedAction = selected
			d.revealChoice()
			view := d.View()
			row, col := d.Position()
			dl := NewDialogLayout(view, row, col)
			for y := row; y < row+lipgloss.Height(view); y++ {
				for x := col; x < col+lipgloss.Width(view); x++ {
					action, hit := d.ActionKeyAt(x, y, dl)
					if !hit {
						continue
					}
					// Interior cells of a run share the boundary's decision; checking the edges keeps the sweep fast.
					if prev, prevHit := d.ActionKeyAt(x-1, y, dl); prevHit && prev.Code == action.Code {
						if next, nextHit := d.ActionKeyAt(x+1, y, dl); nextHit && next.Code == action.Code {
							continue
						}
					}
					msgs := collectMsgs(clickCell(d, y, x))
					if action.Text == "R" {
						require.Truef(t, hasMsg[OpenDialogMsg](msgs), "width %d: reject-with-reason cell must open the reason dialog", width)
						continue
					}
					response, ok := findMsg[tuimessages.InteractionResponseMsg](msgs)
					require.Truef(t, ok, "width %d cell %d,%d action %s must dispatch a decision", width, x, y, action.Text)
					assert.Equalf(t, want[action.Text], response.Response.Resume, "width %d cell %d,%d action %s", width, x, y, action.Text)
				}
			}
			assert.Nilf(t, clickCell(d, row+lipgloss.Height(view), col), "width %d: outside clicks never authorize", width)
		}
	}
}
