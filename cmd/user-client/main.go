package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"mcp-remoteaccess/internal/agent"
	"mcp-remoteaccess/internal/protocol"
)

func main() {
	if len(os.Args) > 1 {
		agent.EnsureCLIConsole()
		flag.CommandLine.SetOutput(os.Stderr)
	}
	saved := agent.LoadPersistentConfig()

	cliMode := flag.Bool("cli", false, "Run in headless CLI mode instead of GUI mode")
	guiMode := flag.Bool("gui", false, "Force GUI mode")
	serverAddr := flag.String("server", envOrDefault("MCP_SERVER_ADDR", protocol.DefaultServerAddr), "Relay server address (CLI override; defaults to built-in DefaultServerAddr)")
	userKey := flag.String("key", envOrDefault("MCP_USER_KEY", saved.UserKey), "Connection key for user-client")
	machineID := flag.String("id", envOrDefault("MCP_MACHINE_ID", saved.MachineID), "Custom machine ID")
	shellSpec := flag.String("shell", envOrDefault("MCP_SHELL", saved.Shell), "Preferred shell (pwsh, powershell, cmd, bash, sh, ash)")
	workdir := flag.String("workdir", envOrDefault("MCP_WORKDIR", saved.Workdir), "Initial working directory")
	noUpdate := flag.Bool("no-update", false, "Disable automatic binary self-update check")
	insecure := flag.Bool("insecure", envOrDefault("MCP_INSECURE", "") == "1", "Skip SSL certificate verification and allow insecure connections")
	flag.Parse()

	opts := agent.ClientOptions{
		ServerAddr: *serverAddr,
		UserKey:    *userKey,
		MachineID:  *machineID,
		Shell:      *shellSpec,
		Workdir:    *workdir,
		AutoUpdate: !*noUpdate,
		Insecure:   *insecure,
	}

	if *cliMode && !*guiMode {
		agent.EnsureCLIConsole()
		if opts.UserKey == "" {
			opts.UserKey = "default-user-key"
		}
		// Persist CLI settings for convenience
		saved.ServerAddr = opts.ServerAddr
		saved.UserKey = opts.UserKey
		saved.MachineID = opts.MachineID
		if opts.Shell != "" {
			saved.Shell = opts.Shell
		}
		_ = agent.SavePersistentConfig(saved)

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		client := agent.NewClient(opts)
		if err := client.Run(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "[user-client] fatal: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := agent.RunGUI(opts); err != nil {
		fmt.Fprintf(os.Stderr, "[user-client] GUI error: %v\n", err)
		os.Exit(1)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
