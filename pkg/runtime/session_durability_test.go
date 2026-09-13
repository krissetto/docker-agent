package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type legacyDurableSessionStore struct {
	session.Store

	tree subagent.Store
}

func (*legacyDurableSessionStore) Durability() subagent.Durability { return subagent.DurabilityDurable }

func TestSessionDurabilityRequiresCoordinationAndIdempotentOutput(t *testing.T) {
	native := coordinationSQLite(t)
	legacy := &legacyDurableSessionStore{Store: native, tree: native.(subagent.Store)}
	rt, owner := coordinationRuntime(t, legacy, coordinationReply("root"), coordinationReply("worker"))
	assert.Equal(t, subagent.DurabilityVolatile, rt.sessionDurability())
	_, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("durable-denied")), SessionBinding{AgentName: "root", Durability: subagent.DurabilityDurable})
	var denied *SessionError
	require.ErrorAs(t, err, &denied)
	assert.Equal(t, SessionErrorUnsupported, denied.Kind)
	h := coordinationCreate(t, owner.Runtime(), "volatile", "")
	assert.Equal(t, subagent.DurabilityVolatile, h.Metadata().Capabilities.Durability)
	direct := &LocalRuntime{sessionStore: native}
	assert.Equal(t, subagent.DurabilityDurable, direct.sessionDurability())
}

func (s *legacyDurableSessionStore) SaveTree(ctx context.Context, id string, snapshot subagent.Snapshot) error {
	return s.tree.SaveTree(ctx, id, snapshot)
}

func (s *legacyDurableSessionStore) LoadTree(ctx context.Context, id string) (*subagent.Snapshot, error) {
	return s.tree.LoadTree(ctx, id)
}
