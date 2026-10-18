//go:build !windows && !js

package shell

import (
	"bufio"
	"io"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKillDetachedSessionTerminatesProcessGroup(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 60 & echo ready; wait")
	cmd.SysProcAttr = platformSpecificSysProcAttr()
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	reader := bufio.NewReader(output)
	ready, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", ready)

	// The child keeps stdout open: EOF before Wait proves it died with the shell.
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, reader)
		done <- err
	}()
	require.NoError(t, kill(cmd.Process, nil))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("process-group cancellation left a descendant holding stdout open")
	}
}
