package server

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestRecordingWriterCapturesStatusAndBytes verifies the debug middleware's
// bookkeeping: the wrapper records status and byte counts while leaving the
// underlying response untouched.
//
//	Given  a recordingWriter over a recorder;
//	When   a status is written and bytes are appended;
//	Then   the captured status/bytes match and the recorder sees the same
//	       output.
func TestRecordingWriterCapturesStatusAndBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &recordingWriter{ResponseWriter: rec, status: http.StatusOK}

	rw.WriteHeader(http.StatusTeapot)
	n, err := rw.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	_, _ = rw.Write([]byte(" world"))

	if rw.status != http.StatusTeapot {
		t.Errorf("captured status = %d, want %d", rw.status, http.StatusTeapot)
	}
	if rw.bytes != 11 {
		t.Errorf("captured bytes = %d, want 11", rw.bytes)
	}
	if rec.Code != http.StatusTeapot || rec.Body.String() != "hello world" {
		t.Error("underlying writer must receive the same status and bytes")
	}
}

// capturedLogs swaps the standard log output (which slog's default handler
// writes to) for a thread-safe buffer and returns a reader function. The
// original output is restored when the test ends.
func capturedLogs(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	log.SetOutput(&lockedBuffer{mu: &mu, buf: &buf})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// lockedBuffer is a mutex-guarded bytes.Buffer, safe for the concurrent
// slog writes captured during tests.
type lockedBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *lockedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// TestLogRequestsLogsEveryRequest verifies the -debug contract: with the
// middleware installed, every request is logged on arrival (method, path,
// peer, query) and on completion (status, bytes, duration), and serving
// still works through the wrapper.
//
//	Given  an HTTP server wrapped by logRequests;
//	When   one request is served;
//	Then   the response is correct and the log contains both the arrival
//	       line and the request-done line with status and byte count.
func TestLogRequestsLogsEveryRequest(t *testing.T) {
	read := capturedLogs(t)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("payload"))
	})
	srv := httptest.NewServer(logRequests(inner))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/t/abcd1234/x?debug=1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (middleware must not break serving)", resp.StatusCode)
	}

	logs := read()
	for _, want := range []string{
		"request method=GET",
		"path=/t/abcd1234/x",
		`query="debug=1"`,
		"request done",
		"status=200",
		"bytes=7",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log output missing %q; got:\n%s", want, logs)
		}
	}
}
