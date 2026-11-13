//go:build windows

package agent

import (
	"fmt"
	"syscall"
)

var (
	kernel32DLL              = syscall.NewLazyDLL("kernel32.dll")
	procGetACP               = kernel32DLL.NewProc("GetACP")
	procGetOEMCP             = kernel32DLL.NewProc("GetOEMCP")
	procGetConsoleOutputCP   = kernel32DLL.NewProc("GetConsoleOutputCP")
)

func getWindowsCodePageName(cp uint32) string {
	switch cp {
	case 65001:
		return "utf-8"
	case 936:
		return "gbk"
	case 54936:
		return "gb18030"
	case 950:
		return "big5"
	case 932:
		return "shift_jis"
	case 949:
		return "euc-kr"
	case 1252:
		return "windows-1252"
	case 1251:
		return "windows-1251"
	case 1250:
		return "windows-1250"
	case 1253:
		return "windows-1253"
	case 1254:
		return "windows-1254"
	case 1255:
		return "windows-1255"
	case 1256:
		return "windows-1256"
	case 1257:
		return "windows-1257"
	case 1258:
		return "windows-1258"
	case 437:
		return "cp437"
	case 850:
		return "cp850"
	case 866:
		return "cp866"
	case 20866:
		return "koi8-r"
	case 21866:
		return "koi8-u"
	case 28591:
		return "iso-8859-1"
	case 1200:
		return "utf-16le"
	case 1201:
		return "utf-16be"
	default:
		if cp > 0 {
			return fmt.Sprintf("cp%d", cp)
		}
		return "utf-8"
	}
}

// SystemFileEncoding returns the OS default ANSI file encoding on Windows (GetACP).
func SystemFileEncoding() string {
	r, _, _ := procGetACP.Call()
	cp := uint32(r)
	if cp == 0 {
		return "utf-8"
	}
	return getWindowsCodePageName(cp)
}

// SystemConsoleEncoding returns the active console output encoding on Windows.
// GUI processes (-H=windowsgui) have no attached console so GetConsoleOutputCP returns 0;
// in that case child console processes inherit the OEM codepage (GetOEMCP), falling back to GetACP.
func SystemConsoleEncoding() string {
	if r, _, _ := procGetConsoleOutputCP.Call(); r != 0 {
		return getWindowsCodePageName(uint32(r))
	}
	if r, _, _ := procGetOEMCP.Call(); r != 0 {
		return getWindowsCodePageName(uint32(r))
	}
	return SystemFileEncoding()
}
