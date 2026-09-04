// Command selftunnel-client is the headless selftunnel client (spec §3.5):
// it connects to the relay server, reclaims (or allocates) a tunnelID and
// forwards relayed requests to the configured target, reconnecting with
// exponential backoff forever. Suitable for servers, containers and CI.
//
// Usage:
//
//	selftunnel-client [-config config.json] [-server wss://host]
//	             [-target http://host:port] [-custom-id ab12cd34]
//	             [-insecure] [-server-insecure] [-client-cert p -client-key k]
//	             [-debug]
//
// Command-line flags override the config file; the allocated tunnelID and
// secret are persisted to the config file after each successful handshake.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"selftunnel/internal/client"
)

// main loads the config, applies flag overrides, validates the addresses
// and runs the client until SIGINT/SIGTERM. It exits non-zero only when
// the client stops with an error (e.g. invalid configuration up front).
func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	serverFlag := flag.String("server", "", "relay server, e.g. wss://relay.example.com or https://relay.example.com")
	targetFlag := flag.String("target", "", "real target base URL, e.g. http://192.168.1.10:8080")
	customIDFlag := flag.String("custom-id", "", "desired 8-char tunnelID (optional)")
	insecureTarget := flag.Bool("insecure", false, "skip TLS verification for the target HTTPS server")
	insecureServer := flag.Bool("server-insecure", false, "skip TLS verification for the relay WSS server")
	clientCert := flag.String("client-cert", "", "client certificate PEM for target mTLS")
	clientKey := flag.String("client-key", "", "client key PEM for target mTLS")
	debug := flag.Bool("debug", false, "print every forwarded network request for debugging")
	flag.Parse()

	if *debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	cfg, err := client.LoadConfig(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	if *serverFlag != "" {
		cfg.Server = client.NormalizeServer(*serverFlag)
	}
	if *targetFlag != "" {
		cfg.Target = strings.TrimSpace(*targetFlag)
	}
	if *customIDFlag != "" {
		cfg.CustomID = strings.ToLower(strings.TrimSpace(*customIDFlag))
	}

	cfg.Server = client.NormalizeServer(cfg.Server)
	if cfg.Server == "" {
		slog.Error("missing relay server; use -server or set it in the config file")
		os.Exit(1)
	}
	if cfg.Target != "" && !client.IsValidTarget(cfg.Target) {
		slog.Error("invalid target URL", "target", cfg.Target)
		os.Exit(1)
	}
	if cfg.CustomID != "" && !client.IsValidCustomID(cfg.CustomID) {
		slog.Error("invalid custom-id", "id", cfg.CustomID)
		os.Exit(1)
	}

	var c *client.Client
	c = client.New(cfg, client.Options{
		ServerInsecure: *insecureServer,
		TargetInsecure: *insecureTarget,
		ClientCert:     *clientCert,
		ClientKey:      *clientKey,
		Debug:          *debug,
		OnStatusChange: func(s client.Status) {
			slog.Info("status changed", "status", s.String())
			if s == client.StatusOnline {
				printConnected(c.Server(), c.TunnelID())
			}
		},
		OnLog: func(level, msg string, attrs ...any) {
			switch level {
			case "warn":
				slog.Warn(msg, attrs...)
			case "debug":
				slog.Debug(msg, attrs...)
			default:
				slog.Info(msg, attrs...)
			}
		},
	})
	c.SetConfigPath(*configPath)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	slog.Info("selftunnel-client starting", "server", cfg.Server, "target", cfg.Target, "customId", cfg.CustomID, "debug", *debug)
	if err := c.Run(ctx); err != nil {
		slog.Error("client stopped", "err", err)
		os.Exit(1)
	}
}

// publicTunnelURL turns the WebSocket server URL into the public HTTP
// tunnel URL, e.g. wss://relay.example.com/ws/tunnel →
// https://relay.example.com/t/{tunnelID}.
func publicTunnelURL(serverWS, tunnelID string) string {
	s := strings.TrimSuffix(serverWS, "/ws/tunnel")
	if strings.HasPrefix(s, "wss://") {
		s = "https://" + s[6:]
	} else if strings.HasPrefix(s, "ws://") {
		s = "http://" + s[5:]
	}
	return fmt.Sprintf("%s/t/%s", s, tunnelID)
}

// printConnected prints the boxed "Tunnel connected" notice with the public
// entry URL — the user-facing confirmation after each successful handshake.
func printConnected(server, tunnelID string) {
	url := publicTunnelURL(server, tunnelID)
	lines := []string{
		"Tunnel connected",
		url,
	}
	width := 0
	for _, line := range lines {
		if n := len([]rune(line)); n > width {
			width = n
		}
	}
	border := strings.Repeat("─", width+4)
	fmt.Println()
	fmt.Println("┌" + border + "┐")
	for _, line := range lines {
		fmt.Printf("│  %-*s  │\n", width, line)
	}
	fmt.Println("└" + border + "┘")
	fmt.Println()
}
