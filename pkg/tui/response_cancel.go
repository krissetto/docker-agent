package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
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
	deadline          animation.Transition
	expiresAt         time.Time
	noticeToken       messagebar.Token
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
	m.responsePrompt.deadline.Cancel()
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
	if m.responsePrompt.armed && (m.application != m.responsePrompt.application || m.application.Session() == nil || m.application.Session().ID != m.responsePrompt.sessionID || m.application.SessionHandle() != m.responsePrompt.handle || m.responseRunGeneration != m.responsePrompt.generation || m.responseTurnID() != m.responsePrompt.turnID) {
		return m.clearResponsePrompt()
	}
	m.responsePrompt.deadline.Tick()
	if !m.responsePrompt.deadline.Running() {
		return m.clearResponsePrompt()
	}
	return nil
}

func (m *appModel) handleResponseEscape() tea.Cmd {
	if m.application == nil || m.application.Session() == nil {
		return nil
	}
	valid := m.responsePrompt.armed && time.Now().Before(m.responsePrompt.expiresAt) && m.responsePrompt.deadline.Running() && m.responsePrompt.application == m.application && m.responsePrompt.sessionID == m.application.Session().ID && m.responsePrompt.handle == m.application.SessionHandle() && m.responsePrompt.generation == m.responseRunGeneration && m.responsePrompt.turnID == m.responseTurnID()
	if valid {
		clearCmd := m.clearResponsePrompt()
		cmd, accepted := chat.CancelResponse(m.chatPage)
		if !accepted {
			return tea.Batch(clearCmd, cmd)
		}
		m.ensureMessageBar()
		m.responsePrompt = responseCancelPrompt{application: m.application, deadline: m.ar.Transition()}
		return tea.Batch(clearCmd, cmd, m.responsePrompt.deadline.Start(time.Second, animation.Linear), m.setResponseNotice("Response cancelled.", messagebar.Success, messagebar.Cancellation))
	}
	clearCmd := m.clearResponsePrompt()
	m.ensureMessageBar()
	m.responsePrompt = responseCancelPrompt{armed: true, application: m.application, sessionID: m.application.Session().ID, handle: m.application.SessionHandle(), generation: m.responseRunGeneration, turnID: m.responseTurnID(), deadline: m.ar.Transition(), expiresAt: time.Now().Add(responsePromptLifetime)}
	return tea.Batch(clearCmd, m.responsePrompt.deadline.Start(responsePromptLifetime, animation.Linear), m.setResponseNotice("Press Esc again to cancel the response.", messagebar.Warning, messagebar.Cancellation))
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
	m.responsePrompt = responseCancelPrompt{application: m.application, deadline: m.ar.Transition()}
	noticeCmd := m.setResponseNotice(msg.text, messagebar.Info, messagebar.Hint)
	if m.responsePrompt.noticeToken.Generation == 0 {
		m.responsePrompt = responseCancelPrompt{}
		m.interactionHintVisible = false
		return nil
	}
	return tea.Batch(m.responsePrompt.deadline.Start(responsePromptLifetime, animation.Linear), noticeCmd)
}

func (m *appModel) setResponseNotice(text string, severity messagebar.Severity, category messagebar.Category) tea.Cmd {
	token, cmd, accepted := m.messageBar.SetNotice(messagebar.Message{Text: text, Severity: severity, Category: category, Owner: m.paneFocus()}, time.Time{})
	if accepted {
		m.responsePrompt.noticeToken = token
	}
	return cmd
}
