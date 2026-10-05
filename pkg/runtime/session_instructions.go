package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/session"
)

func (r *LocalRuntime) commitInstructionState(ctx context.Context, scratch *session.Session, state *session.InstructionContextState) error {
	identity, canonical := ctx.Value(executionIdentityKey{}).(executionIdentity)
	if !canonical {
		scratch.InstructionContext = state
		return nil
	}
	err := identity.driver.persistInstructionContext(ctx, identity.generation, state)
	if err == nil {
		scratch.InstructionContext = state
	}
	return err
}
