//go:build windows

package agent

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func buildCmdExe(shellPath, command string) *exec.Cmd {
	quotedShell := `"` + strings.Trim(shellPath, `"`) + `"`
	cmd := exec.Command(shellPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true,
		CmdLine:    fmt.Sprintf(`%s /d /s /c "%s"`, quotedShell, command),
	}
	return cmd
}

func configureProcessAttributes(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow: true,
		}
	} else {
		cmd.SysProcAttr.HideWindow = true
	}
}

func KillProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	killer := exec.Command("taskkill", "/pid", strconv.Itoa(pid), "/f", "/t")
	killer.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = killer.Run()
}

func KillTargetDescription(pid int) string {
	return fmt.Sprintf("process tree of pid %d, best-effort", pid)
}

func IsProcessGroupAlive(pid int) *bool {
	return nil
}

func CheckIsAdmin() bool {
	// Attempt to open physical drive or check token elevation via net session
	f, err := syscall.Open(`\\.\PHYSICALDRIVE0`, syscall.O_RDONLY, 0)
	if err == nil {
		_ = syscall.Close(f)
		return true
	}
	return false
}
