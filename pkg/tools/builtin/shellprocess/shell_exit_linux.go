//go:build linux && !js

package shellprocess

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

func shellBackgroundWaitSupported() bool { return true }

// waitid(WNOWAIT) observes process exit without reaping the process-group leader.
func observeShellLeaderExit(proc *os.Process) (<-chan shellProcessExit, func(), error) {
	if proc == nil {
		return nil, func() {}, errors.New("shell process is nil")
	}
	pid := proc.Pid
	exited := make(chan shellProcessExit, 1)
	go func() {
		defer close(exited)
		var info [128]byte
		for {
			_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, uintptr(1), uintptr(pid), uintptr(unsafe.Pointer(&info[0])), uintptr(syscall.WEXITED|syscall.WNOWAIT), 0, 0)
			if errno == syscall.EINTR {
				continue
			}
			if errno != 0 {
				exited <- shellProcessExit{err: errno}
			} else {
				exited <- shellProcessExit{}
			}
			return
		}
	}()
	runtime.KeepAlive(proc)
	return exited, func() {}, nil
}
