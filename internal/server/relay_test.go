package server

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mgwn/selftunnel/internal/proto"
)

// --- relay handler integration tests (white-box) ---------------------------
//
// These tests drive handleRelay through a real httptest server with a
// scripted WebSocket tunnel peer, so the two-stage timeout and session-
// teardown paths (spec §3.2 step 4) are exercised without waiting the
// production 120s/300s: relayTimeout/streamIdleTimeout are package vars
// lowered per test (the tests never run t.Parallel, so the mutation is
// safe; withRelayTimeouts restores the production values on cleanup).

// testHTTPClient bounds every relay request in this file so a regression
// that stalls the handler fails the test instead of hanging it.
var testHTTPClient = &http.Client{Timeout: 5 * time.Second}

// withRelayTimeouts installs per-test values for the two-stage timeout
// (spec §3.2 step 4) and restores the production values on cleanup.
func withRelayTimeouts(t *testing.T, firstFrame, idle time.Duration) {
	t.Helper()
	oldFirst, oldIdle := relayTimeout, streamIdleTimeout
	relayTimeout, streamIdleTimeout = firstFrame, idle
	t.Cleanup(func() { relayTimeout, streamIdleTimeout = oldFirst, oldIdle })
}

// relayEnv is a relay test harness: a real server plus one scripted
// WebSocket tunnel peer (an online session bound to a fresh tunnel ID).
type relayEnv struct {
	ts   *httptest.Server
	peer *websocket.Conn
	id   string
}

// newRelayEnv starts the server and connects a tunnel peer through the
// real /ws/tunnel upgrade and hello handshake.
func newRelayEnv(t *testing.T) *relayEnv {
	t.Helper()
	srv := New(NewRegistry(t.TempDir()), ":0", []string{"*"}, false)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	hdr := http.Header{"Origin": []string{"native-client://test"}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/tunnel", hdr)
	if err != nil {
		t.Fatalf("dial /ws/tunnel: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	writeFrame(t, conn, &proto.Frame{Type: "hello", Version: proto.HelloVersion, ClientType: "test"})
	ack := readFrame(t, conn)
	if !ack.Ok {
		t.Fatalf("hello rejected: %s", ack.Error)
	}
	return &relayEnv{ts: ts, peer: conn, id: ack.TunnelID}
}

// awaitRequestStart reads the request_start for the next relayed request.
func (e *relayEnv) awaitRequestStart(t *testing.T) proto.Frame {
	t.Helper()
	f := readFrame(t, e.peer)
	if f.Type != "request_start" {
		t.Fatalf("first frame = %q, want request_start", f.Type)
	}
	return f
}

// awaitCancel reads frames until a cancel for reqID arrives (spec §3.2
// step 4 requires the client to be told to stop working on the relay).
func (e *relayEnv) awaitCancel(t *testing.T, reqID uint32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f := readFrame(t, e.peer)
		if f.Type == "cancel" && f.ReqID == reqID {
			return
		}
	}
	t.Fatalf("no cancel frame for reqId %d", reqID)
}

func writeFrame(t *testing.T, c *websocket.Conn, f *proto.Frame) {
	t.Helper()
	if err := c.WriteJSON(f); err != nil {
		t.Fatalf("write frame %s: %v", f.Type, err)
	}
}

func readFrame(t *testing.T, c *websocket.Conn) proto.Frame {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var f proto.Frame
	if err := proto.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return f
}

// TestRelayResponseStartTimeout verifies spec §3.2 step 4 (stage one): no
// response_start within relayTimeout → the caller gets 504 and the tunnel
// client receives a cancel frame for that request.
//
//	Given  a tunnel peer that acknowledges the request but never answers;
//	When   the first-frame timeout expires;
//	Then   the caller gets 504 and the peer receives cancel with the reqId.
func TestRelayResponseStartTimeout(t *testing.T) {
	withRelayTimeouts(t, 200*time.Millisecond, 300*time.Second)
	env := newRelayEnv(t)

	resCh := make(chan *http.Response, 1)
	go func() {
		resp, err := testHTTPClient.Get(env.ts.URL + "/t/" + env.id + "/slow")
		if err != nil {
			t.Errorf("GET /slow: %v", err)
			resCh <- nil
			return
		}
		resCh <- resp
	}()

	start := env.awaitRequestStart(t)
	resp := <-resCh
	if resp == nil {
		t.Fatal("no response")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	env.awaitCancel(t, start.ReqID)
}

// TestRelayStreamIdleTimeout verifies spec §3.2 step 4 (stage two): the
// streaming phase has no total-duration cap, but streamIdleTimeout of
// silence is cut — the caller's connection is truncated and the client
// receives cancel.
//
//	Given  a peer that sends one chunk and then goes silent;
//	When   the idle timeout expires;
//	Then   the caller received the chunk, the body then fails mid-read, and
//	       the peer receives cancel.
func TestRelayStreamIdleTimeout(t *testing.T) {
	withRelayTimeouts(t, 5*time.Second, 200*time.Millisecond)
	env := newRelayEnv(t)

	resCh := make(chan *http.Response, 1)
	go func() {
		resp, err := testHTTPClient.Get(env.ts.URL + "/t/" + env.id + "/stream")
		if err != nil {
			t.Errorf("GET /stream: %v", err)
			resCh <- nil
			return
		}
		resCh <- resp
	}()

	start := env.awaitRequestStart(t)
	writeFrame(t, env.peer, &proto.Frame{Type: "response_start", ReqID: start.ReqID, Status: 200,
		Headers: map[string][]string{"Content-Type": {"text/plain"}}})
	writeFrame(t, env.peer, &proto.Frame{Type: "response_chunk", ReqID: start.ReqID, Seq: 0,
		Data: base64.StdEncoding.EncodeToString([]byte("hello-"))})

	resp := <-resCh
	if resp == nil {
		t.Fatal("no response")
	}
	defer resp.Body.Close()

	// The first chunk must have reached the caller before the cut.
	var got []byte
	buf := make([]byte, 64)
	for len(got) < len("hello-") {
		n, err := resp.Body.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			t.Fatalf("stream ended before the first chunk: %v", err)
		}
	}
	if string(got) != "hello-" {
		t.Fatalf("body = %q, want %q", got, "hello-")
	}

	// After the idle timeout the truncated body must surface a read error
	// (not a clean EOF — the caller must be able to tell the stream broke).
	var readErr error
	for readErr == nil {
		_, readErr = resp.Body.Read(buf)
	}
	if readErr == io.EOF {
		t.Fatalf("truncated stream must not end with a clean EOF, got %v", readErr)
	}

	env.awaitCancel(t, start.ReqID)
}

// TestRelayFirstFrameResponseEnd verifies spec §3.2 step 3: a response_end
// arriving as the FIRST frame (target failed before response_start) yields
// 502 to the caller, carrying the error, since no status was written yet.
//
//	Given  a peer that answers a request_end-style failure immediately;
//	When   the relay handler receives it as the first frame;
//	Then   the caller gets 502 with the target's error message.
func TestRelayFirstFrameResponseEnd(t *testing.T) {
	env := newRelayEnv(t)

	resCh := make(chan *http.Response, 1)
	go func() {
		resp, err := testHTTPClient.Get(env.ts.URL + "/t/" + env.id + "/fail")
		if err != nil {
			t.Errorf("GET /fail: %v", err)
			resCh <- nil
			return
		}
		resCh <- resp
	}()

	start := env.awaitRequestStart(t)
	writeFrame(t, env.peer, &proto.Frame{Type: "response_end", ReqID: start.ReqID, Error: "boom"})

	resp := <-resCh
	if resp == nil {
		t.Fatal("no response")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "boom") {
		t.Fatalf("body = %q, want it to carry the target error", body)
	}
}

// TestRelaySessionDropMidStream verifies the pr.done teardown path: when
// the tunnel session dies while a response is streaming, the handler ends
// the caller's response cleanly instead of hanging.
//
//	Given  an open stream with one flushed chunk;
//	When   the tunnel peer disconnects abruptly;
//	Then   the caller has already received the chunk and the body then ends
//	       cleanly (no error, no further data).
func TestRelaySessionDropMidStream(t *testing.T) {
	env := newRelayEnv(t)

	resCh := make(chan *http.Response, 1)
	go func() {
		resp, err := testHTTPClient.Get(env.ts.URL + "/t/" + env.id + "/stream")
		if err != nil {
			t.Errorf("GET /stream: %v", err)
			resCh <- nil
			return
		}
		resCh <- resp
	}()

	start := env.awaitRequestStart(t)
	writeFrame(t, env.peer, &proto.Frame{Type: "response_start", ReqID: start.ReqID, Status: 200,
		Headers: map[string][]string{"Content-Type": {"text/plain"}}})
	writeFrame(t, env.peer, &proto.Frame{Type: "response_chunk", ReqID: start.ReqID, Seq: 0,
		Data: base64.StdEncoding.EncodeToString([]byte("hello"))})

	resp := <-resCh
	if resp == nil {
		t.Fatal("no response")
	}
	defer resp.Body.Close()

	// Read the flushed chunk first: once it has been read, the relay
	// handler must have consumed it, so dropping the peer cannot race the
	// handler's select between pr.ch and pr.done.
	first := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if string(first) != "hello" {
		t.Fatalf("first chunk = %q, want %q", first, "hello")
	}

	env.peer.Close()

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("body after tunnel drop: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("body after tunnel drop = %q, want empty", rest)
	}
}

// TestRelaySlowUploadExemptFromFirstFrameTimeout verifies that relayTimeout
// constrains only the wait for the first RESPONSE frame (spec §3.2 step 4):
// a request body dribbled in slower than relayTimeout must still relay.
//
//	Given  a first-frame budget of 200ms and an upload dribbled over 400ms;
//	When   the request finally completes and the peer answers promptly;
//	Then   the caller gets the peer's 200 — the upload phase was not cut.
func TestRelaySlowUploadExemptFromFirstFrameTimeout(t *testing.T) {
	withRelayTimeouts(t, 200*time.Millisecond, 300*time.Second)
	env := newRelayEnv(t)

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("slow-"))
		time.Sleep(400 * time.Millisecond) // 2× relayTimeout of silence
		_, _ = pw.Write([]byte("payload"))
		_ = pw.Close()
	}()

	resCh := make(chan *http.Response, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, env.ts.URL+"/t/"+env.id+"/upload", pr)
		if err != nil {
			t.Errorf("build request: %v", err)
			resCh <- nil
			return
		}
		resp, err := testHTTPClient.Do(req)
		if err != nil {
			t.Errorf("POST /upload: %v", err)
			resCh <- nil
			return
		}
		resCh <- resp
	}()

	start := env.awaitRequestStart(t)
	if !start.HasBody {
		t.Fatal("request_start must carry hasBody for a POST with a body")
	}

	var body []byte
	for {
		f := readFrame(t, env.peer)
		switch f.Type {
		case "request_chunk":
			chunk, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil {
				t.Fatalf("decode chunk: %v", err)
			}
			body = append(body, chunk...)
		case "request_end":
			if string(body) != "slow-payload" {
				t.Fatalf("uploaded body = %q, want %q", body, "slow-payload")
			}
			writeFrame(t, env.peer, &proto.Frame{Type: "response_start", ReqID: start.ReqID, Status: 200})
			writeFrame(t, env.peer, &proto.Frame{Type: "response_end", ReqID: start.ReqID})
			resp := <-resCh
			if resp == nil {
				t.Fatal("no response")
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			return
		default:
			t.Fatalf("unexpected frame %q while reading the upload", f.Type)
		}
	}
}

// TestSplitTunnelPath verifies the /t/{id}/rest URL parsing that feeds the
// relay handler (spec §3.2): the tunnel ID and the target path are split
// at the first path segment, "/" when no rest follows.
//
//	Given  relay paths with and without a trailing rest;
//	When   splitTunnelPath runs;
//	Then   id and rest match the table.
func TestSplitTunnelPath(t *testing.T) {
	cases := []struct {
		in   string
		id   string
		rest string
	}{
		{"/t/abcd1234", "abcd1234", "/"},
		{"/t/abcd1234/", "abcd1234", "/"},
		{"/t/abcd1234/api/users", "abcd1234", "/api/users"},
		{"/t/abcd1234/api/users/", "abcd1234", "/api/users/"},
	}
	for _, c := range cases {
		id, rest := splitTunnelPath(c.in)
		if id != c.id || rest != c.rest {
			t.Errorf("splitTunnelPath(%q) = (%q, %q), want (%q, %q)", c.in, id, rest, c.id, c.rest)
		}
	}
}

// TestValidTunnelID verifies the tunnelID format gate (spec §3.2): only
// exactly-8-character [a-z0-9] IDs are accepted.
//
//	Given  candidate IDs of varying case, length and charset;
//	When   validTunnelID runs;
//	Then   only the canonical ones pass.
func TestValidTunnelID(t *testing.T) {
	cases := map[string]bool{
		"abcd1234":  true,
		"ABCD1234":  false,
		"abcd123":   false,
		"abcd12345": false,
		"abcd-123":  false,
		"":          false,
	}
	for id, want := range cases {
		if got := validTunnelID(id); got != want {
			t.Errorf("validTunnelID(%q) = %v, want %v", id, got, want)
		}
	}
}

// TestFilterHeadersStripsHopByHop verifies spec §3.2 step 2: hop-by-hop
// headers never reach the target, while end-to-end headers (auth, cookies,
// custom, multi-value) pass through untouched.
//
//	Given  a request header set mixing hop-by-hop and end-to-end headers;
//	When   filterHeaders runs;
//	Then   hop-by-hop names are gone and the rest survive with all values.
func TestFilterHeadersStripsHopByHop(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "keep-alive")
	h.Set("Upgrade", "websocket")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("Proxy-Authorization", "Basic x")
	h.Set("Transfer-Encoding", "chunked")
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer tok")
	h.Add("X-Custom", "a")
	h.Add("X-Custom", "b")

	out := filterHeaders(h)
	for _, banned := range []string{"Connection", "Upgrade", "Keep-Alive", "Proxy-Authorization", "Transfer-Encoding"} {
		if _, ok := out[banned]; ok {
			t.Errorf("hop-by-hop header %q must be stripped", banned)
		}
	}
	if out["Content-Type"][0] != "application/json" || out["Authorization"][0] != "Bearer tok" {
		t.Error("end-to-end headers must pass through")
	}
	if len(out["X-Custom"]) != 2 {
		t.Error("multi-value headers must pass through with all values")
	}
}

// TestWriteResponseHeaders verifies spec §3.2 step 3: response headers are
// applied with hop-by-hop headers and Content-Length stripped (the body is
// re-chunked by the relay).
//
//	Given  a response_start header map with Content-Length, Connection and
//	       a custom header;
//	When   writeResponseHeaders applies them;
//	Then   the status and custom header are set, Content-Length is not.
func TestWriteResponseHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	headers := map[string][]string{
		"Content-Length": {"9999"},
		"Connection":     {"close"},
		"X-Custom":       {"yes"},
	}
	writeResponseHeaders(rec, 201, headers)
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if rec.Header().Get("X-Custom") != "yes" {
		t.Error("custom response headers must pass through")
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Error("Content-Length must be stripped (chunked streaming)")
	}
}

// TestWriteResponseHeadersZeroStatusBecomes502 verifies the defensive
// default: a missing status code surfaces as 502, never as a 200.
//
//	Given  a response frame with status 0;
//	When   writeResponseHeaders applies it;
//	Then   the written status is 502.
func TestWriteResponseHeadersZeroStatusBecomes502(t *testing.T) {
	rec := httptest.NewRecorder()
	writeResponseHeaders(rec, 0, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

// TestCheckOrigin verifies the WebSocket origin gate (spec §3.1 step 1)
// with the default native-client:// prefix: only matching prefixes (and
// non-empty origins) pass.
//
//	Given  a server allowing the native-client:// prefix;
//	When   handshakes arrive with various Origin headers;
//	Then   only the prefix matches (and wildcards) are accepted.
func TestCheckOrigin(t *testing.T) {
	s := New(NewRegistry(t.TempDir()), ":0", []string{"native-client://"}, false)
	cases := map[string]bool{
		"":                           false,
		"native-client://selftunnel": true,
		"native-client://anything":   true,
		"https://evil.example.com":   false,
	}
	for origin, want := range cases {
		r := httptest.NewRequest("GET", "/ws/tunnel", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if got := s.checkOrigin(r); got != want {
			t.Errorf("checkOrigin(%q) = %v, want %v", origin, got, want)
		}
	}
}

// TestCheckOriginWildcard verifies the "*" escape hatch (spec §4,
// -allowed-origins): any origin is accepted when wildcard is configured.
//
//	Given  a server configured with allowedOrigins ["*"];
//	When   a handshake arrives with an arbitrary origin;
//	Then   it is accepted.
func TestCheckOriginWildcard(t *testing.T) {
	s := New(NewRegistry(t.TempDir()), ":0", []string{"*"}, false)
	r := httptest.NewRequest("GET", "/ws/tunnel", nil)
	r.Header.Set("Origin", "https://anything.example.com")
	if !s.checkOrigin(r) {
		t.Error("wildcard * must accept any origin")
	}
}
