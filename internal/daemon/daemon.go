// Package daemon runs `ccgp serve` as a detached background process and
// controls it via /health (which reports the server pid).
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	serviceName = "ccgp"
	maxLogSize  = 10 << 20
)

// Health is the /health payload.
type Health struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// BaseURL is http://host:port.
func BaseURL(host, port string) string { return "http://" + host + ":" + port }

// Check returns health of a ccgp server at base, or an error if none answers.
func Check(base string) (*Health, error) {
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Get(base + "/health")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var h Health
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&h) != nil || h.Service != serviceName {
		return nil, fmt.Errorf("%s is not a ccgp server", base)
	}
	return &h, nil
}

// LogPath is <UserCacheDir>/cc-go-proxy/logs/ccgp.log (%LOCALAPPDATA% on Windows).
func LogPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cc-go-proxy", "logs", "ccgp.log"), nil
}

// openLog opens the log for append, rotating to .1 when over maxLogSize.
func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogSize {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// Start spawns `exe args...` detached with output to logPath, then waits until
// base reports healthy.
func Start(exe string, args []string, logPath, base string, timeout time.Duration) (*Health, error) {
	log, err := openLog(logPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = log.Close() }()

	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = log, log, nil
	cmd.SysProcAttr = detachedAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h, err := Check(base); err == nil {
			return h, nil
		}
		select {
		case err := <-exited:
			// Lost a start race to another ccgp: fine if one is up now.
			if h, cerr := Check(base); cerr == nil {
				return h, nil
			}
			return nil, fmt.Errorf("server exited (%v), see %s", err, logPath)
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("server not healthy after %s, see %s", timeout, logPath)
}

// Stop terminates the ccgp server at base, identified by the pid it reports.
func Stop(base string, timeout time.Duration) error {
	h, err := Check(base)
	if err != nil {
		return ErrNotRunning
	}
	p, err := os.FindProcess(h.PID)
	if err != nil {
		return err
	}
	if err := terminate(p); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := Check(base); err != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("pid %d still answering after %s", h.PID, timeout)
}

// ErrNotRunning: no ccgp server answers.
var ErrNotRunning = errors.New("ccgp is not running")
