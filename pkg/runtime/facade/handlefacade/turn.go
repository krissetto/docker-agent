package handlefacade

import (
	"context"

	"github.com/docker/docker-agent/pkg/runtime/facade"
)

// turn is an admitted turn. Its stream and settlement come from the session feed.
type turn struct {
	sess *sessionImpl
	st   *turnState
}

func (t *turn) ID() string { return t.st.id }

func (t *turn) Events() <-chan facade.Event { return t.st.box.out }

func (t *turn) Cancel(ctx context.Context) (facade.CancelOutcome, error) {
	result, err := t.sess.handle.Cancel(ctx, t.st.id)
	if err != nil {
		return "", err
	}
	return facade.CancelOutcome(result.Outcome), nil
}

func (t *turn) Wait(ctx context.Context) (facade.Outcome, error) {
	select {
	case <-t.st.settled:
		return t.st.outcome, t.st.err
	case <-ctx.Done():
		return facade.Outcome{}, ctx.Err()
	}
}
