package runtime

import (
	"context"
	"errors"
	"net/http"

	"github.com/docker/docker-agent/pkg/api"
)

// SessionStarter recovers creation and initial input with stable request identities.
type SessionStarter interface {
	StartSession(ctx context.Context, request api.SessionStartRequest) (Submission, error)
}

func (r *SessionTransport) StartSession(ctx context.Context, request api.SessionStartRequest) (Submission, error) {
	if request.Source == "" {
		request.Source = r.source
	}
	var result api.SessionSubmission[SubmissionDisposition]
	if err := r.client.sessionJSON(ctx, http.MethodPost, api.SessionAPIPath+"/start", request, &result); err != nil {
		return Submission{}, err
	}
	if result.SessionID != request.SessionID || result.TurnID == "" {
		return Submission{}, errors.New("invalid session start identity")
	}
	return Submission(result), nil
}
