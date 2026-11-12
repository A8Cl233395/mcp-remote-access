//go:build !windows

package agent

import (
	"os/exec"
	"sync"
)

type WakeLock struct {
	mu     sync.Mutex
	active bool
	cmd    *exec.Cmd
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
	cmd := exec.Command(
		inhibitPath,
		"--what=idle:sleep",
		"--who=mcp-user-client",
		"--why=Remote AI session active",
		"sleep", "86400",
	)
	if err := cmd.Start(); err == nil {
		w.cmd = cmd
	}
}

func (w *WakeLock) Release() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return
	}
	w.active = false
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
		_, _ = w.cmd.Process.Wait()
		w.cmd = nil
	}
}
