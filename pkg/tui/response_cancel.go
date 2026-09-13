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
	m.responsePrompt = responseCancelPrompt{}
	if m.messageBar == nil {
		return nil
	}
	m.viewCacheValid = false
	return m.messageBar.Update(messagebar.ClearMessageMsg{})
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
		return tea.Batch(clearCmd, cmd, m.responsePrompt.deadline.Start(time.Second, animation.Linear), m.messageBar.Update(messagebar.SetMessageMsg{Text: "Response cancelled.", Severity: messagebar.Success}))
	}
	clearCmd := m.clearResponsePrompt()
	m.ensureMessageBar()
	m.responsePrompt = responseCancelPrompt{armed: true, application: m.application, sessionID: m.application.Session().ID, handle: m.application.SessionHandle(), generation: m.responseRunGeneration, turnID: m.responseTurnID(), deadline: m.ar.Transition(), expiresAt: time.Now().Add(responsePromptLifetime)}
	return tea.Batch(clearCmd, m.responsePrompt.deadline.Start(responsePromptLifetime, animation.Linear), m.messageBar.Update(messagebar.SetMessageMsg{Text: "Press Esc again to cancel the response.", Severity: messagebar.Warning}))
}

func (m *appModel) showInteractionHint(msg messages.ShowInteractionHintMsg) tea.Cmd {
	if m.responsePrompt.armed || m.application == nil || m.application.Session() == nil || m.application.Session().ID != msg.SessionID {
		return nil
	}
	clearCmd := m.clearResponsePrompt()
	if msg.Text == "" {
		return clearCmd
	}
	m.ensureMessageBar()
	m.responsePrompt = responseCancelPrompt{application: m.application, deadline: m.ar.Transition()}
	return tea.Batch(clearCmd, m.responsePrompt.deadline.Start(responsePromptLifetime, animation.Linear), m.messageBar.Update(messagebar.SetMessageMsg{Text: msg.Text, Severity: messagebar.Info}))
}
