//go:build !windows && !js

package hooks

import (
	"os"
	"syscall"
)

type hookProcessGroup struct {
	// Unix doesn't need to store handles, process group is managed by kernel
}

func hookSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid: true,
	}
}

func newHookProcessGroup(_ *os.Process) (*hookProcessGroup, error) {
	return &hookProcessGroup{}, nil
}

func (*hookProcessGroup) kill(proc *os.Process) error {
	// Wait may already have reaped the leader while draining inherited pipes.
	// os.Process guards its identity; a raw group PID could target a recycled group.
	// Descendants may survive cancellation, but WaitDelay bounds their pipe drain.
	return proc.Kill()
}

func (*hookProcessGroup) close() {}
