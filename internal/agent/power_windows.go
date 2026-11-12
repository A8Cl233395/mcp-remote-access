//go:build windows

package agent

import (
	"sync"
	"syscall"
	"time"
)

const (
	esContinuous     uintptr = 0x80000000
	esSystemRequired uintptr = 0x00000001
)

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procSetThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
)

type WakeLock struct {
	mu     sync.Mutex
	active bool
	stopCh chan struct{}
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
	stopCh := make(chan struct{})
	w.stopCh = stopCh

	go func() {
		_, _, _ = procSetThreadExecutionState.Call(esContinuous | esSystemRequired)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_, _, _ = procSetThreadExecutionState.Call(esContinuous | esSystemRequired)
			case <-stopCh:
				_, _, _ = procSetThreadExecutionState.Call(esContinuous)
				return
			}
		}
	}()
}

func (w *WakeLock) Release() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return
	}
	w.active = false
	close(w.stopCh)
	w.stopCh = nil
}
