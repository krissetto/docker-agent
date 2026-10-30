package facade

import "errors"

var (
	// ErrUnknownInteraction means no interaction with the given ID is or was outstanding.
	ErrUnknownInteraction = errors.New("unknown interaction")
	// ErrStaleInteraction means the interaction was outstanding but has already been resolved or superseded.
	ErrStaleInteraction = errors.New("stale interaction")
	// ErrAmbiguousInteraction means a Response without an InteractionID could not pick one of several outstanding interactions.
	ErrAmbiguousInteraction = errors.New("ambiguous interaction")
	// ErrNotActive means the operation requires an active turn or interaction and none exists.
	ErrNotActive = errors.New("not active")
	// ErrInvalidResponse means the response does not match the interaction's kind or payload.
	ErrInvalidResponse = errors.New("invalid response")
	// ErrUnsupported means an operation was invoked that the implementation cannot perform.
	// Implementations must not return it for an advertised capability.
	ErrUnsupported = errors.New("unsupported")
	// ErrSessionClosed means the session handle was closed.
	ErrSessionClosed = errors.New("session closed")
	// ErrBusy means the operation needs an idle session and the session is busy.
	ErrBusy = errors.New("session busy")
	// ErrUnknownAgent means the named agent is not part of the session's team.
	ErrUnknownAgent = errors.New("unknown agent")
)
