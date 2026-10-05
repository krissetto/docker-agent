package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/session"
)

func (d *sessionDriver) persistInstructionContext(ctx context.Context, generation uint64, state *session.InstructionContextState) error {
	return d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.generation != generation || d.settledGeneration >= generation {
			return sessionIOReservation{}, &SessionError{Kind: SessionErrorStale, Operation: "instruction_context"}
		}
		next := d.sess.OwnSnapshot()
		next.InstructionContext = state
		next = next.OwnSnapshot()
		var write func(context.Context) error
		if d.r.sessionStore != nil {
			write = func(ctx context.Context) error { return d.r.sessionStore.UpdateSession(ctx, next) }
		}
		return sessionIOReservation{write: write, commit: func(err error) error {
			if err != nil {
				return err
			}
			if d.generation != generation || d.settledGeneration >= generation {
				return &SessionError{Kind: SessionErrorStale, Operation: "instruction_context"}
			}
			d.sess.InstructionContext = next.InstructionContext
			return nil
		}}, nil
	})
}
