//go:build windows

package client

import "syscall"

// winInhibitor uses SetThreadExecutionState to prevent idle sleep.
// https://learn.microsoft.com/windows/win32/api/winbase/nf-winbase-setthreadexecutionstate
type winInhibitor struct {
	active bool
}

func newSleepInhibitor() sleepInhibitor {
	return &winInhibitor{}
}

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	setThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
)

const (
	esContinuous      = 0x80000000
	esSystemRequired  = 0x00000001
	esDisplayRequired = 0x00000002
)

func (i *winInhibitor) inhibit() error {
	if i.active {
		return nil
	}
	r, _, err := setThreadExecutionState.Call(uintptr(esContinuous | esSystemRequired | esDisplayRequired))
	if r == 0 {
		return err
	}
	i.active = true
	return nil
}

func (i *winInhibitor) release() error {
	if !i.active {
		return nil
	}
	_, _, err := setThreadExecutionState.Call(uintptr(esContinuous))
	i.active = false
	if err != nil {
		return err
	}
	return nil
}
