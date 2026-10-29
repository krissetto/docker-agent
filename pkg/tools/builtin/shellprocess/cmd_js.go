//go:build js

package shellprocess

import (
	"errors"
	"os"
	"syscall"
)

type processGroup struct{}

func platformSpecificSysProcAttr() *syscall.SysProcAttr       { return nil }
func platformSpecificShellSysProcAttr() *syscall.SysProcAttr  { return nil }
func createProcessGroup(_ *os.Process) (*processGroup, error) { return &processGroup{}, nil }
func terminateProcess(_ *os.Process, _ *processGroup, _ bool) error {
	return errors.New("shellprocess: process termination not supported on js/wasm")
}
func terminateShellProcessGroup(proc *os.Process, pg *processGroup, force bool) error {
	return terminateProcess(proc, pg, force)
}
func shellBackgroundWaitSupported() bool { return false }
func observeShellLeaderExit(*os.Process) (<-chan shellProcessExit, func(), error) {
	return nil, func() {}, errors.New("safe shell process exit observation unavailable")
}
