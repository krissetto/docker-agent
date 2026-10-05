//go:build !windows && !js

package backgroundjobs

import (
	"os"
	"syscall"
)

type processGroup struct{}

func platformSpecificSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func createProcessGroup(_ *os.Process) (*processGroup, error) {
	return &processGroup{}, nil
}

func terminateProcess(proc *os.Process, _ *processGroup, force bool) error {
	if force {
		return proc.Kill()
	}
	// Process.Signal coordinates with Wait; a bare group PID can be reused after reaping.
	return proc.Signal(syscall.SIGTERM)
}
