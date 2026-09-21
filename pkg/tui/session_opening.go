package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type sessionOpening struct {
	origin                     *app.App
	focus                      string
	generation                 uint64
	target, node, title, agent string
	cancel                     context.CancelFunc
	spinner                    spinner.Spinner
	transition                 animation.Transition
	started                    time.Duration
	phase                      openingPhase
	installed                  bool
}
type openingPhase uint8

const (
	openingWaiting openingPhase = iota
	openingLoaderOut
	openingChatIn
)

type subagentOpenedMsg struct {
	request     *sessionOpening
	application *app.App
	prepared    supervisor.PreparedHostedView
	err         error
}

func (m *appModel) beginSubagentOpening(nodeID subagent.NodeID, target, title, agent string) tea.Cmd {
	if m.opening != nil && m.opening.node == string(nodeID) {
		return nil
	}
	m.cancelSessionOpening()
	if m.hostedLoad != nil {
		m.hostedLoad.cancel()
		m.hostedLoad = nil
	}
	lifetime := m.ctx()
	ctx, cancel := context.WithCancel(lifetime)
	shutdown := m.shutdownDone
	go func() {
		select {
		case <-shutdown:
			cancel()
		case <-ctx.Done():
		}
	}()
	generation, _ := m.supervisor.RouteGeneration(m.paneFocus())
	r := &sessionOpening{origin: m.application, focus: m.paneFocus(), generation: generation, target: target, node: string(nodeID), title: title, agent: agent, cancel: cancel, transition: animation.NewTransition(m.ar), started: m.ar.Now()}
	r.spinner = spinner.NewWithStyleProvider(m.ar, spinner.ModeBoth, func() lipgloss.Style { return styles.SpinnerDotsHighlightStyle })
	r.spinner.SetMessage("Loading...")
	r.spinner.Init()
	m.opening = r
	m.editor.Blur()
	owner := m.supervisor
	origin := m.application
	sessions, services := origin.SessionRuntime(), origin.Runtime()
	_, preparedSupported := sessions.(runtime.SessionViewPreparer)
	tabs, active := owner.GetTabs()
	tabCmd := m.setTabs(tabs, active)
	return tea.Batch(tabCmd, func() tea.Msg {
		if !preparedSupported {
			lookup, ok := services.(subagentSessionLookup)
			if !ok {
				return subagentOpenedMsg{request: r, err: runtime.ErrUnsupported}
			}
			info, ok := lookup.SubagentAttachInfo(nodeID)
			if !ok || info.Session == nil {
				return subagentOpenedMsg{request: r, err: runtime.ErrUnsupported}
			}
			info.Session = info.Session.Clone()
			a := newAttachedSubagentApp(lifetime, sessions, services, info, runtime.SessionBinding{AgentName: info.Agent, Model: info.Session.AgentModelOverrides[info.Agent]})
			if ctx.Err() != nil {
				a.Close()
				return subagentOpenedMsg{request: r, err: ctx.Err()}
			}
			return subagentOpenedMsg{request: r, application: a}
		}
		prepared, err := owner.AcquireSessionView(ctx, target)
		result := subagentOpenedMsg{request: r, prepared: prepared, err: err}
		if err == nil {
			committed, commitErr := prepared.Commit(ctx)
			result.err = commitErr
			if commitErr == nil {
				result.application, result.err = prepared.NewApp(lifetime, committed)
			}
		}
		if ctx.Err() != nil {
			if result.application != nil {
				result.application.Close()
				result.application = nil
			}
			if prepared != nil {
				prepared.Abort()
			}
			result.err = ctx.Err()
		}
		return result
	})
}

func (m *appModel) finishSubagentOpening(result subagentOpenedMsg) tea.Cmd {
	r := result.request
	discard := func() tea.Msg {
		if result.application != nil {
			result.application.Close()
		}
		if result.prepared != nil {
			result.prepared.Abort()
		}
		return nil
	}
	if r == nil {
		return discard
	}
	generation, exists := m.supervisor.RouteGeneration(r.focus)
	if m.opening != r || m.contextClosed || !exists || generation != r.generation || m.application != r.origin || m.paneFocus() != r.focus {
		return discard
	}
	if result.err != nil || result.application == nil {
		m.cancelSessionOpening()
		return tea.Batch(discard, notification.ErrorCmd(fmt.Sprintf("Cannot open subagent: %v", result.err)))
	}
	a := result.application
	if a.Session() == nil || a.Session().ID != r.target {
		m.cancelSessionOpening()
		return tea.Batch(discard, notification.ErrorCmd("Subagent session identity changed"))
	}
	if existing := m.supervisor.FindBySession(r.target); existing != nil {
		m.cancelSessionOpening()
		_, cmd := m.handleSwitchTab(existing.ID)
		return tea.Batch(discard, cmd)
	}
	id, err := m.supervisor.AddSession(m.ctx(), a, a.Session(), a.Session().WorkingDir, nil)
	if err != nil {
		m.cancelSessionOpening()
		return tea.Batch(discard, notification.ErrorCmd(err.Error()))
	}
	if result.prepared != nil {
		result.prepared.Abort()
	}
	r.installed = true
	r.target = id
	// Commit canonical focus only after validating the complete acquisition.
	m.createSessionComponents(id, a, a.Session())
	chat.EnableAsyncReplay(m.chatPages[id])
	initCmd := m.routePaneCmd(id, m.chatPages[id].Init())
	m.opening = nil
	_, cmd := m.handleSwitchTab(id)
	m.opening = r
	cmd = tea.Batch(cmd, initCmd, m.routePaneCmd(id, chat.WatchGitBranch(m.chatPages[id])), m.editors[id].Init())
	m.editor.Blur()
	tabs, active := m.supervisor.GetTabs()
	return tea.Batch(cmd, m.setTabs(tabs, active))
}

func (m *appModel) cancelSessionOpening() {
	r := m.opening
	if r == nil {
		return
	}
	m.opening = nil
	r.cancel()
	r.spinner.Stop()
	r.transition.Cancel()
	m.viewCacheValid = false
	tabs, active := m.supervisor.GetTabs()
	m.setTabs(tabs, active)
}

func (m *appModel) tickSessionOpening(tick animation.TickMsg) tea.Cmd {
	r := m.opening
	if r == nil {
		return nil
	}
	tick.MarkDirty()
	if r.phase != openingChatIn {
		r.spinner.Update(tick)
	}
	switch r.phase {
	case openingWaiting:
		if r.installed && !chat.Loading(m.chatPage) && m.ar.Now()-r.started >= animation.LoadingMinDuration {
			r.phase = openingLoaderOut
			r.transition.Start(animation.ShortDuration, animation.EaseInOutCubic)
		}
	case openingLoaderOut:
		r.transition.Tick()
		if !r.transition.Running() {
			r.spinner.Stop()
			r.phase = openingChatIn
			r.transition.Start(animation.MediumDuration, animation.EaseOutCubic)
		}
	case openingChatIn:
		r.transition.Tick()
		if !r.transition.Running() {
			m.cancelSessionOpening()
			return m.editor.Focus()
		}
	}
	return nil
}

func (m *appModel) openingTabs(tabs []messages.TabInfo, active int) ([]messages.TabInfo, int) {
	r := m.opening
	if r == nil {
		return tabs, active
	}
	tabs = slices.Clone(tabs)
	for i := range tabs {
		if tabs[i].SessionID == r.target {
			return tabs, i
		}
	}
	return append(tabs, messages.TabInfo{SessionID: r.target, Title: r.title, AgentName: r.agent, AgentNodeID: r.node, IsAttached: true}), len(tabs)
}

func (m *appModel) openingContent() string {
	r := m.opening
	if r == nil {
		return ""
	}
	if r.phase == openingChatIn {
		var content string
		if m.panePresentationEnabled() {
			content = m.composePanes()
		} else {
			content = m.chatPage.View()
		}
		fade := styles.NewFadeContext()
		lines := strings.Split(content, "\n")
		for i := range lines {
			lines[i] = styles.FadeLineCtx(lines[i], r.transition.Value(), &fade)
		}
		return strings.Join(lines, "\n")
	}
	content := r.spinner.View()
	if r.phase == openingLoaderOut {
		fade := styles.NewFadeContext()
		content = styles.FadeLineCtx(content, 1-r.transition.Value(), &fade)
	}
	return paneClipped(lipgloss.Place(m.width, m.contentHeight, lipgloss.Center, lipgloss.Center, content), m.width, m.contentHeight)
}

func (m *appModel) openingInput(msg tea.Msg) (bool, tea.Cmd) {
	if m.opening == nil {
		return false, nil
	}
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+w", "esc":
			m.cancelSessionOpening()
			return true, m.editor.Focus()
		case "ctrl+c", "ctrl+q":
			m.cancelSessionOpening()
			return false, nil
		case "ctrl+n", "ctrl+p", "ctrl+tab", "ctrl+shift+tab", "alt+left", "alt+right":
			m.cancelSessionOpening()
			return false, nil
		default:
			return true, nil
		}
	case tea.PasteMsg, messages.SendMsg:
		return true, nil
	case tea.MouseClickMsg:
		if m.opening.phase == openingChatIn {
			m.cancelSessionOpening()
			return false, m.editor.Focus()
		}
		if m.hitTestRegion(msg.Y) == regionTabBar {
			return false, nil
		}
		return true, nil
	case tea.MouseWheelMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg:
		return true, nil
	}
	return false, nil
}

func (m *appModel) openingComposer() string {
	if m.opening == nil {
		return m.composerView()
	}
	return paneClipped(lipgloss.Place(m.width, m.editorHeight, lipgloss.Center, lipgloss.Center, styles.MutedStyle.Render("Opening "+m.opening.title+" · Esc cancels")), m.width, m.editorHeight)
}
