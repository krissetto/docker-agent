//go:build windows || js

package environment

import "syscall"

func secretCommandSysProcAttr() *syscall.SysProcAttr {
	return nil
}
