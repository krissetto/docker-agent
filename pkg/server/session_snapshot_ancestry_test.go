package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionSnapshotEncodesCanonicalParent(t *testing.T) {
	child := session.New(session.WithID("child"), session.WithParentID("parent"))
	encoded, err := json.Marshal(sessionSnapshot(runtime.SessionSnapshot{Session: child}))
	require.NoError(t, err)
	var snapshot struct {
		ParentSessionID string `json:"parent_session_id"`
	}
	require.NoError(t, json.Unmarshal(encoded, &snapshot))
	require.Equal(t, "parent", snapshot.ParentSessionID)
}
