package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
)

type backgroundOutputObserverKey struct{ sessionID, observerID string }
type backgroundOutputObserver struct {
	mu       sync.Mutex
	closed   bool
	interest map[string][]string
	set      func(string, []string) error
}

func validBackgroundOutputInterest(interest map[string][]string) bool {
	data, err := json.Marshal(interest)
	if err != nil || len(data) > api.SessionBackgroundOutputInterestMaxBytes {
		return false
	}
	count := 0
	for sessionID, processes := range interest {
		if sessionID == "" {
			return false
		}
		count += len(processes)
		for _, process := range processes {
			if process == "" {
				return false
			}
		}
	}
	return count <= api.SessionBackgroundOutputInterestMaxProcesses
}

func parseBackgroundOutputObserver(request *http.Request) (string, map[string][]string, error) {
	id := request.Header.Get(api.SessionObserverHeader)
	raw := request.Header.Get(api.SessionBackgroundOutputInterestHeader)
	if id == "" && raw == "" {
		return "", nil, nil
	}
	if !supportsSessionFeature(request, api.SessionFeatureBackgroundProcessOutput) {
		return "", nil, sessionRequestError("background output interest requires the background-process-output feature")
	}
	if len(id) == 0 || len(id) > 128 || strings.ContainsAny(id, " \t\r\n/") {
		return "", nil, sessionRequestError("invalid observer ID")
	}
	interest := make(map[string][]string)
	if raw != "" && (len(raw) > api.SessionBackgroundOutputInterestMaxBytes || json.Unmarshal([]byte(raw), &interest) != nil) {
		return "", nil, sessionRequestError("invalid background output interest")
	}
	if !validBackgroundOutputInterest(interest) {
		return "", nil, sessionRequestError("background output interest exceeds limits or contains empty IDs")
	}
	return id, interest, nil
}

func (s *Server) registerBackgroundOutputObserver(sessionID, id string, interest map[string][]string, observation runtime.Observation) (*backgroundOutputObserver, func(), error) {
	noop := func() {}
	if id == "" || observation.SetBackgroundOutputInterest == nil {
		return nil, noop, nil
	}
	for member, processes := range interest {
		if err := observation.SetBackgroundOutputInterest(member, processes); err != nil {
			return nil, noop, sessionHTTPError(err)
		}
	}
	observer := &backgroundOutputObserver{interest: interest, set: observation.SetBackgroundOutputInterest}
	key := backgroundOutputObserverKey{sessionID, id}
	s.backgroundOutputMu.Lock()
	defer s.backgroundOutputMu.Unlock()
	if s.backgroundOutputObservers == nil {
		s.backgroundOutputObservers = make(map[backgroundOutputObserverKey]*backgroundOutputObserver)
	}
	if s.backgroundOutputObservers[key] != nil {
		return nil, noop, echo.NewHTTPError(http.StatusConflict, "observer already connected")
	}
	s.backgroundOutputObservers[key] = observer
	return observer, func() {
		s.backgroundOutputMu.Lock()
		defer s.backgroundOutputMu.Unlock()
		observer.mu.Lock()
		defer observer.mu.Unlock()
		observer.closed = true
		delete(s.backgroundOutputObservers, key)
	}, nil
}

func (s *Server) setBackgroundOutputInterest(c echo.Context) error {
	if !supportsSessionFeature(c.Request(), api.SessionFeatureBackgroundProcessOutput) {
		return c.NoContent(http.StatusNotImplemented)
	}
	var request api.SessionBackgroundOutputInterest
	decoder := json.NewDecoder(io.LimitReader(c.Request().Body, api.SessionBackgroundOutputInterestMaxBytes+1024))
	if err := decoder.Decode(&request); err != nil {
		return sessionRequestError("invalid background output interest")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return sessionRequestError("invalid background output interest ending")
	}
	if request.ObserverID == "" || len(request.ObserverID) > 128 || !validBackgroundOutputInterest(map[string][]string{request.SessionID: request.ProcessIDs}) {
		return sessionRequestError("invalid background output interest")
	}
	s.backgroundOutputMu.Lock()
	observer := s.backgroundOutputObservers[backgroundOutputObserverKey{c.Param("id"), request.ObserverID}]
	s.backgroundOutputMu.Unlock()
	if observer == nil {
		return echo.NewHTTPError(http.StatusNotFound, "observer not found in session")
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.closed {
		return echo.NewHTTPError(http.StatusNotFound, "observer closed")
	}
	next := make(map[string][]string, len(observer.interest)+1)
	for id, processes := range observer.interest {
		next[id] = processes
	}
	if len(request.ProcessIDs) == 0 {
		delete(next, request.SessionID)
	} else {
		next[request.SessionID] = request.ProcessIDs
	}
	if !validBackgroundOutputInterest(next) {
		return sessionRequestError("background output interest exceeds limits")
	}
	if err := observer.set(request.SessionID, request.ProcessIDs); err != nil {
		return sessionHTTPError(err)
	}
	observer.interest = next
	c.Response().Header().Set(api.SessionFeaturesHeader, api.SessionFeatureBackgroundProcessOutput)
	return c.NoContent(http.StatusNoContent)
}
