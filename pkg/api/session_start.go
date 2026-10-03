package api

// SessionStartRequest recovers creation and first input using stable identities.
// Retrying the same request is recoverable, not a database-atomic transaction.
type SessionStartRequest struct {
	SessionCreateRequest

	Input SessionInputRequest `json:"input"`
}
