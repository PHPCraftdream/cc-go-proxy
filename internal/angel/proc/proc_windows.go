//go:build windows

package proc

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// DetachedAttr: no console, own process group, survives the parent console.
func DetachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
		HideWindow:    true,
	}
}

// Terminate: no SIGTERM on Windows; tokens are persisted atomically.
func Terminate(p *os.Process) error { return p.Kill() }
