//go:build !windows

// Package proc holds what the angel needs from the operating system: how to detach a
// child from its parent, how to ask a process to end, and the Windows job object
// the server lives in.
package proc

import (
	"os"
	"syscall"
)

// DetachedAttr: new session, detached from the controlling terminal.
func DetachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// Terminate sends SIGTERM for graceful shutdown.
func Terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
