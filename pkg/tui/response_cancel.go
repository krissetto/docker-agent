package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const responsePromptLifetime = 3 * time.Second

type responseCancelPrompt struct {
	armed             bool
	application       *app.App
	sessionID, turnID string
	handle            runtime.SessionHandle
	generation        uint64
	expiresAt         time.Time
	noticeToken       messagebar.Token
	cancel            func() runtime.CancelOutcome
}

func (m *appModel) responseTurnID() string {
	if m.application != nil {
		if projection := m.application.Presentation(); projection != nil {
			return projection.Status.TurnID
		}
	}
	return ""
}

func (m *appModel) clearResponsePrompt() tea.Cmd {
	if m.responsePrompt.application == nil {
		return nil
	}
	token := m.responsePrompt.noticeToken
	m.responsePrompt = responseCancelPrompt{}
	m.interactionHintVisible = false
	if m.messageBar == nil {
		return nil
	}
	m.viewCacheValid = false
	return m.messageBar.ClearOwned(token)
}

func (m *appModel) tickResponsePrompt() tea.Cmd {
	if m.responsePrompt.application == nil {
		return nil
	}
	if m.responsePrompt.sessionID != "" && (m.application != m.responsePrompt.application || m.application.Session() == nil || m.application.Session().ID != m.responsePrompt.sessionID || m.application.SessionHandle() != m.responsePrompt.handle || m.responseRunGeneration != m.responsePrompt.generation || m.responseTurnID() != m.responsePrompt.turnID) {
		return m.clearResponsePrompt()
	}
	if m.responsePrompt.armed && !time.Now().Before(m.responsePrompt.expiresAt) {
		m.responsePrompt.armed = false
	}
	return nil
}

func (m *appModel) handleResponseEscape() tea.Cmd {
	if m.application == nil || m.application.Session() == nil {
		return nil
	}
	valid := m.responsePrompt.armed && time.Now().Before(m.responsePrompt.expiresAt) && m.responsePrompt.application == m.application && m.responsePrompt.sessionID == m.application.Session().ID && m.responsePrompt.handle == m.application.SessionHandle() && m.responsePrompt.generation == m.responseRunGeneration && m.responsePrompt.turnID == m.responseTurnID()
	if valid {
		cancel := m.responsePrompt.cancel
		clearCmd := m.clearResponsePrompt()
		cmd, accepted := chat.CancelResponseIntent(m.chatPage, cancel)
		if !accepted {
			return tea.Batch(clearCmd, cmd)
		}
		m.ensureMessageBar()
		m.responsePrompt = responseCancelPrompt{application: m.application}
		return tea.Batch(clearCmd, cmd, m.setResponseNotice("Response cancelled.", messagebar.Success, messagebar.Cancellation))
	}
	clearCmd := m.clearResponsePrompt()
	m.ensureMessageBar()
	m.responsePrompt = responseCancelPrompt{armed: true, application: m.application, sessionID: m.application.Session().ID, handle: m.application.SessionHandle(), generation: m.responseRunGeneration, turnID: m.responseTurnID(), expiresAt: time.Now().Add(responsePromptLifetime), cancel: m.application.CaptureCancelRun()}
	noticeCmd := m.setResponseNotice("Double Esc within 3s cancels the response.", messagebar.Warning, messagebar.Cancellation)
	token := m.responsePrompt.noticeToken
	return tea.Batch(clearCmd, noticeCmd, tea.Tick(responsePromptLifetime, func(time.Time) tea.Msg { return responseArmExpiredMsg{token: token} }))
}

type interactionHintReadyMsg struct {
	generation      uint64
	owner           string
	routeGeneration uint64
	sessionID, text string
}

func (m *appModel) cancelInteractionHint() tea.Cmd {
	m.interactionHintGeneration++
	if !m.interactionHintVisible {
		return nil
	}
	m.interactionHintVisible = false
	return m.clearResponsePrompt()
}

func (m *appModel) showInteractionHint(msg messages.ShowInteractionHintMsg) tea.Cmd {
	if m.responsePrompt.armed || m.application == nil || m.application.Session() == nil || m.application.Session().ID != msg.SessionID {
		return nil
	}
	clearCmd := m.cancelInteractionHint()
	if msg.Text == "" {
		return clearCmd
	}
	owner := m.paneFocus()
	routeGeneration, _ := m.supervisor.RouteGeneration(owner)
	ready := interactionHintReadyMsg{generation: m.interactionHintGeneration, owner: owner, routeGeneration: routeGeneration, sessionID: msg.SessionID, text: msg.Text}
	// One root token, no row timers or animation leases before expiry. Both
	// the double-click window and minimum 500ms have elapsed on delivery.
	delay := max(styles.DoubleClickThreshold, 500*time.Millisecond)
	return tea.Batch(clearCmd, tea.Tick(delay, func(time.Time) tea.Msg { return ready }))
}

func (m *appModel) finishInteractionHint(msg interactionHintReadyMsg) tea.Cmd {
	generation, exists := m.supervisor.RouteGeneration(msg.owner)
	if msg.generation != m.interactionHintGeneration || !exists || generation != msg.routeGeneration ||
		m.paneFocus() != msg.owner || m.application == nil || m.application.Session() == nil ||
		m.application.Session().ID != msg.sessionID || m.dialogMgr.Open() || m.responsePrompt.armed {
		return nil
	}
	m.ensureMessageBar()
	m.interactionHintVisible = true
	m.responsePrompt = responseCancelPrompt{application: m.application}
	noticeCmd := m.setResponseNotice(msg.text, messagebar.Info, messagebar.Hint)
	if m.responsePrompt.noticeToken.Generation == 0 {
		m.responsePrompt = responseCancelPrompt{}
		m.interactionHintVisible = false
		return nil
	}
	return noticeCmd
}

func (m *appModel) setResponseNotice(text string, severity messagebar.Severity, category messagebar.Category) tea.Cmd {
	token, cmd, accepted := m.setTransientNotice(messagebar.Message{Text: text, Severity: severity, Category: category, Owner: m.paneFocus()})
	if accepted {
		m.responsePrompt.noticeToken = token
	}
	return cmd
}

const transientNoticeLifetime = 5 * time.Second

type responseArmExpiredMsg struct{ token messagebar.Token }

type transientNoticeExpiredMsg struct {
	token    messagebar.Token
	deadline time.Time
}

func (m *appModel) setTransientNotice(notice messagebar.Message) (messagebar.Token, tea.Cmd, bool) {
	m.ensureMessageBar()
	deadline := time.Now().Add(transientNoticeLifetime)
	token, cmd, accepted := m.messageBar.SetNotice(notice, deadline)
	if !accepted {
		return token, cmd, false
	}
	m.viewCacheValid = false
	return token, tea.Batch(cmd, tea.Tick(transientNoticeLifetime, func(time.Time) tea.Msg {
		return transientNoticeExpiredMsg{token: token, deadline: deadline}
	})), true
}

func (m *appModel) expireTransientNotice(msg transientNoticeExpiredMsg) tea.Cmd {
	if m.messageBar == nil || time.Now().Before(msg.deadline) {
		return nil
	}
	if m.responsePrompt.noticeToken == msg.token {
		m.responsePrompt = responseCancelPrompt{}
		m.interactionHintVisible = false
	}
	m.viewCacheValid = false
	return m.messageBar.Expire(msg.token, time.Now())
}
