package profiling

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPprofServerLifecycle(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	// Binding errors are synchronous, before any serving goroutines start.
	require.ErrorContains(t, StartPprofServer(t.Context(), addr), "pprof: listen on "+addr)
	require.NoError(t, ln.Close())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, StartPprofServer(ctx, addr))
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + addr + "/debug/pprof/")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "/debug/pprof/")

	cancel()
	require.Eventually(t, func() bool {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return false
		}
		ln.Close()
		return true
	}, 3*time.Second, 10*time.Millisecond)
}
