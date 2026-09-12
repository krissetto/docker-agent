package app

import (
	"context"

	"github.com/docker/docker-agent/pkg/runtime"
)

// appProjectionSink adapts the transport-neutral client attachment to App's
// event bus. The generic attachment owns gap recovery and always Reset()s from
// a genuinely fresh snapshot before applying the replacement tail.
type appProjectionSink struct {
	app       *App
	ctx       context.Context //nolint:containedctx // projection callbacks outlive the attachment setup call
	sessionID string
	epoch     uint64
	position  int
	pending   map[string]struct{}
}

func (s *appProjectionSink) Reset(snapshot runtime.SessionSnapshot) {
	s.position = snapshot.TranscriptPosition
	if snapshot.Session != nil {
		event := runtime.SessionTitle(s.sessionID, snapshot.Session.TitleSnapshot())
		if !s.app.sendBridgedEventFrom(s.ctx, "", event, true, s.sessionID, s.epoch) {
			return
		}
	}
	// Snapshot reset is authoritative when the session supplied status identity.
	// (Compatibility test/fake snapshots without status remain event-neutral.)
	if snapshot.Status.SessionID != "" {
		if !s.app.sendBridgedEventFrom(s.ctx, "", runtime.PauseChanged(s.sessionID, snapshot.Status.AgentName, snapshot.Status.PauseArmed), true, s.sessionID, s.epoch) {
			return
		}
		if snapshot.Status.PauseArmed && snapshot.Status.Paused {
			if !s.app.sendBridgedEventFrom(s.ctx, "", runtime.Paused(s.sessionID, snapshot.Status.AgentName), true, s.sessionID, s.epoch) {
				return
			}
		}
	}
	next := make(map[string]struct{}, len(snapshot.PendingInputs))
	for _, pending := range snapshot.PendingInputs {
		next[pending.TurnID] = struct{}{}
	}
	// A replacement snapshot is authoritative. Remove projected entries that
	// disappeared while observation was disconnected before seeding its FIFO.
	for turnID := range s.pending {
		if _, ok := next[turnID]; ok {
			continue
		}
		event := runtime.PendingUserMessageCanceled(s.sessionID, turnID, -1)
		if !s.app.sendBridgedEventFrom(s.ctx, turnID, event, true, s.sessionID, s.epoch) {
			return
		}
	}
	s.pending = next
	for _, pending := range snapshot.PendingInputs {
		event := runtime.PendingUserMessageAccepted(s.sessionID, pending.TurnID, pending.Content, pending.MultiContent, pending.SessionPosition)
		if !s.app.sendBridgedEventFrom(s.ctx, pending.TurnID, event, true, s.sessionID, s.epoch) {
			return
		}
	}
}

func (s *appProjectionSink) Apply(envelope runtime.SessionEvent) {
	_, accepted := envelope.Event.(*runtime.PendingUserMessageAcceptedEvent)
	_, promoted := envelope.Event.(*runtime.PendingUserMessagePromotedEvent)
	withdrawal, canceled := envelope.Event.(*runtime.PendingUserMessageCanceledEvent)
	if canceled && withdrawal.SessionPosition >= 0 && withdrawal.SessionPosition < s.position {
		s.position--
	}
	if accepted {
		if s.pending == nil {
			s.pending = make(map[string]struct{})
		}
		s.pending[envelope.TurnID] = struct{}{}
	}
	if promoted || canceled {
		delete(s.pending, envelope.TurnID)
	}
	if !accepted && !promoted && !canceled && envelope.TranscriptPosition >= 0 && envelope.TranscriptPosition < s.position {
		return
	}
	if !accepted && !promoted && !canceled && envelope.TranscriptPosition >= s.position {
		s.position = envelope.TranscriptPosition + 1
	}
	s.app.sendBridgedEventFrom(s.ctx, envelope.TurnID, envelope.Event, false, s.sessionID, s.epoch)
}

func (s *appProjectionSink) OnError(err error) {
	if s.ctx.Err() == nil {
		s.app.sendEvent(s.ctx, runtime.Error(err.Error()))
	}
}
