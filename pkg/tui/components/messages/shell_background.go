package messages

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/tool/subagenttool"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const backgroundOutputLimit = 100_000

type backgroundSnapshotMsg struct {
	sessionID, processID string
	snapshot             tools.BackgroundProcessOutput
	err                  error
}

// Commands serialize writes and discard superseded sets without blocking Update.
type backgroundInterest struct {
	sessionID    string
	setter       func([]string) error
	ids          []string
	generation   atomic.Uint64
	execution    sync.Mutex
	applied      uint64
	previous     tea.Cmd
	previousOnce sync.Once
}

type backgroundInterestMsg struct {
	owner *backgroundInterest
	err   error
}

func (b *backgroundInterest) command(ids []string) tea.Cmd {
	if slices.Equal(b.ids, ids) {
		return nil
	}
	b.ids = slices.Clone(ids)
	generation := b.generation.Add(1)
	return func() tea.Msg {
		b.execution.Lock()
		defer b.execution.Unlock()
		if b.generation.Load() != generation || b.applied == generation {
			return nil
		}
		b.applied = generation
		b.previousOnce.Do(func() {
			if b.previous != nil {
				b.previous()
			}
		})
		return backgroundInterestMsg{owner: b, err: b.setter(ids)}
	}
}

func (m *model) SetShellBackgroundOutputInterest(sessionID string, setter func([]string) error) tea.Cmd {
	if m.backgroundInterest != nil && m.backgroundInterest.sessionID == sessionID {
		return nil
	}
	var clear tea.Cmd
	if m.backgroundInterest != nil {
		clear = m.backgroundInterest.command(nil)
	}
	m.backgroundInterest = &backgroundInterest{sessionID: sessionID, setter: setter, previous: clear}
	return tea.Sequence(clear, m.syncBackgroundOutput())
}

func (m *model) SetShellBackgroundOutputVisible(visible bool) tea.Cmd {
	m.backgroundOutputHidden = !visible
	return m.syncBackgroundOutput()
}

func (m *model) SyncShellBackgroundOutput() tea.Cmd { return m.syncBackgroundOutput() }

func (m *model) HasAmbiguousShellBackgroundProcess(processID string) bool {
	count := 0
	m.shellCards(func(_ int, msg *types.Message, _ layout.Model) {
		if msg.Background != nil && msg.Background.ProcessID == processID {
			count++
		}
	})
	return count > 1
}

func (m *model) shellCards(visit func(int, *types.Message, layout.Model)) {
	for i, msg := range m.messages {
		if msg.Type == types.MessageTypeToolCall {
			visit(i, msg, m.views[i])
		}
		if block, ok := m.views[i].(*reasoningblock.Model); ok {
			block.ToolCards(func(msg *types.Message, view layout.Model) { visit(i, msg, view) })
		}
	}
}

func (m *model) invalidateShell(index int) {
	if block, ok := m.views[index].(*reasoningblock.Model); ok {
		block.InvalidateTools()
	}
	m.invalidateItem(index)
}

// SetShellBackgroundProcesses accepts only already-verified same-session call links.
func (m *model) SetShellBackgroundProcesses(sessionID string, processes []tools.BackgroundProcess, source func(string) (tools.BackgroundProcessOutput, error)) tea.Cmd {
	if m.backgroundSession != sessionID {
		m.backgroundWatching = nil
		m.backgroundFetching = nil
	}
	m.backgroundSession, m.backgroundSource = sessionID, source
	m.backgroundReplayCalls, m.backgroundReplayProcesses = nil, nil
	m.backgroundReplayHandoffs = nil
	var commands []tea.Cmd
	m.shellCards(func(i int, msg *types.Message, view layout.Model) {
		bg := msg.Background
		if bg == nil {
			return
		}
		found := false
		for _, process := range processes {
			if m.HasAmbiguousShellBackgroundProcess(bg.ProcessID) || process.SessionID != sessionID || process.ID != bg.ProcessID || process.ToolCallID != msg.ToolCall.ID || !m.HasUniqueToolCall(msg.ToolCall.ID) {
				continue
			}
			found = true
			// Completion text is durable evidence even after the process list is pruned.
			if bg.Authoritative && bg.State != tools.BackgroundProcessRunning && bg.State != "unknown" && bg.State != "" {
				break
			}
			bg.Authoritative, bg.State = true, process.State
			bg.ExitCode, bg.TimedOut = process.ExitCode, process.TimedOut
			bg.StartedAt, bg.FinishedAt = process.StartedAt, process.FinishedAt
			break
		}
		if !found && bg.Authoritative && bg.State == tools.BackgroundProcessRunning {
			bg.State = "unknown"
		}
		m.invalidateShell(i)
		if resumer, ok := view.(interface{ ResumeAnimation() tea.Cmd }); ok {
			commands = append(commands, resumer.ResumeAnimation())
		}
	})
	commands = append(commands, m.syncBackgroundOutput())
	return tea.Batch(commands...)
}

func (m *model) applyShellCompletion(input *session.Message, positions ...int) bool {
	if input == nil || input.InputOrigin != session.InputOriginRuntime || input.InputMode != "steer" || input.SenderID != "" {
		return false
	}
	completion, ok := tools.ParseShellCompletion(subagenttool.RuntimeNoticeBody(types.Input(input)))
	if !ok {
		return false
	}
	matches := 0
	var target *types.Message
	var index int
	m.shellCards(func(i int, msg *types.Message, _ layout.Model) {
		if msg.Background != nil && msg.Background.ProcessID == completion.ProcessID {
			matches++
			target, index = msg, i
		}
	})
	if matches != 1 || !m.HasUniqueToolCall(target.ToolCall.ID) {
		return false
	}
	if m.backgroundReplayCalls != nil && (m.backgroundReplayCalls[target.ToolCall.ID] != 1 || m.backgroundReplayProcesses[completion.ProcessID] != 1) {
		return false
	}
	if len(positions) > 0 && m.backgroundReplayHandoffs != nil {
		position, exists := m.backgroundReplayHandoffs[completion.ProcessID]
		if !exists || positions[0] <= position {
			return false
		}
	}
	bg := target.Background
	bg.Authoritative, bg.State, bg.ExitCode, bg.TimedOut = true, completion.Status, completion.ExitCode, completion.TimedOut
	if bg.FinishedAt.IsZero() {
		bg.FinishedAt = time.Now()
	}
	target.Content = completion.Output
	if len(target.Content) > backgroundOutputLimit {
		target.Content = target.Content[len(target.Content)-backgroundOutputLimit:]
	}
	m.invalidateShell(index)
	return true
}

func (m *model) backgroundCardVisible(i int, msg *types.Message, view layout.Model) bool {
	expanded, ok := view.(interface{ IsExpanded() bool })
	if !ok || !expanded.IsExpanded() || m.height <= 0 || m.backgroundOutputHidden {
		return false
	}
	start := m.slotOffset(i)
	if block := m.workBlockForSource(i); block != nil {
		if !block.IsExpanded() {
			return false
		}
		found := false
		for _, row := range m.blockRows(block) {
			if row.SourceIndex == i {
				start = m.workRangeStart(i) + row.Start
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if reasoning, ok := m.views[i].(*reasoningblock.Model); ok {
		if !reasoning.ToolCardsVisible() {
			return false
		}
		local, exists := reasoning.ToolCallLine(msg.ToolCall.ID)
		if !exists {
			return false
		}
		start += local
	}
	rows := 1
	for _, r := range view.View() {
		if r == '\n' {
			rows++
		}
	}
	return start < m.scrollOffset+m.height && start+rows > m.scrollOffset
}

func (m *model) syncBackgroundOutput() tea.Cmd {
	if m.backgroundWatching == nil {
		m.backgroundWatching = make(map[string]bool)
	}
	if m.backgroundFetching == nil {
		m.backgroundFetching = make(map[string]bool)
	}
	var commands []tea.Cmd
	var ids []string
	watching := make(map[string]bool)
	m.shellCards(func(i int, msg *types.Message, view layout.Model) {
		bg := msg.Background
		if bg == nil {
			return
		}
		visible := bg.Authoritative && bg.State == tools.BackgroundProcessRunning && m.HasUniqueToolCall(msg.ToolCall.ID) && !m.HasAmbiguousShellBackgroundProcess(bg.ProcessID) && m.backgroundCardVisible(i, msg, view)
		if visible {
			ids = append(ids, bg.ProcessID)
		}
		wasVisible := m.backgroundWatching[bg.ProcessID]
		watching[bg.ProcessID] = visible
		if !visible || wasVisible || m.backgroundFetching[bg.ProcessID] || !bg.Authoritative {
			return
		}
		commands = append(commands, m.fetchBackgroundOutput(bg.ProcessID))
	})
	m.backgroundWatching = watching
	slices.Sort(ids)
	var interest tea.Cmd
	if owner := m.backgroundInterest; owner != nil && owner.setter != nil {
		if owner.sessionID != m.backgroundSession {
			ids = nil
		}
		interest = owner.command(ids)
	}
	if interest == nil {
		return tea.Batch(commands...)
	}
	return tea.Sequence(interest, tea.Batch(commands...))
}

func (m *model) fetchBackgroundOutput(processID string) tea.Cmd {
	if m.backgroundSource == nil {
		return nil
	}
	if m.backgroundFetching == nil {
		m.backgroundFetching = make(map[string]bool)
	}
	if m.backgroundFetching[processID] {
		return nil
	}
	m.backgroundFetching[processID] = true
	source, sessionID := m.backgroundSource, m.backgroundSession
	return func() tea.Msg {
		snapshot, err := source(processID)
		return backgroundSnapshotMsg{sessionID: sessionID, processID: processID, snapshot: snapshot, err: err}
	}
}

func (m *model) applyBackgroundSnapshot(result backgroundSnapshotMsg) {
	if result.sessionID != m.backgroundSession {
		return
	}
	delete(m.backgroundFetching, result.processID)
	m.shellCards(func(i int, msg *types.Message, view layout.Model) {
		bg := msg.Background
		if bg == nil || m.HasAmbiguousShellBackgroundProcess(bg.ProcessID) || bg.ProcessID != result.processID || !m.backgroundCardVisible(i, msg, view) {
			return
		}
		if result.err != nil {
			if errors.Is(result.err, tools.ErrBackgroundProcessOutputUnsupported) || errors.Is(result.err, tools.ErrBackgroundProcessNotFound) {
				bg.OutputUnavailable = true
				m.invalidateShell(i)
			}
			return
		}
		snapshot := result.snapshot
		if bg.State != tools.BackgroundProcessRunning && snapshot.State == tools.BackgroundProcessRunning {
			return
		}
		if snapshot.SessionID != m.backgroundSession || snapshot.ProcessID != bg.ProcessID || snapshot.ToolCallID != msg.ToolCall.ID || !m.HasUniqueToolCall(msg.ToolCall.ID) || snapshot.Total < bg.OutputTotal || snapshot.Offset < 0 || snapshot.Total != snapshot.Offset+int64(len(snapshot.Output)) {
			return
		}
		bg.OutputUnavailable = false
		msg.Content = snapshot.Output
		if len(msg.Content) > backgroundOutputLimit {
			msg.Content = msg.Content[len(msg.Content)-backgroundOutputLimit:]
		}
		bg.OutputOffset, bg.OutputTotal = snapshot.Total-int64(len(msg.Content)), snapshot.Total
		m.invalidateShell(i)
	})
}

// AppendBackgroundOutput never scrolls the transcript or updates hidden bodies.
func (m *model) AppendBackgroundOutput(event *runtime.BackgroundProcessOutputEvent) tea.Cmd {
	if event == nil || event.SessionID != m.backgroundSession {
		return nil
	}
	var command tea.Cmd
	m.shellCards(func(i int, msg *types.Message, view layout.Model) {
		bg := msg.Background
		if bg == nil || m.HasAmbiguousShellBackgroundProcess(bg.ProcessID) || bg.ProcessID != event.ProcessID || msg.ToolCall.ID != event.ToolCallID || !m.HasUniqueToolCall(msg.ToolCall.ID) || !bg.Authoritative || bg.State != tools.BackgroundProcessRunning || !m.backgroundCardVisible(i, msg, view) {
			return
		}
		if event.Offset != bg.OutputTotal {
			if bg.OutputUnavailable {
				return
			}
			command = m.fetchBackgroundOutput(bg.ProcessID)
			return
		}
		bg.OutputUnavailable = false
		msg.Content += event.Output
		bg.OutputTotal += int64(len(event.Output))
		if len(msg.Content) > backgroundOutputLimit {
			msg.Content = msg.Content[len(msg.Content)-backgroundOutputLimit:]
		}
		bg.OutputOffset = bg.OutputTotal - int64(len(msg.Content))
		m.invalidateShell(i)
	})
	return command
}
