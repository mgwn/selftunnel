// Package server implements the relay server (spec §3, §4): a single HTTP
// listener that serves the WebSocket tunnel intake (/ws/tunnel), the relay
// endpoint (/t/{tunnelID}/*) and the health check (/healthz). Tunnel
// registrations persist to one JSON file; no database, no web UI.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/gorilla/websocket"

	"github.com/mgwn/selftunnel/internal/proto"
)

// Server is the relay HTTP/WebSocket server. It owns the tunnel registry,
// the WebSocket upgrader and the online-session counter. Construct with
// New; it has no background goroutines of its own — sessions run inside
// their HTTP handlers.
type Server struct {
	registry       *Registry
	addr           string
	allowedOrigins []string
	upgrader       websocket.Upgrader
	onlineCount    atomic.Int64
	debug          bool
}

// New creates a server. allowedOrigins is a list of Origin header prefixes
// to accept on the WebSocket handshake; pass []string{"*"} to disable the
// check. When debug is true, every request handled by the server is logged
// (see Handler).
func New(registry *Registry, addr string, allowedOrigins []string, debug bool) *Server {
	s := &Server{
		registry:       registry,
		addr:           addr,
		allowedOrigins: allowedOrigins,
		debug:          debug,
	}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:    64 * 1024,
		WriteBufferSize:   64 * 1024,
		EnableCompression: false,
		CheckOrigin:       s.checkOrigin,
	}
	return s
}

// checkOrigin reports whether a WebSocket handshake may proceed. The Origin
// header must be present and match one of the configured prefixes (spec
// §3.1 step 1); an empty Origin is always rejected.
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	for _, o := range s.allowedOrigins {
		if o == "*" || strings.HasPrefix(origin, o) {
			return true
		}
	}
	return false
}

// Mux returns the route table (spec §4): /ws/tunnel, /healthz and /t/.
// Routes are registered on a fresh ServeMux on every call, so the returned
// mux can be wrapped or mounted freely.
func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/tunnel", s.handleTunnel)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/t/", s.handleRelay)
	return mux
}

// Handler returns the root HTTP handler. With debug enabled, every request
// is logged on arrival and again with status, bytes and duration when done.
func (s *Server) Handler() http.Handler {
	h := http.Handler(s.Mux())
	if s.debug {
		h = logRequests(h)
	}
	return h
}

// handleTunnel upgrades /ws/tunnel to a WebSocket and runs the session
// until it ends (spec §3.1). The handler blocks for the whole lifetime of
// the tunnel connection; every other endpoint is unaffected meanwhile.
func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("websocket upgrade failed", "err", err)
		return
	}
	slog.Info("client connected", "addr", r.RemoteAddr)
	sess := newSession(conn, s, r.RemoteAddr)
	sess.Run()
}

// handleHello processes the first hello frame of a session (spec §3.1
// steps 3–5): it allocates or reuses a tunnelID, kicks any previous session
// of the same ID (last-connect-wins) and answers hello_ack. The returned
// error is the rejection reason to send back in a failing hello_ack.
func (s *Server) handleHello(sess *Session, f *proto.Frame) error {
	tun, reused, denied, secret, err := s.registry.Register(f.DesiredId, f.Secret)
	if err != nil {
		return err
	}

	sess.tun = tun
	old := tun.session.Swap(sess)
	if old != nil && old != sess {
		old.close()
	}
	s.onlineCount.Add(1)

	ack := &proto.Frame{
		Type:          "hello_ack",
		Ok:            true,
		TunnelID:      tun.ID,
		IDReused:      reused,
		DesiredDenied: denied,
	}
	if !reused && secret != "" {
		ack.Secret = secret
	}

	slog.Info("tunnel online", "tunnel", tun.ID, "client", sess.remoteAddr, "reused", reused, "denied", denied)
	return sess.send(ack)
}

// handleHealthz serves GET /healthz (spec §3.3): a minimal liveness report
// with the number of registered tunnels and online sessions. It deliberately
// leaks no tunnel IDs.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"tunnels": s.registry.Count(),
		"online":  s.onlineCount.Load(),
	})
}
