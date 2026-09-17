package runtime

import (
	"strings"

	"github.com/docker/docker-agent/pkg/session"
)

// quietCoordinationInput recognizes notifications to a parent, not work sent
// to a child. Origin alone is insufficient: unrelated runtime notes and agent
// task requests still require a reply. Never infer intent from message text.
func (r *LocalRuntime) quietCoordinationInput(sess *session.Session, msg QueuedMessage) bool {
	if msg.Retry || msg.SenderID == "" || !msg.trustedSteering() || r.subagents == nil {
		return false
	}
	r.subagents.mu.Lock()
	defer r.subagents.mu.Unlock()
	for id, child := range r.subagents.children {
		if child.parentSession != sess.ID {
			continue
		}
		switch msg.InputOrigin {
		case session.InputOriginAgent:
			if msg.SenderID == string(id) || msg.SenderID == child.sessionID {
				return true
			}
		case session.InputOriginRuntime:
			// Reports have a runtime-assigned, child-qualified accepted turn
			// identity. Other runtime notes from the same child do not qualify.
			prefix := "report:" + childReportID(child.sessionID, "")
			if msg.SenderID == child.sessionID && strings.HasPrefix(msg.RequestID, prefix) && len(msg.RequestID) > len(prefix) {
				return true
			}
		}
	}
	return false
}

// initialTurnInput uses the promoted request, not the transcript tail: later
// accepted FIFO inputs may already be present there but are not yet consumed.
func (r *LocalRuntime) initialTurnInput(sess *session.Session) (QueuedMessage, bool) {
	if r.sessionDrivers != nil {
		if d, ok := r.sessionDrivers.Lookup(sess.ID); ok {
			d.mu.Lock()
			id := d.activeRequestID
			d.mu.Unlock()
			if id != "" {
				for _, item := range sess.MessagesSnapshot() {
					if msg := item.Message; msg != nil && msg.TurnID == id && msg.Accepted && !msg.Pending {
						return queuedSessionInput(msg, -1, false), true
					}
				}
				// A retry has an identity but no new transcript input. Do not
				// inherit a previous report's quiet-stop exemption.
				return QueuedMessage{Retry: true}, true
			}
		}
	}
	msg, _, ok := initialUserPrompt(sess)
	return msg, ok
}

func (ls *loopState) consumeTurnInput(quiet bool) {
	if !ls.turnInputSeen {
		ls.quietCoordinationTurn = quiet
	} else {
		ls.quietCoordinationTurn = ls.quietCoordinationTurn && quiet
	}
	ls.turnInputSeen = true
	if !quiet {
		// Tool work before a new user/task boundary cannot answer that input.
		ls.prevTurnMadeToolCalls = false
	}
}
