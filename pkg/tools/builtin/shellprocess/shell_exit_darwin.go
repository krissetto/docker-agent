//go:build darwin && !js

package shellprocess

import (
	"errors"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

func shellBackgroundWaitSupported() bool { return true }

func observeShellLeaderExit(proc *os.Process) (<-chan shellProcessExit, func(), error) {
	if proc == nil {
		return nil, func() {}, errors.New("shell process is nil")
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, func() {}, err
	}
	change := unix.Kevent_t{Ident: uint64(proc.Pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err = unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		_ = unix.Close(kq)
		if errors.Is(err, unix.ESRCH) {
			exited := make(chan shellProcessExit, 1)
			exited <- shellProcessExit{}
			close(exited)
			return exited, func() {}, nil
		}
		return nil, func() {}, err
	}
	exited := make(chan shellProcessExit, 1)
	var closeOnce sync.Once
	closeObserver := func() { closeOnce.Do(func() { _ = unix.Close(kq) }) }
	go func() {
		events := make([]unix.Kevent_t, 1)
		for {
			_, err := unix.Kevent(kq, nil, events, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			exited <- shellProcessExit{err: err}
			close(exited)
			closeObserver()
			return
		}
	}()
	return exited, closeObserver, nil
}
