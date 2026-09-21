package chat

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/runtime"
	list "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/core"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type replayOwnerMsg struct {
	owner *chatPage
	inner tea.Msg
}

type replayPreparedMsg struct {
	inputReplay lifecycle.InputReplay
	generation  uint64
	prepared    list.PreparedReplay
	snapshot    runtime.SessionSnapshot
	media       map[int][]types.AssistantMedia
	requests    []generatedMediaRequest
}
type replayContinueMsg struct{ generation uint64 }

type pageReplay struct {
	metadataApplied bool
	generation      uint64
	preparing       bool
	queued          []tea.Msg
	snapshot        runtime.SessionSnapshot
	requests        []generatedMediaRequest
}

// EnableAsyncReplay is selected before Init for a newly opened attachment.
func EnableAsyncReplay(page Page) {
	if p, ok := page.(*chatPage); ok {
		p.asyncReplay = true
	}
}

// Loading reports whether the authoritative transcript is still being installed.
func Loading(page Page) bool { p, ok := page.(*chatPage); return ok && p.replay != nil }

func (p *chatPage) beginReplay(snapshot runtime.SessionSnapshot) tea.Cmd {
	p.replayGeneration++
	generation := p.replayGeneration
	p.replay = &pageReplay{generation: generation, preparing: true}
	canResolve := p.app.CanResolveGeneratedFiles()
	return p.replayCommand(func() tea.Msg {
		prepared := list.PrepareReplay(snapshot.Session)
		var inputReplay lifecycle.InputReplay
		inputReplay.Reset(prepared.Session)
		snapshot.Session = prepared.Session
		var media map[int][]types.AssistantMedia
		var requests []generatedMediaRequest
		if canResolve {
			media, requests = collectGeneratedMedia(prepared.Session)
		}
		return replayPreparedMsg{inputReplay: inputReplay, generation: generation, prepared: prepared, snapshot: snapshot, media: media, requests: requests}
	})
}

func (p *chatPage) replayCommand(cmd tea.Cmd) tea.Cmd {
	id := p.routingID
	if id == "" {
		return cmd
	}
	owner := p
	routed := core.MapCommand(cmd, func(msg tea.Msg) tea.Msg {
		return msgtypes.RoutedMsg{SessionID: id, Inner: replayOwnerMsg{owner: owner, inner: msg}}
	})
	if routed != nil {
		p.pendingTimers = append(p.pendingTimers, routed)
	}
	return routed
}

func (p *chatPage) updateReplay(msg tea.Msg) (bool, tea.Cmd) {
	if owned, ok := msg.(replayOwnerMsg); ok {
		if owned.owner != p {
			return true, nil
		}
		msg = owned.inner
	}
	switch msg := msg.(type) {
	case list.ReplayRenderedMsg:
		list.ApplyReplayRender(p.messages, msg)
		if p.replay == nil {
			return true, nil
		}
		generation := p.replay.generation
		return true, p.replayCommand(func() tea.Msg { return replayContinueMsg{generation: generation} })
	case replayPreparedMsg:
		if p.replay == nil || p.replay.generation != msg.generation {
			return true, nil
		}
		p.replay.preparing = false
		p.replay.snapshot = msg.snapshot
		p.replay.requests = msg.requests
		p.inputReplay = msg.inputReplay
		p.snapshotEnd = msg.snapshot.TranscriptPosition
		list.BeginReplay(p.messages, msg.prepared, msg.media)
		return true, p.replayCommand(func() tea.Msg { return replayContinueMsg{generation: msg.generation} })
	case replayContinueMsg:
		if p.replay == nil || p.replay.generation != msg.generation || p.replay.preparing {
			return true, nil
		}
		done, cmd := list.ContinueReplay(p.messages)
		if !done {
			if list.ReplayWaiting(p.messages) {
				return true, p.replayCommand(cmd)
			}
			return true, tea.Batch(cmd, p.replayCommand(func() tea.Msg { return msg }))
		}
		r := p.replay
		cmds := []tea.Cmd{cmd}
		if !r.metadataApplied {
			r.metadataApplied = true
			cmds = append(cmds, p.finishReplayProjection(r.snapshot), p.resolveGeneratedMediaCmd(r.requests))
		}
		deadline := time.Now().Add(4 * time.Millisecond)
		for count := 0; len(r.queued) > 0 && count < 32; count++ {
			event := r.queued[0]
			r.queued[0] = nil
			r.queued = r.queued[1:]
			_, next := p.handleRuntimeEvent(event)
			cmds = append(cmds, next)
			if time.Now().After(deadline) {
				break
			}
		}
		if len(r.queued) > 0 || list.ReconcileReplay(p.messages) {
			cmds = append(cmds, p.replayCommand(func() tea.Msg { return msg }))
			return true, tea.Batch(cmds...)
		}
		p.replay = nil
		return true, tea.Batch(cmds...)
	}
	if p.replay != nil {
		event := msg
		if bridged, ok := msg.(msgtypes.SessionRuntimeEventMsg); ok {
			event = bridged.Event
		}
		switch event.(type) {
		case *app.SessionResetEvent:
			return false, nil
		case runtime.Event:
			p.replay.queued = append(p.replay.queued, msg)
			return true, nil
		}
	}
	return false, nil
}

func (p *chatPage) finishReplayProjection(snapshot runtime.SessionSnapshot) tea.Cmd {
	// The transcript was already installed in bounded turns. Only projection
	// metadata is applied here; the reset must not synchronously replay it again.
	return p.applyProjectionMetadata(snapshot)
}
