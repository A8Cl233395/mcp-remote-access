package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ShellFallbackChain returns the ordered fallback shell list for the current OS:
// Windows: pwsh -> powershell -> cmd
// Linux/Unix: bash -> sh -> ash
func ShellFallbackChain() []string {
	if runtime.GOOS == "windows" {
		return []string{"pwsh", "powershell", "cmd"}
	}
	return []string{"bash", "sh", "ash"}
}

// DetectAvailableShells probes all available shells on the machine, starting with the fallback chain.
func DetectAvailableShells() []string {
	candidates := ShellFallbackChain()
	if runtime.GOOS == "windows" {
		candidates = append(candidates, "bash")
	} else {
		candidates = append(candidates, "zsh", "dash")
	}

	seen := make(map[string]bool)
	var result []string
	for _, c := range candidates {
		if p := ResolveShellExecutable(c); p != "" {
			name := NormalizeShellName(p)
			if !seen[name] {
				seen[name] = true
				result = append(result, name)
			}
		}
	}
	return result
}

// SelectShell resolves the requested shell or falls back along the OS chain.
func SelectShell(preferred string) (shellName string, shellPath string) {
	if preferred != "" {
		if p := ResolveShellExecutable(preferred); p != "" {
			return NormalizeShellName(p), p
		}
	}
	for _, candidate := range ShellFallbackChain() {
		if p := ResolveShellExecutable(candidate); p != "" {
			return NormalizeShellName(p), p
		}
	}
	if runtime.GOOS == "windows" {
		comspec := os.Getenv("COMSPEC")
		if comspec == "" {
			comspec = "cmd.exe"
		}
		return "cmd", comspec
	}
	return "sh", "/bin/sh"
}

func ResolveShellExecutable(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}
	if filepath.IsAbs(spec) {
		if st, err := os.Stat(spec); err == nil && !st.IsDir() {
			return spec
		}
		return ""
	}
	if p, err := exec.LookPath(spec); err == nil {
		return p
	}
	if runtime.GOOS == "windows" && strings.EqualFold(spec, "bash") {
		if gitPath, err := exec.LookPath("git"); err == nil {
			gitBash := filepath.Join(filepath.Dir(filepath.Dir(gitPath)), "bin", "bash.exe")
			if st, err := os.Stat(gitBash); err == nil && !st.IsDir() {
				return gitBash
			}
		}
	}
	return ""
}

func NormalizeShellName(shellPath string) string {
	base := strings.ToLower(filepath.Base(shellPath))
	ext := filepath.Ext(base)
	if ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

func BuildShellCommand(shellPath, command, workdir string) *exec.Cmd {
	name := NormalizeShellName(shellPath)
	var cmd *exec.Cmd
	switch name {
	case "pwsh", "powershell":
		cmd = exec.Command(shellPath, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command)
	case "cmd":
		cmd = buildCmdExe(shellPath, command)
	default:
		// bash, sh, ash, zsh, dash
		cmd = exec.Command(shellPath, "-c", command)
	}
	cmd.Dir = workdir
	cmd.Env = os.Environ()
	configureProcessAttributes(cmd)
	return cmd
}

func ExpandHome(p string) string {
	p = strings.TrimSpace(p)
	if len(p) >= 2 && ((p[0] == '"' && p[len(p)-1] == '"') || (p[0] == '\'' && p[len(p)-1] == '\'')) {
		p = p[1 : len(p)-1]
	}
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func ResolvePath(inputPath, baseWorkdir string) string {
	expanded := ExpandHome(inputPath)
	if expanded == "" || expanded == "." {
		return filepath.Clean(baseWorkdir)
	}
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded)
	}
	return filepath.Clean(filepath.Join(baseWorkdir, expanded))
}

func ResolveWorkdir(inputPath, currentWorkdir string) (string, error) {
	resolved := ResolvePath(inputPath, currentWorkdir)
	st, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("Working directory does not exist: %s", resolved)
		}
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("Working directory is not a directory: %s", resolved)
	}
	return resolved, nil
}
