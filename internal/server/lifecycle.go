package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
)

// App embeds the relay server in a managed http.Server with graceful
// start/stop (spec §3.8.1). The CLI front end keeps its own listen loop;
// the GUI server drives an App.
//
// Stop closes every online session first — relay handlers blocked on
// their pending channels then finish with 502 instead of stalling the
// shutdown — and afterwards shuts the listener down, giving in-flight
// requests up to the caller's context deadline to complete.
type App struct {
	srv      *Server
	addr     string
	certFile string
	keyFile  string

	mu      sync.Mutex
	ln      net.Listener
	httpSrv *http.Server
}

// NewApp creates an App serving srv's handler on addr. If certFile and
// keyFile are both non-empty, the listener serves HTTPS/WSS directly.
func NewApp(srv *Server, addr, certFile, keyFile string) *App {
	return &App{srv: srv, addr: addr, certFile: certFile, keyFile: keyFile}
}

// Start begins listening and serving on a background goroutine; it
// returns immediately. The resolved address is available via Addr.
// Starting an already-running App is an error.
func (a *App) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln != nil {
		return errors.New("server already running")
	}
	ln, err := net.Listen("tcp", a.addr)
	if err != nil {
		return err
	}
	a.ln = ln
	a.httpSrv = &http.Server{Handler: a.srv.Handler()}
	go func() {
		var err error
		if a.certFile != "" && a.keyFile != "" {
			err = a.httpSrv.ServeTLS(ln, a.certFile, a.keyFile)
		} else {
			err = a.httpSrv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("relay listener failed", "addr", a.addr, "err", err)
		}
	}()
	return nil
}

// Addr returns the resolved listener address ("host:port"), or "" while
// the App is not running.
func (a *App) Addr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln == nil {
		return ""
	}
	return a.ln.Addr().String()
}

// Running reports whether the listener is up.
func (a *App) Running() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ln != nil
}

// Stop tears the App down gracefully (spec §3.8.1): all tunnel sessions
// are closed, then the listener shuts down with the caller's context as
// the deadline for in-flight requests. Stop on a stopped App is a no-op.
func (a *App) Stop(ctx context.Context) error {
	a.mu.Lock()
	ln, httpSrv := a.ln, a.httpSrv
	a.ln, a.httpSrv = nil, nil
	a.mu.Unlock()

	if httpSrv == nil {
		return nil
	}
	_ = ln

	// Close every online session first, so relay handlers blocked on
	// pending channels finish immediately instead of stalling Shutdown.
	for _, t := range a.srv.registry.List() {
		if sess := t.Session(); sess != nil {
			sess.Close()
		}
	}
	return httpSrv.Shutdown(ctx)
}

// Srv returns the embedded relay server, for status polling and
// administration (spec §3.8.4).
func (a *App) Srv() *Server {
	return a.srv
}
