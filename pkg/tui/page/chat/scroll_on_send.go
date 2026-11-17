package chat

import "github.com/docker/docker-agent/pkg/runtime"

// This receipt is local presentation state, never a journal/runtime event.
// Its scope prevents a closed or reloaded page from moving a replacement view.
type localSendAcceptedMsg struct {
	inputResult
	submission runtime.Submission
}

func (p *chatPage) scrollToAcceptedSend(msg localSendAcceptedMsg) {
	s := msg.submission
	if !p.ownsInputScope(msg.scope) || !p.scrollOnSend || s.TurnID == "" ||
		s.SessionID != p.app.Session().ID || s.Disposition == runtime.SubmissionDispositionQueued {
		return
	}
	if p.scrolledSends[s.TurnID] {
		return
	}
	if p.scrolledSends == nil {
		p.scrolledSends = make(map[string]bool)
	}
	p.scrolledSends[s.TurnID] = true
	// The viewport defers until this exact nonpending user card exists. The
	// receipt and transcript event can arrive in either order.
	p.messages.ScrollToSentInput(s.TurnID)
}
