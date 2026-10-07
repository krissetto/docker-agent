package runtime

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestSafetyCeilingAtomicWithStricterEdit(t *testing.T) {
	for range 10 {
		r := newPersistedSessionRuntime(t, session.NewInMemorySessionStore())
		h, err := r.CreateSession(t.Context(), session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous)), SessionBinding{AgentName: "root"})
		require.NoError(t, err)
		strict, ceiling := session.SafetyPolicyStrict, session.SafetyPolicyRestricted
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, edit := range []SessionEdit{{Kind: SessionEditPolicy, SafetyPolicy: &strict}, {Kind: SessionEditPolicy, SafetyCeiling: &ceiling}} {
			wg.Go(func() { _, err := h.Edit(t.Context(), edit); errs <- err })
		}
		wg.Wait()
		for range 2 {
			require.NoError(t, <-errs)
		}
		snapshot, err := h.Snapshot(t.Context())
		require.NoError(t, err)
		require.Equal(t, strict, snapshot.GetSafetyPolicy())
		// A lost acknowledgement followed by retry must not weaken authority.
		_, err = h.Edit(t.Context(), SessionEdit{Kind: SessionEditPolicy, SafetyCeiling: &ceiling})
		require.NoError(t, err)
		snapshot, err = h.Snapshot(t.Context())
		require.NoError(t, err)
		require.Equal(t, strict, snapshot.GetSafetyPolicy())
	}
}

func TestSafetyCeilingValidationPersistenceAndWire(t *testing.T) {
	store := &recoveryMetadataStore{Store: session.NewInMemorySessionStore()}
	r := newPersistedSessionRuntime(t, store)
	h, err := r.CreateSession(t.Context(), session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	strict := session.SafetyPolicyStrict
	store.failNext = true
	_, err = h.Edit(t.Context(), SessionEdit{Kind: SessionEditPolicy, SafetyCeiling: &strict})
	require.Error(t, err)
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, session.SafetyPolicyAutonomous, snapshot.GetSafetyPolicy())
	invalid := session.SafetyPolicy("invalid")
	for _, edit := range []SessionEdit{{Kind: SessionEditPolicy, SafetyCeiling: &invalid}, {Kind: SessionEditPolicy, SafetyCeiling: &strict, SafetyPolicy: &strict}, {Kind: SessionEditPolicy, SafetyCeiling: &strict, ToolsApproved: new(true)}} {
		_, err = h.Edit(t.Context(), edit)
		require.Error(t, err)
	}
	data, err := json.Marshal(SessionEdit{Kind: SessionEditPolicy, SafetyCeiling: &strict})
	require.NoError(t, err)
	var wire SessionEdit
	require.NoError(t, json.Unmarshal(data, &wire))
	require.Equal(t, &strict, wire.SafetyCeiling)
	_, err = h.Edit(t.Context(), wire)
	require.NoError(t, err)
	// Ordinary explicit policy edits retain their existing semantics.
	autonomous := session.SafetyPolicyAutonomous
	_, err = h.Edit(t.Context(), SessionEdit{Kind: SessionEditPolicy, SafetyPolicy: &autonomous})
	require.NoError(t, err)
	snapshot, err = h.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, autonomous, snapshot.GetSafetyPolicy())
}
