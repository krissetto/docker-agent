//go:build !windows && !js

package environment

import "syscall"

func secretCommandSysProcAttr() *syscall.SysProcAttr {
	// Credential prompts must not bypass captured IO through the TUI's /dev/tty.
	return &syscall.SysProcAttr{Setsid: true}
}
