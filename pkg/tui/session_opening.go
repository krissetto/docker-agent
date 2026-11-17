package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/widgets/key"
)

// Keys that abandon a pending session opening; the key then reaches normal routing.
var (
	sessionOpeningQuitKeys = key.NewBinding(key.WithKeys("ctrl+c", "ctrl+q"))
	sessionOpeningTabKeys  = key.NewBinding(key.WithKeys("ctrl+n", "ctrl+p", "ctrl+tab", "ctrl+shift+tab", "alt+left", "alt+right"))
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
	id                         uint64
	queued                     []tea.Msg
	queuedBytes                int
	overflowed                 bool
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
	// Fade the complete loader from its first frame within the existing minimum
	// loading interval, without delaying acquisition or adding another hold.
	r.transition.Start(animation.LoadingMinDuration, animation.EaseOutCubic)
	m.openingSeq++
	r.id = m.openingSeq
	m.opening = r
	m.editor.Blur()
	owner := m.supervisor
	tabs, active := owner.GetTabs()
	tabCmd := m.setTabs(tabs, active)
	return tea.Batch(tabCmd, func() tea.Msg {
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
	id, initCmd, err := m.adoptSessionView(a, a.Session(), a.Session().WorkingDir, sessionViewPolicy{asyncReplay: true})
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
	m.opening = nil
	_, cmd := m.handleSwitchTab(id)
	m.opening = r
	cmd = tea.Batch(cmd, initCmd)
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
	m.retainOpeningInput(r.target, r.title, r.takeQueued())
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
		r.transition.Tick()
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
			queued := r.takeQueued()
			m.cancelSessionOpening()
			focus := m.editor.Focus()
			return tea.Batch(focus, m.replayOpeningInput(r, queued))
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
	// Only the transcript fades in; the live composer and footer keep their
	// normal surfaces and measured rows throughout acquisition and presentation.
	if r.phase == openingChatIn {
		if m.panePresentationEnabled() {
			return fadeOpeningContent(m.composePanes(), r.transition.Value())
		}
		return fadeOpeningContent(m.chatPage.View(), r.transition.Value())
	}
	alpha := r.transition.Value()
	if r.phase == openingLoaderOut {
		alpha = 1 - alpha
	}
	content := fadeOpeningContent(r.spinner.View(), alpha)
	return paneClipped(lipgloss.Place(m.width, m.contentHeight, lipgloss.Center, lipgloss.Center, content), m.width, m.contentHeight)
}

func (m *appModel) openingInput(msg tea.Msg) (bool, tea.Cmd) {
	if m.opening == nil {
		return false, nil
	}
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, core.GetKeys().CloseTab, core.UIKeys.Cancel):
			m.cancelSessionOpening()
			return true, m.editor.Focus()
		case key.Matches(msg, sessionOpeningQuitKeys, sessionOpeningTabKeys):
			m.cancelSessionOpening()
			return false, nil
		default:
			return true, m.bufferOpeningInput(msg)
		}
	case tea.PasteMsg:
		return true, m.bufferOpeningInput(msg)
	case messages.RoutedMsg:
		send, ok := msg.Inner.(messages.SendMsg)
		if !ok {
			return false, nil
		}
		return true, m.restoreOpeningRoutedSend(msg.SessionID, send)
	case messages.SendMsg:
		// Origin-stamped sends must not be replayed into the opening target.
		m.openingNotices = append(m.openingNotices, notification.WarningCmd("Message not sent while the subagent view is opening"))
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

func fadeOpeningContent(content string, alpha float64) string {
	if alpha >= 1 {
		return content
	}
	fade := styles.NewFadeContext()
	lines := strings.Split(content, "\n")
	for i := range lines {
		lines[i] = styles.FadeLineCtx(lines[i], alpha, &fade)
	}
	return strings.Join(lines, "\n")
}

// openingInputLimit bounds the input kept while a subagent view is opening.
const openingInputLimit = 4 << 10

// openingInputReplayMsg replays input buffered for one opening, only if it still
// addresses the same target after the overlay has closed.
type openingInputReplayMsg struct {
	id     uint64
	target string
	title  string
	msgs   []tea.Msg
}

func (r *sessionOpening) takeQueued() []tea.Msg {
	queued := r.queued
	r.queued, r.queuedBytes = nil, 0
	return queued
}

func openingInputSize(msg tea.Msg) int {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		return len(msg.Content)
	case tea.KeyPressMsg:
		switch {
		case msg.Text != "":
			return len(msg.Text)
		case msg.String() == "enter", msg.String() == "backspace":
			return 1
		}
	}
	return 0
}

func (m *appModel) bufferOpeningInput(msg tea.Msg) tea.Cmd {
	r := m.opening
	size := openingInputSize(msg)
	if r == nil || size == 0 {
		return nil
	}
	if r.queuedBytes+size > openingInputLimit {
		if r.overflowed {
			return nil
		}
		r.overflowed = true
		return notification.WarningCmd("Input typed while opening is limited to 4 KB; the rest was not kept")
	}
	r.queued = append(r.queued, msg)
	r.queuedBytes += size
	return nil
}

// openingInputText renders buffered keys as plain draft text. Enter becomes a
// newline, so nothing is submitted by retention.
func openingInputText(msgs []tea.Msg) string {
	text := ""
	for _, msg := range msgs {
		switch msg := msg.(type) {
		case tea.PasteMsg:
			text += msg.Content
		case tea.KeyPressMsg:
			switch msg.String() {
			case "enter":
				text += "\n"
			case "backspace":
				if _, size := utf8.DecodeLastRuneInString(text); size > 0 {
					text = text[:len(text)-size]
				}
			default:
				text += msg.Text
			}
		}
	}
	return text
}

func (m *appModel) replayOpeningInput(r *sessionOpening, msgs []tea.Msg) tea.Cmd {
	if len(msgs) == 0 {
		return nil
	}
	return func() tea.Msg {
		return openingInputReplayMsg{id: r.id, target: r.target, title: r.title, msgs: msgs}
	}
}

func (m *appModel) handleOpeningInputReplay(msg openingInputReplayMsg) (tea.Model, tea.Cmd) {
	if m.opening != nil || msg.id != m.openingSeq || m.contextClosed || m.supervisor.ActiveID() != msg.target || m.paneFocus() != msg.target || m.application == nil || m.application.Session() == nil || m.application.Session().ID != msg.target {
		m.retainOpeningInput(msg.target, msg.title, msg.msgs)
		return m, nil
	}
	var cmds []tea.Cmd
	for _, input := range msg.msgs {
		_, cmd := m.updateWithLifecycle(input)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// retainOpeningInput keeps unsent text as a draft of its intended target only.
// A composer receives the text directly. Otherwise it is held per target, and the
// held text itself is the budget: consuming it at composer creation releases it.
func (m *appModel) retainOpeningInput(target, title string, msgs []tea.Msg) {
	text := openingInputText(msgs)
	if text == "" {
		return
	}
	if title == "" {
		title = target
	}
	if ed := m.editors[target]; ed != nil {
		ed.SetValue(ed.Value() + text)
		m.openingNotices = append(m.openingNotices, notification.InfoCmd("Unsent input kept for "+title+"; it was not sent"))
		return
	}
	held := m.retainedOpeningInput[target]
	room := openingInputLimit - len(held)
	if room <= 0 {
		m.openingNotices = append(m.openingNotices, notification.WarningCmd("Unsent input for "+title+" was not kept: the 4 KB limit is reached"))
		return
	}
	kept, dropped := text, ""
	if len(text) > room {
		cut := room
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		kept, dropped = text[:cut], text[cut:]
	}
	if m.retainedOpeningInput == nil {
		m.retainedOpeningInput = map[string]string{}
	}
	m.retainedOpeningInput[target] = held + kept
	if dropped != "" {
		m.openingNotices = append(m.openingNotices, notification.WarningCmd("Unsent input kept for "+title+" up to the 4 KB limit; the rest was not kept"))
		return
	}
	m.openingNotices = append(m.openingNotices, notification.InfoCmd("Unsent input kept for "+title+"; it was not sent"))
}

// restoreOpeningRoutedSend keeps an origin-stamped composer submission in its origin
// draft. It is never dispatched during opening and never reaches the opening target.
func (m *appModel) restoreOpeningRoutedSend(origin string, send messages.SendMsg) tea.Cmd {
	ed := m.editors[origin]
	if origin == "" || origin == m.opening.target || ed == nil {
		m.openingNotices = append(m.openingNotices, notification.WarningCmd("Message not sent while the subagent view is opening"))
		return nil
	}
	ed.SetValue(ed.Value() + send.Content)
	text := "Message not sent while the subagent view is opening; its text was kept in the origin draft"
	if len(send.Attachments) > 0 {
		text += ", attachments were not kept"
	}
	m.openingNotices = append(m.openingNotices, notification.WarningCmd(text))
	return nil
}

func (m *appModel) takeOpeningNotices() []tea.Cmd {
	notices := m.openingNotices
	m.openingNotices = nil
	return notices
}
