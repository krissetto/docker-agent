package runtime

import "sync"

type sessionInteractions struct {
	mu     sync.Mutex
	resume map[string]chan ResumeRequest
}

func newSessionInteractions() *sessionInteractions {
	return &sessionInteractions{resume: map[string]chan ResumeRequest{}}
}

func (s *sessionInteractions) resumeChannel(sessionID string) chan ResumeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.resume[sessionID]
	if ch == nil {
		ch = make(chan ResumeRequest, 1)
		s.resume[sessionID] = ch
	}
	return ch
}

func (s *sessionInteractions) sendResume(sessionID string, req ResumeRequest) bool {
	if sessionID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.resume[sessionID]
	if ch == nil {
		return false
	}
	select {
	case ch <- req:
		return true
	default:
		return false
	}
}

func (s *sessionInteractions) deleteSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.resume, sessionID)
}

func (s *sessionInteractions) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resume = map[string]chan ResumeRequest{}
}

func (s *sessionInteractions) removeResume(sessionID string, ch chan ResumeRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resume[sessionID] == ch {
		delete(s.resume, sessionID)
	}
}
