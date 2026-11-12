//go:build windows

package agent

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	gdi32                = syscall.NewLazyDLL("gdi32.dll")
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procShowWindow       = user32.NewProc("ShowWindow")
	procUpdateWindow     = user32.NewProc("UpdateWindow")
	procSetWindowTextW   = user32.NewProc("SetWindowTextW")
	procGetWindowTextW   = user32.NewProc("GetWindowTextW")
	procGetWindowTextLen = user32.NewProc("GetWindowTextLengthW")
	procSendMessageW     = user32.NewProc("SendMessageW")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procAttachConsole    = kernel32.NewProc("AttachConsole")
	procGetStockObject   = gdi32.NewProc("GetStockObject")
)

const (
	attachParentProcess = ^uintptr(0) // (DWORD)-1
	swHide              = 0
)

// EnsureCLIConsole attaches to the parent terminal console if the binary was compiled
// with -H=windowsgui and launched with -cli from an interactive shell.
func EnsureCLIConsole() {
	r, _, _ := procAttachConsole.Call(attachParentProcess)
	if r != 0 {
		if f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
			os.Stdout = f
			os.Stderr = f
		}
	}
}

func hideConsoleWindow() {
	hConsole, _, _ := procGetConsoleWindow.Call()
	if hConsole != 0 {
		_, _, _ = procShowWindow.Call(hConsole, swHide)
	}
}

const (
	wsOverlappedWindow = 0x00000000 | 0x00C00000 | 0x00080000 | 0x00020000
	wsVisible          = 0x10000000
	wsChild            = 0x40000000
	wsBorder           = 0x00800000
	wsTabStop          = 0x00010000
	esAutoHScroll      = 0x0080
	bsPushButton       = 0x00000000

	wmCreate  = 0x0001
	wmDestroy = 0x0002
	wmCommand = 0x0111
	wmSetFont = 0x0030
	wmAppStat = 0x8001

	swShow          = 5
	colorBtnFace    = 15
	defaultGuiFont  = 17
	idEditServer    = 101
	idEditKey       = 102
	idBtnConnect    = 103
	idStaticStatus  = 104
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance      syscall.Handle
	hIcon         syscall.Handle
	hCursor       syscall.Handle
	hbrBackground syscall.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       syscall.Handle
}

type point struct {
	x, y int32
}

type msg struct {
	hwnd    syscall.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

var (
	guiMu          sync.Mutex
	hwndMain       syscall.Handle
	hwndServerEdit syscall.Handle
	hwndKeyEdit    syscall.Handle
	hwndBtnConnect syscall.Handle
	hwndStatus     syscall.Handle
	latestStatus   string
	guiCancel      context.CancelFunc
	guiBaseOpts    ClientOptions
)

func setWindowText(hwnd syscall.Handle, text string) {
	if hwnd == 0 {
		return
	}
	ptr, _ := syscall.UTF16PtrFromString(text)
	_, _, _ = procSetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(ptr)))
}

func getWindowText(hwnd syscall.Handle) string {
	length, _, _ := procGetWindowTextLen.Call(uintptr(hwnd))
	if length == 0 {
		return ""
	}
	buf := make([]uint16, length+1)
	_, _, _ = procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), length+1)
	return syscall.UTF16ToString(buf)
}

func startOrRestartAgent(userKey string) {
	guiMu.Lock()
	if guiCancel != nil {
		guiCancel()
		guiCancel = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	guiCancel = cancel
	opts := guiBaseOpts
	guiMu.Unlock()

	userKey = strings.TrimSpace(userKey)
	if userKey == "" {
		updateGUIStatus("Status: Please enter Connection Key.")
		return
	}

	pCfg := LoadPersistentConfig()
	pCfg.UserKey = userKey
	if opts.MachineID != "" {
		pCfg.MachineID = opts.MachineID
	}
	_ = SavePersistentConfig(pCfg)

	opts.UserKey = userKey
	if opts.MachineID == "" {
		opts.MachineID = pCfg.MachineID
	}
	opts.OnStatus = func(st string) {
		updateGUIStatus("Status: " + st)
	}

	client := NewClient(opts)
	go func() {
		_ = client.Run(ctx)
	}()
}

func updateGUIStatus(st string) {
	guiMu.Lock()
	latestStatus = st
	h := hwndMain
	guiMu.Unlock()
	if h != 0 {
		_, _, _ = procSendMessageW.Call(uintptr(h), wmAppStat, 0, 0)
	}
}

func wndProc(hwnd syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmCreate:
		hFont, _, _ := procGetStockObject.Call(defaultGuiFont)
		createCtrl := func(class, text string, style uint32, x, y, w, h int, id uintptr) syscall.Handle {
			cPtr, _ := syscall.UTF16PtrFromString(class)
			tPtr, _ := syscall.UTF16PtrFromString(text)
			hCtrl, _, _ := procCreateWindowExW.Call(
				0,
				uintptr(unsafe.Pointer(cPtr)),
				uintptr(unsafe.Pointer(tPtr)),
				uintptr(style),
				uintptr(x), uintptr(y), uintptr(w), uintptr(h),
				uintptr(hwnd),
				id,
				0, 0,
			)
			_, _, _ = procSendMessageW.Call(hCtrl, wmSetFont, hFont, 1)
			return syscall.Handle(hCtrl)
		}

		createCtrl("STATIC", "Connection Key:", wsChild|wsVisible, 16, 22, 95, 22, 0)
		hwndKeyEdit = createCtrl("EDIT", guiBaseOpts.UserKey, wsChild|wsVisible|wsBorder|wsTabStop|esAutoHScroll, 115, 19, 285, 24, idEditKey)

		hwndBtnConnect = createCtrl("BUTTON", "Connect / Save", wsChild|wsVisible|wsTabStop|bsPushButton, 115, 56, 140, 30, idBtnConnect)
		hwndStatus = createCtrl("STATIC", "Status: Idle", wsChild|wsVisible, 16, 100, 385, 44, idStaticStatus)
		return 0

	case wmCommand:
		id := wParam & 0xFFFF
		if id == idBtnConnect {
			key := getWindowText(hwndKeyEdit)
			startOrRestartAgent(key)
			return 0
		}

	case wmAppStat:
		guiMu.Lock()
		st := latestStatus
		guiMu.Unlock()
		setWindowText(hwndStatus, st)
		return 0

	case wmDestroy:
		guiMu.Lock()
		if guiCancel != nil {
			guiCancel()
		}
		guiMu.Unlock()
		_, _, _ = procPostQuitMessage.Call(0)
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return ret
}

func RunGUI(opts ClientOptions) error {
	hideConsoleWindow()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	guiBaseOpts = opts

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("MCPRemoteUserClientWnd")
	title, _ := syscall.UTF16PtrFromString("MCP Remote Access - User Client")

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     syscall.Handle(hInst),
		hbrBackground: syscall.Handle(colorBtnFace + 1),
		lpszClassName: className,
	}
	_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	h, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow|wsVisible,
		250, 250, 435, 190,
		0, 0, hInst, 0,
	)
	hwndMain = syscall.Handle(h)
	_, _, _ = procShowWindow.Call(h, swShow)
	_, _, _ = procUpdateWindow.Call(h)

	if strings.TrimSpace(opts.UserKey) != "" {
		startOrRestartAgent(opts.UserKey)
	} else {
		updateGUIStatus("Status: Enter Connection Key and click Connect.")
	}

	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	return nil
}
