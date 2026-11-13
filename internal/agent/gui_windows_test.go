//go:build windows

package agent

import (
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

var (
	procCreateWindowExTest = user32.NewProc("CreateWindowExW")
	procDestroyWindowTest  = user32.NewProc("DestroyWindow")
)

func TestUpdateGUIStatusNonBlocking(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	className, _ := syscall.UTF16PtrFromString("STATIC")
	title, _ := syscall.UTF16PtrFromString("test")
	h, _, _ := procCreateWindowExTest.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		0,
		0, 0, 10, 10,
		0, 0, 0, 0,
	)
	if h == 0 {
		t.Fatal("CreateWindowExW failed")
	}
	defer func() {
		guiMu.Lock()
		hwndMain = 0
		guiMu.Unlock()
		_, _, _ = procDestroyWindowTest.Call(h)
	}()

	guiMu.Lock()
	hwndMain = syscall.Handle(h)
	guiMu.Unlock()

	// UI thread intentionally does not pump messages; PostMessageW must return immediately without blocking.
	done := make(chan struct{})
	go func() {
		updateGUIStatus("Status: testing async")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("updateGUIStatus blocked when UI thread was not pumping messages")
	}
}
