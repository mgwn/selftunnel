package server

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// logRequests wraps next so that every request is logged (the -debug
// feature): one line when it arrives (method, path, peer, query) and one
// line with status, bytes and duration once it completes. WebSocket
// upgrades are reported with status 101 when the handler returns.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		attrs := []any{"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr}
		if r.URL.RawQuery != "" {
			attrs = append(attrs, "query", r.URL.RawQuery)
		}
		slog.Info("request", attrs...)

		rec := &recordingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		status := rec.status
		if rec.hijacked {
			status = http.StatusSwitchingProtocols
		}
		slog.Info("request done", "method", r.Method, "path", r.URL.Path,
			"status", status, "bytes", rec.bytes, "duration", time.Since(start))
	})
}

// recordingWriter wraps an http.ResponseWriter to capture the response
// status and byte count for the debug request log. It transparently
// forwards Flush and Hijack — the streaming relay handler depends on
// per-chunk Flush and the WebSocket upgrade depends on Hijack, so a plain
// wrapper would break both.
type recordingWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	hijacked bool
}

// WriteHeader records status and forwards the call.
func (w *recordingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Write counts the payload bytes and forwards the call.
func (w *recordingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush forwards Flush when the underlying writer supports it, keeping
// chunked/SSE streaming behavior unchanged.
func (w *recordingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards connection hijacking (required by the WebSocket upgrade)
// and marks the request as hijacked so the debug log reports status 101.
func (w *recordingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	w.hijacked = true
	return h.Hijack()
}
