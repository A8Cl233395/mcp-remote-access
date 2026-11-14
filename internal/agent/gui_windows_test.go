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

func TestGUILayoutAndStatusStyling(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	enableVisualStyles()
	hbrBg, _, _ = procCreateSolidBrush.Call(rgb(248, 250, 252))
	hbrStatIdle, _, _ = procCreateSolidBrush.Call(rgb(241, 245, 249))
	hbrStatOk, _, _ = procCreateSolidBrush.Call(rgb(240, 253, 244))
	hbrStatErr, _, _ = procCreateSolidBrush.Call(rgb(254, 242, 242))

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("MCPRemoteUserClientTestWnd")
	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     syscall.Handle(hInst),
		hbrBackground: syscall.Handle(hbrBg),
		lpszClassName: className,
	}
	_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	title, _ := syscall.UTF16PtrFromString("Test")
	h, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedDialog,
		0, 0, 440, 282,
		0, 0, hInst, 0,
	)
	if h == 0 {
		t.Fatal("failed to create test GUI window")
	}
	defer procDestroyWindowTest.Call(h)

	if hwndKeyEdit == 0 || hwndBtnConnect == 0 || hwndStatus == 0 || hwndStatusBox == 0 {
		t.Fatal("expected all GUI controls to be created in wmCreate")
	}

	latestStatus = "Status: Connected as test-pc"
	if br := wndProc(syscall.Handle(h), wmCtlColorStatic, 0, uintptr(hwndStatus)); br != hbrStatOk {
		t.Fatalf("expected connected status brush %x, got %x", hbrStatOk, br)
	}
	latestStatus = "Status: Error: auth failed"
	if br := wndProc(syscall.Handle(h), wmCtlColorStatic, 0, uintptr(hwndStatusBox)); br != hbrStatErr {
		t.Fatalf("expected error status brush %x, got %x", hbrStatErr, br)
	}
}
