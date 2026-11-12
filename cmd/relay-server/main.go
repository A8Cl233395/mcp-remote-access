package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"

	"mcp-remoteaccess/internal/relay"
)

func main() {
	addr := flag.String("addr", envOrDefault("RELAY_ADDR", ":20055"), "Listen address (e.g. :20055)")
	userKey := flag.String("user-key", envOrDefault("RELAY_USER_KEY", "default-user-key"), "Fixed key for user-client authentication")
	devKey := flag.String("dev-key", envOrDefault("RELAY_DEV_KEY", "default-dev-key"), "Fixed key for dev-client authentication")
	releasesDir := flag.String("releases-dir", envOrDefault("RELAY_RELEASES_DIR", "./releases"), "Directory hosting user-client binaries for auto-update")
	tlsCert := flag.String("tls-cert", envOrDefault("RELAY_TLS_CERT", ""), "Path to SSL/TLS certificate PEM file (e.g. fullchain.pem)")
	tlsKey := flag.String("tls-key", envOrDefault("RELAY_TLS_KEY", ""), "Path to SSL/TLS private key PEM file (e.g. privkey.pem)")
	insecure := flag.Bool("insecure", envOrDefault("RELAY_INSECURE", "") == "1", "Allow running without a formal SSL certificate (uses self-signed TLS and accepts plain HTTP/WS)")
	flag.Parse()

	if err := os.MkdirAll(*releasesDir, 0755); err != nil {
		log.Fatalf("Failed to create releases dir %s: %v", *releasesDir, err)
	}

	tlsCfg, err := relay.BuildTLSConfig(*tlsCert, *tlsKey, *insecure)
	if err != nil {
		log.Fatalf("[relay-server] %v", err)
	}

	srv := relay.NewServer(relay.Config{
		ListenAddr:  *addr,
		UserKey:     *userKey,
		DevKey:      *devKey,
		ReleasesDir: *releasesDir,
	})

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("[relay-server] listen %s failed: %v", *addr, err)
	}

	listener := relay.NewFlexTLSListener(ln, tlsCfg, *insecure && *tlsCert == "")
	if *insecure && *tlsCert == "" {
		log.Printf("[relay-server] listening on %s in -insecure mode (self-signed HTTPS/WSS + plain HTTP/WS enabled, releases-dir=%s)", *addr, *releasesDir)
	} else {
		log.Printf("[relay-server] listening on %s with strict HTTPS/WSS (cert=%s, releases-dir=%s)", *addr, *tlsCert, *releasesDir)
	}

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: srv.Handler(),
	}
	if err := httpSrv.Serve(listener); err != nil {
		log.Fatalf("[relay-server] exited: %v", err)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
