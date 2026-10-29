package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/notification"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
)

type reviewCommentInput struct {
	owner     string
	anchor    view.Anchor
	editID    string
	general   bool
	chatDraft string
}

type reviewCommentEditor interface {
	BeginCommentInput() bool
	CommentInputActive() bool
	CommentInputValue() string
	SetCommentInputValue(value string)
	FinishCommentInput() (string, bool)
}

func isReviewCommentKey(msg tea.KeyPressMsg) bool {
	key := msg.Key()
	return key.Code == 'c' && key.Mod == 0 && key.Text != "C"
}

func (m *appModel) startReviewComment(msg view.CommentInputRequestMsg) tea.Cmd {
	ws := m.activeReview()
	if ws == nil || ws.anchor != msg.Owner || m.dialogMgr.Open() {
		return nil
	}
	input, ok := m.editor.(reviewCommentEditor)
	if !ok {
		return nil
	}
	chatDraft := m.editor.Value()
	if !input.BeginCommentInput() {
		return nil
	}
	m.reviewCommentInput = &reviewCommentInput{owner: msg.Owner, anchor: msg.Anchor, editID: msg.ID, chatDraft: chatDraft}
	if msg.ID != "" {
		input.SetCommentInputValue(msg.Body)
	}
	m.reviewHoldC = false
	m.focusedPanel = PanelEditor
	m.panelFocused = ""
	m.editor.SetContextBarFocused(false)
	m.chatPage.BlurMessages()
	ws.current().Blur()
	cmd := m.editor.Focus()
	m.viewCacheValid = false
	return cmd
}

func (m *appModel) startReviewGeneralInput(owner string) tea.Cmd {
	ws := m.gitReviews[owner]
	input, ok := m.editor.(reviewCommentEditor)
	if ws == nil || ws != m.activeReview() || !ws.open || m.reviewModel() == nil || !ok {
		return nil
	}
	if m.reviewCommentInput != nil {
		if m.reviewCommentInput.general && m.reviewCommentInput.owner == owner {
			return nil
		}
		return nil
	}
	chatDraft := m.editor.Value()
	if !input.BeginCommentInput() {
		return nil
	}
	input.SetCommentInputValue(ws.draft.Snapshot().General)
	m.reviewCommentInput = &reviewCommentInput{owner: owner, general: true, chatDraft: chatDraft}
	m.reviewHoldC = false
	m.focusedPanel = PanelEditor
	m.panelFocused = ""
	m.editor.SetContextBarFocused(false)
	m.chatPage.BlurMessages()
	if model := m.reviewModel(); model != nil {
		model.Blur()
	}
	return m.editor.Focus()
}

func (m *appModel) focusReviewComposer() tea.Cmd {
	if ws := m.activeReview(); ws != nil && m.reviewModel() != nil {
		if m.editor.HasContextBar() && !m.editor.IsContextBarFocused() {
			m.focusedPanel = PanelEditor
			m.editor.SetContextBarFocused(true)
			m.editor.Blur()
			return nil
		}
		return m.startReviewGeneralInput(ws.anchor)
	}
	return m.editor.Focus()
}

func (m *appModel) prepareReviewInputTransition() (tea.Cmd, bool) {
	if m.reviewCommentInput == nil {
		return nil, true
	}
	cmd := m.finishReviewComment(false)
	if input, ok := m.editor.(reviewCommentEditor); ok && input.CommentInputActive() {
		return cmd, false
	}
	return cmd, true
}

func (m *appModel) syncReviewGeneralInput() {
	state := m.reviewCommentInput
	input, ok := m.editor.(reviewCommentEditor)
	if state == nil || !state.general || !ok || !input.CommentInputActive() {
		return
	}
	ws := m.gitReviews[state.owner]
	if ws == nil {
		return
	}
	if err := ws.draft.SetGeneral(input.CommentInputValue()); err == nil {
		ws.pushDraft()
	}
	m.viewCacheValid = false
}

func (m *appModel) finishReviewComment(save bool) tea.Cmd {
	input, ok := m.editor.(reviewCommentEditor)
	state := m.reviewCommentInput
	if !ok || !input.CommentInputActive() {
		m.reviewCommentInput = nil
		m.reviewHoldC = false
		return nil
	}
	if state != nil && state.general && state.owner != "" {
		if ws := m.gitReviews[state.owner]; ws != nil {
			if err := ws.draft.SetGeneral(input.CommentInputValue()); err != nil {
				return notification.ErrorCmd("Review comment not saved: " + err.Error())
			}
			ws.pushDraft()
		}
	}
	body, _ := input.FinishCommentInput()
	if state != nil && m.editor.Value() != state.chatDraft {
		m.editor.SetValue(state.chatDraft)
	}
	if state != nil && state.general {
		m.reviewCommentInput = nil
		m.reviewHoldC = false
		m.focusedPanel = PanelContent
		m.editor.Blur()
		m.chatPage.BlurMessages()
		if model := m.reviewModel(); model != nil {
			model.Focus()
		}
		m.viewCacheValid = false
		return nil
	}
	m.reviewCommentInput = nil
	m.reviewHoldC = false
	m.focusedPanel = PanelContent
	m.panelFocused = ""
	m.editor.Blur()
	m.chatPage.BlurMessages()
	if model := m.reviewModel(); model != nil {
		model.Focus()
	}
	m.viewCacheValid = false
	if !save || state == nil || strings.TrimSpace(body) == "" {
		return nil
	}
	if state.editID != "" {
		return m.handleReviewComponentMessage(view.EditCommentMsg{Owner: state.owner, ID: state.editID, Body: body})
	}
	return m.handleReviewComponentMessage(view.AddCommentMsg{Owner: state.owner, Anchor: state.anchor, Body: body})
}

func (m *appModel) updateReviewCommentKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	input, ok := m.editor.(reviewCommentEditor)
	if !ok || !input.CommentInputActive() {
		return false, nil
	}
	switch msg.String() {
	case "esc":
		return true, m.finishReviewComment(false)
	case "enter":
		if m.reviewCommentInput != nil && m.reviewCommentInput.general {
			if cmd := m.captureReviewGeneral(m.reviewCommentInput.owner); cmd != nil {
				return true, cmd
			}
			return true, m.sendReviewFromComposer()
		}
		return true, m.finishReviewComment(true)
	case "f1":
		return true, core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewHelpDialog(m.helpDocument())})
	case "tab":
		if m.reviewCommentInput != nil && m.reviewCommentInput.general {
			if cmd := m.captureReviewGeneral(m.reviewCommentInput.owner); cmd != nil {
				return true, cmd
			}
			if m.editor.HasContextBar() {
				if input, ok := m.editor.(reviewCommentEditor); ok {
					_, _ = input.FinishCommentInput()
				}
				m.reviewCommentInput = nil
				m.editor.SetContextBarFocused(true)
				m.editor.Blur()
				return true, nil
			}
			if m.reviewDocked() {
				m.reviewCommentInput.general = false
				return true, tea.Batch(m.finishReviewComment(false), m.focusDockedChat())
			}
			return true, m.finishReviewComment(true)
		}
		return true, nil
	case "shift+tab":
		if m.reviewCommentInput != nil && m.reviewCommentInput.general {
			return true, m.finishReviewComment(true)
		}
		return true, nil
	case "ctrl+n", "ctrl+p", "ctrl+t", "ctrl+w", "ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5", "ctrl+6", "ctrl+7", "ctrl+8", "ctrl+9":
		return true, nil
	default:
		cmd := m.updateEditorCmd(msg)
		if m.reviewCommentInput != nil && m.reviewCommentInput.general {
			m.syncReviewGeneralInput()
		}
		return true, cmd
	}
}

func (m *appModel) handleKeyRelease(msg tea.KeyReleaseMsg) (tea.Model, tea.Cmd) {
	if input, ok := m.editor.(reviewCommentEditor); ok && input.CommentInputActive() {
		return m, nil
	}
	if !m.reviewHoldC || msg.Key().Code != 'c' || !m.keyReleaseSupported {
		return m, nil
	}
	m.reviewHoldC = false
	model := m.reviewModel()
	if model == nil || !model.Focused() || !model.CommentSelectionContext() {
		return m, nil
	}
	cmd := model.StartComment()
	m.viewCacheValid = false
	return m, cmd
}

func (m *appModel) reviewCommentStatus() string {
	model := m.reviewModel()
	if model != nil && model.CommentInputActive() {
		return model.CommentInputStatus()
	}
	if state := m.reviewCommentInput; state != nil && state.general {
		count := 0
		if ws := m.gitReviews[state.owner]; ws != nil {
			count = len(ws.draft.Snapshot().Comments)
		}
		return fmt.Sprintf("Review · %d inline comments · Enter send review · Esc back · %s", count, fallbackString(m.reviewTargetLabel(state.owner), "current session"))
	}
	if input, ok := m.editor.(reviewCommentEditor); ok && input.CommentInputActive() {
		return "Comment · Enter save · Esc cancel"
	}
	if model == nil || !model.Focused() || model.DocumentState() != view.StateReady {
		return ""
	}
	if model.MouseSelecting() {
		return "Drag diff lines · release · c comment"
	}
	if m.keyReleaseSupported {
		if model.Mode() == view.ModeFiles {
			return "Click line · drag snippet · Hold c + move · space expand · f file/diff · o edit"
		}
		return "Click line · drag snippet · Hold c + move · space expand · f file/diff"
	}
	if model.Mode() == view.ModeFiles {
		return "Click line · drag snippet · c comment · space expand · f file/diff · o edit"
	}
	return "Click line · drag snippet · c comment · space expand · f file/diff"
}

func (m *appModel) handleReviewCommentMouse(msg tea.MouseMsg) tea.Cmd {
	model := m.reviewModel()
	if model == nil {
		return nil
	}
	_, cmd := model.HandleMouse(msg)
	m.viewCacheValid = false
	return cmd
}

func (m *appModel) reviewCommentMouse(msg tea.MouseMsg) tea.MouseMsg {
	mouse := msg.Mouse()
	mouse.X -= m.editorFrame().GetMarginLeft() + m.editorFrame().GetPaddingLeft()
	mouse.Y -= m.editorTop() + m.editorFrame().GetPaddingTop()
	switch msg := msg.(type) {
	case tea.MouseClickMsg:
		msg.X, msg.Y = mouse.X, mouse.Y
		return msg
	case tea.MouseMotionMsg:
		msg.X, msg.Y = mouse.X, mouse.Y
		return msg
	case tea.MouseReleaseMsg:
		msg.X, msg.Y = mouse.X, mouse.Y
		return msg
	}
	return msg
}
