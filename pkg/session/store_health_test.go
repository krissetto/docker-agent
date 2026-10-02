package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorePingConnectivityAndCancellation(t *testing.T) {
	sqlite := openMemoryStore(t)
	memory := NewInMemorySessionStore().(StorePinger)
	for _, probe := range []StorePinger{sqlite, memory} {
		require.NoError(t, probe.Ping(t.Context()))
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, probe.Ping(canceled), context.Canceled)
	}
	require.NoError(t, sqlite.Close())
	require.Error(t, sqlite.Ping(t.Context()))
}
