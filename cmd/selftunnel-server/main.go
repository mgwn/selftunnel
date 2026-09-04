// Command selftunnel-server runs the selftunnel server (spec §3): a single
// listener serving the WebSocket tunnel intake, the relay endpoint and the
// health check. Registrations persist under -data; no database, no web UI.
//
// Usage:
//
//	selftunnel-server [-addr :8080] [-data ./data] [-allowed-origins native-client://]
//	             [-cert cert.pem -key key.pem] [-debug]
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"selftunnel/internal/server"
)

// main parses the flags, loads (or starts fresh) the tunnel registry and
// serves until the listener fails. With -debug the slog level is lowered
// to Debug and every request is logged by the server's debug middleware.
func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dataDir := flag.String("data", "./data", "data directory")
	certFile := flag.String("cert", "", "TLS certificate file (PEM); if set with -key, serves HTTPS/WSS")
	keyFile := flag.String("key", "", "TLS private key file (PEM)")
	allowed := flag.String("allowed-origins", "native-client://", "comma-separated WebSocket Origin prefixes to allow; use '*' to allow any")
	debug := flag.Bool("debug", false, "print every network request for debugging")
	flag.Parse()

	if *debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	if err := os.MkdirAll(*dataDir, 0755); err != nil {
		slog.Error("mkdir data", "err", err)
		os.Exit(1)
	}

	reg := server.NewRegistry(*dataDir)
	if err := reg.Load(); err != nil {
		slog.Error("load registry", "err", err)
		os.Exit(1)
	}

	origins := parseList(*allowed)
	srv := server.New(reg, *addr, origins, *debug)
	useTLS := *certFile != "" && *keyFile != ""
	slog.Info("selftunnel-server starting", "addr", *addr, "data", *dataDir, "tls", useTLS, "debug", *debug)

	var err error
	if useTLS {
		err = http.ListenAndServeTLS(*addr, *certFile, *keyFile, srv.Handler())
	} else {
		err = http.ListenAndServe(*addr, srv.Handler())
	}
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}

// parseList splits a comma-separated flag value into trimmed non-empty
// entries; an empty input yields nil.
func parseList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
