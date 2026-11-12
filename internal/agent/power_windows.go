//go:build windows

package agent

import (
	"runtime"
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
	doneCh chan struct{}
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
	doneCh := make(chan struct{})
	w.stopCh = stopCh
	w.doneCh = doneCh

	go func() {
		// SetThreadExecutionState is per-OS-thread; lock this goroutine to its OS thread
		// so the state persists and is cleared on the exact same thread upon Release().
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(doneCh)

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
	if !w.active {
		w.mu.Unlock()
		return
	}
	w.active = false
	stopCh := w.stopCh
	doneCh := w.doneCh
	w.stopCh = nil
	w.doneCh = nil
	w.mu.Unlock()

	close(stopCh)
	if doneCh != nil {
		<-doneCh
	}
}
