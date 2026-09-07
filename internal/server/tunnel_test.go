package server

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mgwn/selftunnel/internal/proto"
)

// --- session teardown tests (white-box) ------------------------------------
//
// These tests pin the teardown contract that prevents "send on closed
// channel" panics (spec §6.1): sendQ and pendingResp.ch are NEVER closed —
// done is the sole abort signal, so a sender that passed its closed check
// before close() ran can never panic on a channel closed under it. Before
// the fix, close() closed sendQ (panicking blocked senders) and closed the
// pending mailboxes (panicking a routeResponse still delivering).

// sessionTestServer starts an httptest server whose handler upgrades the
// WebSocket and runs a real Session, handing it to the test through the
// returned channel before blocking in Run. The Server pointer is nil —
// these tests never bind a tunnel, so close() never dereferences it.
func sessionTestServer(t *testing.T) (*httptest.Server, <-chan *Session) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	sessCh := make(chan *Session, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		sess := newSession(conn, nil, r.RemoteAddr)
		sessCh <- sess
		sess.Run()
	}))
	t.Cleanup(ts.Close)
	return ts, sessCh
}

// dialSession connects to the session test server and returns the live
// session behind the upgrade.
func dialSession(t *testing.T, ts *httptest.Server, sessCh <-chan *Session) (*websocket.Conn, *Session) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial session test server: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	select {
	case sess := <-sessCh:
		return conn, sess
	case <-time.After(2 * time.Second):
		t.Fatal("session did not upgrade")
		return nil, nil
	}
}

// TestSessionCloseUnblocksBlockedSenders is the regression test for the
// sendQ teardown race: a sender parked on a full queue must be released by
// close() with an error, never panicked by a closed channel — the original
// code closed sendQ in close(), and a sender parked in its select was woken
// into "send on closed channel".
//
//	The peer stops reading and its receive buffer is shrunk, so the
//	writer goroutine stalls once the kernel buffers fill and the queue
//	stays full — the extra sender below is deterministically parked
//	inside its select when close() runs;
//	When   the session closes;
//	Then   the parked sender returns an error promptly, later sends fail
//	       fast, and the writer goroutine has exited.
func TestSessionCloseUnblocksBlockedSenders(t *testing.T) {
	ts, sessCh := sessionTestServer(t)
	conn, sess := dialSession(t, ts, sessCh)

	// Keep the peer from reading and shrink its receive buffer: the
	// writer goroutine then blocks once the kernel buffers fill, the send
	// queue stays full, and a further sender is genuinely parked inside
	// its select when close() runs — the exact interleaving that panics
	// if close() shuts the queue.
	if tc, ok := conn.UnderlyingConn().(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4 * 1024)
	}
	big := base64.StdEncoding.EncodeToString(make([]byte, 4*1024))
	send := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return sess.sendWithContext(ctx, &proto.Frame{Type: "response_chunk", ReqID: 1, Data: big})
	}

	// Fill the queue until the writer stalls and everything sent so far
	// is buffered (len == cap means a further send must park).
	for len(sess.sendQ) < sendQueueCap {
		if err := send(); err != nil {
			t.Fatalf("fill sendQ: %v", err)
		}
	}

	blocked := make(chan error, 1)
	go func() {
		blocked <- send()
	}()

	// Give the sender a moment to run: it cannot complete (the queue is
	// full and nothing drains), so if it has not returned by now it is
	// parked inside its select — the interleaving close() must survive.
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-blocked:
		t.Fatalf("sender completed instead of parking: %v", err)
	default:
	}

	sess.close()

	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("blocked sender must get an error after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not unblock the sender")
	}

	if err := sess.send(&proto.Frame{Type: "ping"}); err == nil {
		t.Fatal("send after close must fail")
	}

	select {
	case <-sess.writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("writer goroutine did not exit after close")
	}
}

// TestSessionCloseAbortsBlockedRouteResponse is the regression test for
// the mailbox teardown race: a routeResponse blocked on a full frame
// channel must abort via done when the session closes — closing ch under
// it (the old behaviour) panicked the sender.
//
//	Given  a pending mailbox whose frame channel has been filled to
//	       capacity and one more routeResponse blocked on it;
//	When   the session closes;
//	Then   the blocked delivery returns promptly and late deliveries for
//	       the cleared registry drop without blocking.
func TestSessionCloseAbortsBlockedRouteResponse(t *testing.T) {
	ts, sessCh := sessionTestServer(t)
	_, sess := dialSession(t, ts, sessCh)

	const reqID = 1
	pr := sess.RegisterPending(reqID)
	defer sess.UnregisterPending(reqID)

	// Fill the mailbox; the next delivery blocks on the full channel.
	for i := 0; i < cap(pr.ch); i++ {
		sess.routeResponse(&proto.Frame{Type: "response_chunk", ReqID: reqID})
	}

	delivered := make(chan struct{})
	go func() {
		sess.routeResponse(&proto.Frame{Type: "response_end", ReqID: reqID})
		close(delivered)
	}()

	sess.close()

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not abort the blocked routeResponse")
	}

	// The registry is cleared: late frames for the reqId drop immediately.
	sess.routeResponse(&proto.Frame{Type: "response_chunk", ReqID: reqID})
}
