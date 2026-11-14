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
	procPostMessageW     = user32.NewProc("PostMessageW")
	procMessageBoxW      = user32.NewProc("MessageBoxW")
	procIsDialogMessageW = user32.NewProc("IsDialogMessageW")
	procSetFocus         = user32.NewProc("SetFocus")
	procLoadCursorW      = user32.NewProc("LoadCursorW")
	procLoadIconW        = user32.NewProc("LoadIconW")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procInvalidateRect   = user32.NewProc("InvalidateRect")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procAttachConsole    = kernel32.NewProc("AttachConsole")
	procCreateActCtxW    = kernel32.NewProc("CreateActCtxW")
	procActivateActCtx   = kernel32.NewProc("ActivateActCtx")
	procGetStockObject   = gdi32.NewProc("GetStockObject")
	procCreateFontW      = gdi32.NewProc("CreateFontW")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	procSetBkColor       = gdi32.NewProc("SetBkColor")
	procSetTextColor     = gdi32.NewProc("SetTextColor")
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
	wsOverlappedDialog = 0x00000000 | 0x00C00000 | 0x00080000 | 0x00020000
	wsVisible          = 0x10000000
	wsChild            = 0x40000000
	wsBorder           = 0x00800000
	wsTabStop          = 0x00010000
	wsExClientEdge     = 0x00000200
	esAutoHScroll      = 0x0080
	bsDefPushButton    = 0x00000001
	bsAutoCheckbox     = 0x00000003
	ssEditControl      = 0x00002000

	bmGetCheck   = 0x00F0
	bmSetCheck   = 0x00F1
	bstUnchecked = 0
	bstChecked   = 1
	emSetMargins = 0x00D3

	wmCreate         = 0x0001
	wmDestroy        = 0x0002
	wmCommand        = 0x0111
	wmSetFont        = 0x0030
	wmCtlColorStatic = 0x0138
	wmAppStat        = 0x8001
	wmAppError       = 0x8002

	mbOK            = 0x00000000
	mbIconError     = 0x00000010
	mbSetForeground = 0x00010000

	swShow         = 5
	smCxScreen     = 0
	smCyScreen     = 1
	idcArrow       = 32512
	idiApplication = 32512
	defaultGuiFont = 17
	idEditKey      = 102
	idBtnConnect   = 103
	idStaticStatus = 104
	idCheckSaveFile = 105
)

type actCtxW struct {
	cbSize                 uint32
	dwFlags                uint32
	lpSource               *uint16
	wProcessorArchitecture uint16
	wLangId                uint16
	lpAssemblyDirectory    *uint16
	lpResourceName         uintptr
	lpApplicationName      *uint16
	hModule                syscall.Handle
}

func enableVisualStyles() {
	src, _ := syscall.UTF16PtrFromString("shell32.dll")
	ctx := actCtxW{
		cbSize:         uint32(unsafe.Sizeof(actCtxW{})),
		dwFlags:        0x008, // ACTCTX_FLAG_RESOURCE_NAME_VALID
		lpSource:       src,
		lpResourceName: 124,
	}
	hCtx, _, _ := procCreateActCtxW.Call(uintptr(unsafe.Pointer(&ctx)))
	if hCtx != 0 && hCtx != ^uintptr(0) {
		var cookie uintptr
		_, _, _ = procActivateActCtx.Call(hCtx, uintptr(unsafe.Pointer(&cookie)))
	}
}

func rgb(r, g, b uint32) uintptr {
	return uintptr(r | (g << 8) | (b << 16))
}

func createUIFont(height, weight int) uintptr {
	face, _ := syscall.UTF16PtrFromString("Segoe UI")
	h, _, _ := procCreateFontW.Call(
		uintptr(height), 0, 0, 0, uintptr(weight),
		0, 0, 0, 1, 0, 0, 5, 0, // DEFAULT_CHARSET=1, CLEARTYPE_QUALITY=5
		uintptr(unsafe.Pointer(face)),
	)
	if h == 0 {
		h, _, _ = procGetStockObject.Call(defaultGuiFont)
	}
	return h
}

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
	guiMu           sync.Mutex
	hwndMain        syscall.Handle
	hwndSubTitle    syscall.Handle
	hwndKeyEdit     syscall.Handle
	hwndSaveFileChk syscall.Handle
	hwndBtnConnect  syscall.Handle
	hwndStatusBox   syscall.Handle
	hwndStatus      syscall.Handle
	hbrBg           uintptr
	hbrStatIdle     uintptr
	hbrStatOk       uintptr
	hbrStatErr      uintptr
	latestStatus    string
	latestError     string
	guiRunSeq       uint64
	guiCancel       context.CancelFunc
	guiClient       *Client
	guiBaseOpts     ClientOptions
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

func startOrRestartAgent(userKey string, saveFile bool) {
	userKey = strings.TrimSpace(userKey)
	if userKey == "" {
		guiMu.Lock()
		if guiCancel != nil {
			guiCancel()
			guiCancel = nil
		}
		guiMu.Unlock()
		updateGUIStatus("Status: Please enter Connection Key.")
		return
	}

	pCfg := LoadPersistentConfig()
	if saveFile {
		pCfg.UserKey = userKey
		if guiBaseOpts.MachineID != "" {
			pCfg.MachineID = guiBaseOpts.MachineID
		}
		_ = SavePersistentConfig(pCfg)
	} else {
		_ = RemoveConfigFile()
	}

	guiMu.Lock()
	if guiCancel != nil {
		guiCancel()
		guiCancel = nil
	}
	guiRunSeq++
	mySeq := guiRunSeq
	latestError = ""
	ctx, cancel := context.WithCancel(context.Background())
	guiCancel = cancel

	if guiClient == nil {
		opts := guiBaseOpts
		opts.UserKey = userKey
		if opts.MachineID == "" {
			opts.MachineID = pCfg.MachineID
		}
		opts.OnStatus = func(st string) {
			updateGUIStatus("Status: " + st)
		}
		guiClient = NewClient(opts)
	} else {
		guiClient.SetUserKey(userKey)
	}
	client := guiClient
	guiMu.Unlock()

	go func(seq uint64, runCtx context.Context) {
		err := client.Run(runCtx)
		if err != nil && runCtx.Err() == nil {
			showGUIError(seq, err.Error())
		}
	}(mySeq, ctx)
}

func updateGUIStatus(st string) {
	guiMu.Lock()
	latestStatus = st
	h := hwndMain
	guiMu.Unlock()
	if h != 0 {
		_, _, _ = procPostMessageW.Call(uintptr(h), wmAppStat, 0, 0)
	}
}

func showGUIError(seq uint64, errMsg string) {
	guiMu.Lock()
	if seq != guiRunSeq {
		guiMu.Unlock()
		return
	}
	latestStatus = "Status: Error: " + errMsg
	latestError = errMsg
	h := hwndMain
	guiMu.Unlock()
	if h != 0 {
		_, _, _ = procPostMessageW.Call(uintptr(h), wmAppStat, 0, 0)
		_, _, _ = procPostMessageW.Call(uintptr(h), wmAppError, 0, 0)
	}
}

func wndProc(hwnd syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmCreate:
		hFontTitle := createUIFont(-19, 700)
		hFontSub := createUIFont(-13, 400)
		hFontUI := createUIFont(-15, 400)
		hFontBtn := createUIFont(-15, 600)

		createCtrl := func(exStyle uint32, class, text string, style uint32, x, y, w, h int, id uintptr, font uintptr) syscall.Handle {
			cPtr, _ := syscall.UTF16PtrFromString(class)
			tPtr, _ := syscall.UTF16PtrFromString(text)
			hCtrl, _, _ := procCreateWindowExW.Call(
				uintptr(exStyle),
				uintptr(unsafe.Pointer(cPtr)),
				uintptr(unsafe.Pointer(tPtr)),
				uintptr(style),
				uintptr(x), uintptr(y), uintptr(w), uintptr(h),
				uintptr(hwnd),
				id,
				0, 0,
			)
			_, _, _ = procSendMessageW.Call(hCtrl, wmSetFont, font, 1)
			return syscall.Handle(hCtrl)
		}

		subText := "Remote Agent Control Panel"
		if srv := strings.TrimSpace(guiBaseOpts.ServerAddr); srv != "" {
			subText = "Relay Server: " + srv
		}

		createCtrl(0, "STATIC", "MCP Remote Access", wsChild|wsVisible, 22, 16, 380, 26, 0, hFontTitle)
		hwndSubTitle = createCtrl(0, "STATIC", subText, wsChild|wsVisible, 22, 42, 380, 18, 0, hFontSub)

		createCtrl(0, "STATIC", "Connection Key", wsChild|wsVisible, 22, 70, 380, 20, 0, hFontUI)
		hwndKeyEdit = createCtrl(wsExClientEdge, "EDIT", guiBaseOpts.UserKey, wsChild|wsVisible|wsTabStop|esAutoHScroll, 22, 92, 380, 28, idEditKey, hFontUI)
		_, _, _ = procSendMessageW.Call(uintptr(hwndKeyEdit), emSetMargins, 3, (6<<16)|6)

		hwndSaveFileChk = createCtrl(0, "BUTTON", "Save Login State", wsChild|wsVisible|wsTabStop|bsAutoCheckbox, 22, 134, 180, 24, idCheckSaveFile, hFontUI)
		initCheck := bstUnchecked
		if HasConfigFile() {
			initCheck = bstChecked
		}
		_, _, _ = procSendMessageW.Call(uintptr(hwndSaveFileChk), bmSetCheck, uintptr(initCheck), 0)

		hwndBtnConnect = createCtrl(0, "BUTTON", "Connect", wsChild|wsVisible|wsTabStop|bsDefPushButton, 282, 130, 120, 32, idBtnConnect, hFontBtn)
		hwndStatusBox = createCtrl(0, "STATIC", "", wsChild|wsVisible|wsBorder, 22, 174, 380, 52, 0, hFontUI)
		hwndStatus = createCtrl(0, "STATIC", "Status: Idle", wsChild|wsVisible|ssEditControl, 32, 182, 360, 36, idStaticStatus, hFontUI)
		_, _, _ = procSetFocus.Call(uintptr(hwndKeyEdit))
		return 0

	case wmCtlColorStatic:
		hdc := wParam
		ctrl := syscall.Handle(lParam)
		if (ctrl == hwndStatus || ctrl == hwndStatusBox) && hbrStatIdle != 0 {
			guiMu.Lock()
			st := latestStatus
			guiMu.Unlock()
			switch {
			case strings.Contains(st, "Error") || strings.Contains(st, "Please enter"):
				_, _, _ = procSetTextColor.Call(hdc, rgb(153, 27, 27))
				_, _, _ = procSetBkColor.Call(hdc, rgb(254, 242, 242))
				return hbrStatErr
			case strings.Contains(st, "Connected"):
				_, _, _ = procSetTextColor.Call(hdc, rgb(22, 101, 52))
				_, _, _ = procSetBkColor.Call(hdc, rgb(240, 253, 244))
				return hbrStatOk
			default:
				_, _, _ = procSetTextColor.Call(hdc, rgb(51, 65, 85))
				_, _, _ = procSetBkColor.Call(hdc, rgb(241, 245, 249))
				return hbrStatIdle
			}
		}
		if hbrBg != 0 {
			if ctrl == hwndSubTitle {
				_, _, _ = procSetTextColor.Call(hdc, rgb(100, 116, 139))
			} else {
				_, _, _ = procSetTextColor.Call(hdc, rgb(15, 23, 42))
			}
			_, _, _ = procSetBkColor.Call(hdc, rgb(248, 250, 252))
			return hbrBg
		}

	case wmCommand:
		id := wParam & 0xFFFF
		if id == idBtnConnect {
			key := getWindowText(hwndKeyEdit)
			checkState, _, _ := procSendMessageW.Call(uintptr(hwndSaveFileChk), bmGetCheck, 0, 0)
			saveFile := checkState == bstChecked
			startOrRestartAgent(key, saveFile)
			return 0
		}

	case wmAppStat:
		guiMu.Lock()
		st := latestStatus
		guiMu.Unlock()
		setWindowText(hwndStatus, st)
		if hwndStatusBox != 0 {
			_, _, _ = procInvalidateRect.Call(uintptr(hwndStatusBox), 0, 1)
		}
		if hwndStatus != 0 {
			_, _, _ = procInvalidateRect.Call(uintptr(hwndStatus), 0, 1)
		}
		return 0

	case wmAppError:
		guiMu.Lock()
		errText := latestError
		latestError = ""
		guiMu.Unlock()
		if errText != "" {
			titlePtr, _ := syscall.UTF16PtrFromString("MCP Remote Access - Connection Error")
			msgPtr, _ := syscall.UTF16PtrFromString(errText)
			_, _, _ = procMessageBoxW.Call(
				uintptr(hwnd),
				uintptr(unsafe.Pointer(msgPtr)),
				uintptr(unsafe.Pointer(titlePtr)),
				mbOK|mbIconError|mbSetForeground,
			)
		}
		return 0

	case wmDestroy:
		guiMu.Lock()
		guiRunSeq++
		latestError = ""
		hwndMain = 0
		hwndSubTitle = 0
		hwndKeyEdit = 0
		hwndSaveFileChk = 0
		hwndBtnConnect = 0
		hwndStatusBox = 0
		hwndStatus = 0
		if guiCancel != nil {
			guiCancel()
			guiCancel = nil
		}
		cl := guiClient
		guiClient = nil
		guiMu.Unlock()
		if cl != nil {
			cl.Close()
		}
		_, _, _ = procPostQuitMessage.Call(0)
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return ret
}

func RunGUI(opts ClientOptions) error {
	hideConsoleWindow()
	enableVisualStyles()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	guiBaseOpts = opts

	hbrBg, _, _ = procCreateSolidBrush.Call(rgb(248, 250, 252))
	hbrStatIdle, _, _ = procCreateSolidBrush.Call(rgb(241, 245, 249))
	hbrStatOk, _, _ = procCreateSolidBrush.Call(rgb(240, 253, 244))
	hbrStatErr, _, _ = procCreateSolidBrush.Call(rgb(254, 242, 242))

	hInst, _, _ := procGetModuleHandleW.Call(0)
	hCursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	hIcon, _, _ := procLoadIconW.Call(0, idiApplication)
	className, _ := syscall.UTF16PtrFromString("MCPRemoteUserClientWnd")
	title, _ := syscall.UTF16PtrFromString("MCP Remote Access")

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     syscall.Handle(hInst),
		hIcon:         syscall.Handle(hIcon),
		hCursor:       syscall.Handle(hCursor),
		hbrBackground: syscall.Handle(hbrBg),
		lpszClassName: className,
		hIconSm:       syscall.Handle(hIcon),
	}
	_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	const winW, winH = 440, 282
	scrW, _, _ := procGetSystemMetrics.Call(smCxScreen)
	scrH, _, _ := procGetSystemMetrics.Call(smCyScreen)
	posX, posY := 250, 250
	if int(scrW) > winW && int(scrH) > winH {
		posX = (int(scrW) - winW) / 2
		posY = (int(scrH) - winH) / 2
	}

	h, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedDialog|wsVisible,
		uintptr(posX), uintptr(posY), winW, winH,
		0, 0, hInst, 0,
	)
	guiMu.Lock()
	hwndMain = syscall.Handle(h)
	guiMu.Unlock()
	_, _, _ = procShowWindow.Call(h, swShow)
	_, _, _ = procUpdateWindow.Call(h)

	if strings.TrimSpace(opts.UserKey) != "" {
		startOrRestartAgent(opts.UserKey, HasConfigFile())
	} else {
		updateGUIStatus("Status: Enter Connection Key and click Connect.")
	}

	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		handled, _, _ := procIsDialogMessageW.Call(uintptr(hwndMain), uintptr(unsafe.Pointer(&m)))
		if handled == 0 {
			_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}
	return nil
}
