package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mgwn/selftunnel/internal/proto"
)

// TestNormalizeServer verifies the server address normalization contract
// (spec §3.5.3): https/http/bare-host inputs all become the canonical
// wss://host/ws/tunnel WebSocket URL, and empty input stays empty.
//
//	Given  server addresses in every accepted spelling;
//	When   NormalizeServer runs;
//	Then   each maps to its canonical wss://…/ws/tunnel form.
func TestNormalizeServer(t *testing.T) {
	cases := map[string]string{
		"https://relay.example.com":         "wss://relay.example.com/ws/tunnel",
		"http://relay.example.com":          "ws://relay.example.com/ws/tunnel",
		"relay.example.com":                 "wss://relay.example.com/ws/tunnel",
		"wss://relay.example.com":           "wss://relay.example.com/ws/tunnel",
		"ws://relay.example.com":            "ws://relay.example.com/ws/tunnel",
		"wss://relay.example.com/":          "wss://relay.example.com/ws/tunnel",
		"wss://relay.example.com/ws/tunnel": "wss://relay.example.com/ws/tunnel",
		"":                                  "",
	}
	for in, want := range cases {
		if got := NormalizeServer(in); got != want {
			t.Errorf("NormalizeServer(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsValidTarget verifies the target validation contract (spec §6.5):
// targets must start with http:// or https://.
//
//	Given  target URLs of various schemes;
//	When   IsValidTarget runs;
//	Then   only http(s) prefixes pass.
func TestIsValidTarget(t *testing.T) {
	valid := []string{"http://192.168.1.10:8080", "https://example.com"}
	invalid := []string{"", "ftp://example.com", "example.com"}
	for _, v := range valid {
		if !IsValidTarget(v) {
			t.Errorf("IsValidTarget(%q) = false, want true", v)
		}
	}
	for _, v := range invalid {
		if IsValidTarget(v) {
			t.Errorf("IsValidTarget(%q) = true, want false", v)
		}
	}
}

// TestIsValidCustomID verifies the custom ID gate (spec §3.1): exactly 8
// lowercase alphanumerics.
//
//	Given  candidate custom IDs of varying length, case and charset;
//	When   IsValidCustomID runs;
//	Then   only the canonical ones pass.
func TestIsValidCustomID(t *testing.T) {
	valid := []string{"abcd1234", "00000000", "zzzzzzzz"}
	invalid := []string{"", "abcd123", "abcd12345", "ABCD1234", "abcd-123", "abcd_123"}
	for _, v := range valid {
		if !IsValidCustomID(v) {
			t.Errorf("IsValidCustomID(%q) = false, want true", v)
		}
	}
	for _, v := range invalid {
		if IsValidCustomID(v) {
			t.Errorf("IsValidCustomID(%q) = true, want false", v)
		}
	}
}

// TestConfigSaveLoadRoundtrip verifies spec §6.5: the config file survives
// a save/load roundtrip with every field intact.
//
//	Given  a fully populated config;
//	When   it is saved and loaded back;
//	Then   the loaded config equals the original.
func TestConfigSaveLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	in := &Config{
		Server:   "wss://relay.example.com/ws/tunnel",
		Target:   "http://192.168.1.10:8080",
		CustomID: "abcd1234",
		TunnelID: "wxyz9876",
		Secret:   "s3cr3t-value",
	}
	if err := SaveConfig(path, in); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	out, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if *out != *in {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", out, in)
	}
}

// TestLoadConfigMissingFile verifies the first-run experience (spec §3.5
// step 1): a missing config file yields an empty config, not an error.
//
//	Given  no config file on disk;
//	When   LoadConfig runs;
//	Then   an empty config is returned with a nil error.
func TestLoadConfigMissingFile(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing config must not error: %v", err)
	}
	if cfg == nil || cfg.Server != "" {
		t.Fatalf("missing config must yield an empty config, got %+v", cfg)
	}
}

// --- reconnect lifecycle test ----------------------------------------------

// wsStub is a minimal relay-server stub for client lifecycle tests: it
// accepts tunnel upgrades, answers hello, and tracks the connections so
// the test can kill them server-side to force reconnects.
type wsStub struct {
	mu    sync.Mutex
	conns []*websocket.Conn
}

func (s *wsStub) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
}

// newWSServer starts the stub and returns it together with its ws:// URL.
func newWSServer(t *testing.T, stub *wsStub) string {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		stub.mu.Lock()
		stub.conns = append(stub.conns, conn)
		stub.mu.Unlock()
		if _, _, err := conn.ReadMessage(); err != nil { // hello
			return
		}
		if err := conn.WriteJSON(&proto.Frame{Type: "hello_ack", Ok: true, TunnelID: "abc123de"}); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http")
}

// TestCloseFiresPerConnectionGeneration is the regression test for the
// client-side once-flag leak: connect() must re-arm close() for every new
// connection. Before the fix, closed stayed true after the first
// disconnect and every later close() was a silent no-op (no "closing
// connection" log, no sleep-inhibitor release, no WS close frame).
//
//	Given  a client whose relay connection is killed server-side twice;
//	When   each connection loss runs the reconnect loop;
//	Then   close() fires for BOTH generations — two "closing connection"
//	       log lines — and the client comes back online after each loss.
func TestCloseFiresPerConnectionGeneration(t *testing.T) {
	stub := &wsStub{}
	serverURL := newWSServer(t, stub)

	online := make(chan struct{}, 8)
	var closeCount atomic.Int32
	c := New(&Config{Server: serverURL}, Options{
		OnStatusChange: func(s Status) {
			if s == StatusOnline {
				online <- struct{}{}
			}
		},
		OnLog: func(level, msg string, attrs ...any) {
			if msg == "closing connection" {
				closeCount.Add(1)
			}
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = c.Run(ctx)
	}()
	defer c.Stop()

	waitOnline := func() {
		t.Helper()
		select {
		case <-online:
		case <-time.After(15 * time.Second):
			t.Fatal("client did not come online in time")
		}
	}

	waitOnline() // generation 1

	stub.closeAll()
	waitOnline() // generation 2: only reachable if generation 1 was torn down

	stub.closeAll()
	deadline := time.Now().Add(5 * time.Second)
	for closeCount.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := closeCount.Load(); n < 2 {
		t.Fatalf("close() fired %d times across two reconnects, want 2 — the once-flag was not re-armed", n)
	}
}
