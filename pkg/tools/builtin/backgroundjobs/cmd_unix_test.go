//go:build !windows && !js

package backgroundjobs

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTerminateReapedProcessUsesProcessIdentity(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "exit 0")
	cmd.SysProcAttr = platformSpecificSysProcAttr()
	require.NoError(t, cmd.Start())
	group, err := createProcessGroup(cmd.Process)
	require.NoError(t, err)
	require.NoError(t, cmd.Wait())
	for _, force := range []bool{false, true} {
		err := terminateProcess(cmd.Process, group, force)
		require.ErrorIs(t, err, os.ErrProcessDone, "reaped process must never be signaled by numeric group ID")
	}
}
