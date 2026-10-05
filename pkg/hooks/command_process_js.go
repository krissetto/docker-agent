//go:build js

package hooks

import (
	"errors"
	"os"
	"syscall"
)

// hookProcessGroup is a no-op under js/wasm: there are no processes to track,
// because os/exec cannot actually spawn subprocesses in the browser.
type hookProcessGroup struct{}

func hookSysProcAttr() *syscall.SysProcAttr {
	return nil
}

func newHookProcessGroup(_ *os.Process) (*hookProcessGroup, error) {
	return &hookProcessGroup{}, nil
}

func (*hookProcessGroup) kill(_ *os.Process) error {
	return errors.New("shell: process termination not supported on js/wasm")
}

func (*hookProcessGroup) close() {}
