//go:build !windows

package angel

import (
	"os"
	"syscall"
)

// detachedAttr: new session, detached from the controlling terminal.
func detachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// terminate sends SIGTERM for graceful shutdown.
func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
