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

func platformSpecificShellSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
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

func terminateShellProcessGroup(proc *os.Process, _ *processGroup, force bool) error {
	if proc == nil {
		return os.ErrProcessDone
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(-proc.Pid, signal)
}
