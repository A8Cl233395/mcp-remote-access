//go:build !windows

package agent

import (
	"io"
	"os/exec"
	"sync"
)

type WakeLock struct {
	mu      sync.Mutex
	active  bool
	cmd     *exec.Cmd
	stdinW  io.WriteCloser
}

func NewWakeLock() *WakeLock {
	return &WakeLock{}
}

func (w *WakeLock) Acquire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active {
		return
	}
	w.active = true
	inhibitPath, err := exec.LookPath("systemd-inhibit")
	if err != nil {
		return
	}
	// Run `cat` reading from a stdin pipe instead of `sleep 86400` so the wake lock
	// never expires after 24 hours and automatically terminates if user-client exits abruptly.
	cmd := exec.Command(
		inhibitPath,
		"--what=idle:sleep",
		"--who=mcp-user-client",
		"--why=Remote AI session active",
		"cat",
	)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err == nil {
		w.cmd = cmd
		w.stdinW = stdinPipe
	} else {
		_ = stdinPipe.Close()
	}
}

func (w *WakeLock) Release() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return
	}
	w.active = false
	if w.stdinW != nil {
		_ = w.stdinW.Close()
		w.stdinW = nil
	}
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
		_, _ = w.cmd.Process.Wait()
		w.cmd = nil
	}
}
