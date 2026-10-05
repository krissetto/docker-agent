package hooks

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type hookProcessGroup struct {
	jobHandle     windows.Handle
	processHandle windows.Handle
}

func hookSysProcAttr() *syscall.SysProcAttr {
	return nil
}

func newHookProcessGroup(proc *os.Process) (*hookProcessGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), //nolint:gosec // Windows API requires unsafe.Pointer
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}

	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(proc.Pid)) //nolint:gosec // proc.Pid fits in uint32 on Windows
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}

	if err := windows.AssignProcessToJobObject(job, handle); err != nil {
		_ = windows.CloseHandle(handle)
		_ = windows.CloseHandle(job)
		return nil, err
	}

	return &hookProcessGroup{
		jobHandle:     job,
		processHandle: handle,
	}, nil
}

func (pg *hookProcessGroup) close() {
	if pg.processHandle != 0 {
		_ = windows.CloseHandle(pg.processHandle)
		pg.processHandle = 0
	}
	if pg.jobHandle != 0 {
		_ = windows.CloseHandle(pg.jobHandle)
		pg.jobHandle = 0
	}
}

func (pg *hookProcessGroup) kill(proc *os.Process) error {
	pg.close()
	return proc.Kill()
}
