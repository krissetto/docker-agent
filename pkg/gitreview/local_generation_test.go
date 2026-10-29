package gitreview

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewLocalIsLazyUntilFirstPublish(t *testing.T) {
	local, err := NewLocal(t.TempDir())
	require.NoError(t, err)
	require.Zero(t, local.CurrentSnapshot().Generation, "constructing a Local publishes nothing")

	snap, err := local.BrowseRoot(t.Context())
	require.NoError(t, err)
	require.NotZero(t, snap.Generation)
}

func TestLocalGenerationsStayUniqueAcrossRecreatedProviders(t *testing.T) {
	workspace := t.TempDir()
	first, err := NewLocal(workspace)
	require.NoError(t, err)
	old, err := first.Snapshot(t.Context())
	require.NoError(t, err)
	again, err := first.Snapshot(t.Context())
	require.NoError(t, err)
	require.Greater(t, again.Generation, old.Generation, "each publish advances the generation")

	recreated, err := NewLocal(workspace)
	require.NoError(t, err)
	fresh, err := recreated.Snapshot(t.Context())
	require.NoError(t, err)
	require.Greater(t, fresh.Generation, again.Generation,
		"a recreated provider must not reuse a generation an old client may still hold")
	require.NotEqual(t, old.Generation, fresh.Generation)
}
