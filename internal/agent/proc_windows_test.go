//go:build windows

package agent

import (
	"strings"
	"testing"
)

func TestBuildCmdExeQuotesShellPathWithSpaces(t *testing.T) {
	shellPath := `C:\Program Files\Custom Shell\cmd.exe`
	cmd := buildCmdExe(shellPath, "echo ok")
	if cmd.SysProcAttr == nil {
		t.Fatalf("expected SysProcAttr to be set")
	}
	wantPrefix := `"` + shellPath + `" /d /s /c `
	if !strings.HasPrefix(cmd.SysProcAttr.CmdLine, wantPrefix) {
		t.Fatalf("expected CmdLine to quote shellPath (%q), got %q", wantPrefix, cmd.SysProcAttr.CmdLine)
	}
}
