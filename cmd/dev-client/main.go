package main

import (
	"flag"
	"fmt"
	"os"

	"mcp-remoteaccess/internal/dev"
	"mcp-remoteaccess/internal/protocol"
)

func main() {
	serverAddr := flag.String("server", envOrDefault("MCP_SERVER_ADDR", protocol.DefaultServerAddr), "Relay server address (defaults to built-in DefaultServerAddr)")
	devKey := flag.String("key", envOrDefault("MCP_DEV_KEY", "default-dev-key"), "Developer authentication key")
	defaultMachine := flag.String("machine", envOrDefault("MCP_DEFAULT_MACHINE", ""), "Optional default target machine ID")
	insecure := flag.Bool("insecure", envOrDefault("MCP_INSECURE", "") == "1", "Skip SSL certificate verification and allow insecure connections")
	flag.Parse()

	sess := dev.NewSession(dev.SessionConfig{
		ServerAddr:     *serverAddr,
		DevKey:         *devKey,
		DefaultMachine: *defaultMachine,
		Insecure:       *insecure,
	})
	defer sess.Close()

	mcpSrv := dev.NewMCPServer(sess)
	if err := mcpSrv.ServeStdio(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "[dev-client] stdio error: %v\n", err)
		os.Exit(1)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
