//go:build !windows && !js && !darwin && !linux

package shellprocess

import (
	"errors"
	"os"
)

func shellBackgroundWaitSupported() bool { return false }
func observeShellLeaderExit(*os.Process) (<-chan shellProcessExit, func(), error) {
	return nil, func() {}, errors.New("safe shell process exit observation unsupported on this platform")
}
