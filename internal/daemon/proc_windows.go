//go:build windows

package daemon

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachedAttr: no console, own process group, survives the parent console.
func detachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
		HideWindow:    true,
	}
}

// terminate: no SIGTERM on Windows; tokens are persisted atomically.
func terminate(p *os.Process) error { return p.Kill() }
