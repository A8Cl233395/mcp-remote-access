//go:build !windows

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func buildCmdExe(shellPath, command string) *exec.Cmd {
	return exec.Command(shellPath, "-c", command)
}

func configureProcessAttributes(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

func KillProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	// Send SIGTERM to process group (-pid), wait 200ms, then SIGKILL
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func KillTargetDescription(pid int) string {
	return fmt.Sprintf("process group %d", pid)
}

func IsProcessGroupAlive(pid int) *bool {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, 0)
	alive := err == nil || err == syscall.EPERM
	return &alive
}

func CheckIsAdmin() bool {
	return os.Geteuid() == 0
}
