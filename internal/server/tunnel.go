package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mgwn/selftunnel/internal/proto"
)

// Session tuning constants (spec §3.1, §6.1):
//
//	sendQueueCap      frames buffered for the single writer goroutine
//	readDeadlineHello max wait for the hello frame after upgrade
//	readDeadlineIdle  idle deadline, renewed on every frame (touch)
//	pingInterval      server-side keepalive ping cadence
const (
	sendQueueCap      = 256
	readDeadlineHello = 10 * time.Second
	readDeadlineIdle  = 90 * time.Second
	pingInterval      = 25 * time.Second
)

// pendingResp tracks one in-flight relayed request (spec §6.1): ch carries
// the response frames to the consuming relay handler, done is closed by
// UnregisterPending (or session close) so a routeResponse blocked on a full
// ch can abort when the consumer goes away.
type pendingResp struct {
	ch   chan *proto.Frame
	done chan struct{}
}

// Session represents one WebSocket connection from a tunnel client. It is
// created per /ws/tunnel upgrade and runs entirely inside its HTTP handler
// goroutine plus one writer goroutine (Run). Frames are only enqueued to
// sendQ by other components; the writer is the sole sender on the wire.
type Session struct {
	conn       *websocket.Conn
	tun        *Tunnel
	server     *Server
	remoteAddr string
	sendQ      chan []byte
	pending    map[uint32]*pendingResp
	pendingMu  sync.RWMutex
	nextID     atomic.Uint32
	lastRx     atomic.Int64
	closed     atomic.Bool
	helloDone  atomic.Bool
	writeDone  chan struct{}
}

// newSession creates a session for an upgraded connection. The hello
// handshake has not happened yet; tun is bound later by handleHello.
func newSession(conn *websocket.Conn, srv *Server, remoteAddr string) *Session {
	return &Session{
		conn:       conn,
		server:     srv,
		remoteAddr: remoteAddr,
		sendQ:      make(chan []byte, sendQueueCap),
		pending:    make(map[uint32]*pendingResp),
		writeDone:  make(chan struct{}),
	}
}

// Run drives the session until it ends: it starts the writer goroutine,
// reads frames until the connection dies or the idle deadline fires, then
// tears everything down and waits for the writer to finish. Run does not
// return while the tunnel is healthy — callers (handleTunnel) block on it.
func (s *Session) Run() {
	go s.writeLoop()
	s.readLoop()
	s.close()
	<-s.writeDone
}

// readLoop consumes frames until error or deadline (spec §6.1). The first
// frame must be hello within readDeadlineHello; afterwards every received
// frame renews the idle deadline via touch. Response frames are routed to
// their pending relay handlers; everything else is session control.
// readLoop returns on any read error — the caller closes the session.
func (s *Session) readLoop() {
	defer s.close()

	_ = s.conn.SetReadDeadline(time.Now().Add(readDeadlineHello))
	s.conn.SetPongHandler(func(string) error {
		s.touch()
		return nil
	})

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if !s.closed.Load() {
				slog.Debug("tunnel read error", "tunnel", s.tunID(), "err", err)
			}
			return
		}

		s.touch()

		var f proto.Frame
		if err := proto.Unmarshal(data, &f); err != nil {
			continue
		}

		switch f.Type {
		case "hello":
			if s.helloDone.Load() {
				continue
			}
			if f.Version != proto.HelloVersion {
				_ = s.send(&proto.Frame{Type: "hello_ack", Ok: false, Error: "version mismatch"})
				return
			}
			if err := s.server.handleHello(s, &f); err != nil {
				_ = s.send(&proto.Frame{Type: "hello_ack", Ok: false, Error: err.Error()})
				return
			}
			s.helloDone.Store(true)

		case "pong":
			// activity recorded above

		case "set_target_ack":
			// server does not currently wait for ack

		case "stats":
			if f.Target != "" && s.tun != nil {
				s.tun.SetTarget(f.Target)
				if err := s.server.registry.Save(); err != nil {
					slog.Warn("failed to save registry", "err", err)
				}
			}
			if s.tun != nil {
				s.tun.reqCount.Store(f.Forwarded)
			}

		case "response_start", "response_chunk", "response_end":
			s.routeResponse(&f)

		default:
			// Ignore unknown frames for forward compatibility.
		}
	}
}

// writeLoop is the single goroutine that writes to the WebSocket (spec
// §6.1): it drains sendQ, sends a keepalive ping every pingInterval, and
// exits when sendQ is closed (session shutdown) or a write fails. Each
// write gets a 60s deadline — enough for a ~350KB base64 frame on a slow
// link (v5.1: raised from 10s to stop killing slow-tunnel responses).
func (s *Session) writeLoop() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	defer close(s.writeDone)

	for {
		select {
		case b, ok := <-s.sendQ:
			if !ok {
				_ = s.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			_ = s.conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
			if err := s.conn.WriteMessage(websocket.TextMessage, b); err != nil {
				slog.Debug("tunnel write error", "tunnel", s.tunID(), "err", err)
				return
			}

		case <-ticker.C:
			if s.closed.Load() {
				return
			}
			if err := s.send(&proto.Frame{Type: "ping", TS: time.Now().Unix()}); err != nil {
				return
			}
		}
	}
}

// send enqueues a frame with a short blocking timeout — a convenience
// wrapper for control frames that must not stall the caller for long.
func (s *Session) send(f *proto.Frame) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.sendWithContext(ctx, f)
}

// sendWithContext enqueues a frame for the writer goroutine, giving up when
// ctx is done (queue full or caller cancelled). It never writes to the
// connection directly. Callers on the relay path pass the request context
// so a slow client cannot pin a handler past its deadline.
func (s *Session) sendWithContext(ctx context.Context, f *proto.Frame) error {
	if s.closed.Load() {
		return errors.New("session closed")
	}
	b, err := proto.Marshal(f)
	if err != nil {
		return err
	}
	select {
	case s.sendQ <- b:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NextReqID returns the next unused request ID in this session. IDs are
// monotonically increasing uint32 values that skip over IDs still held by
// in-flight requests, so a wrapped counter cannot collide (spec §5).
func (s *Session) NextReqID() uint32 {
	for {
		id := s.nextID.Add(1)
		s.pendingMu.Lock()
		_, inUse := s.pending[id]
		s.pendingMu.Unlock()
		if !inUse {
			return id
		}
	}
}

// RegisterPending marks reqID as in-flight and returns the response
// mailbox for it. Exactly one relay handler may register a given reqID;
// the handler must call UnregisterPending when done.
func (s *Session) RegisterPending(reqID uint32) *pendingResp {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	pr := &pendingResp{
		ch:   make(chan *proto.Frame, 16),
		done: make(chan struct{}),
	}
	s.pending[reqID] = pr
	return pr
}

// UnregisterPending removes the reqID mailbox and closes its done channel,
// aborting any routeResponse still blocked trying to deliver to it.
func (s *Session) UnregisterPending(reqID uint32) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if pr, ok := s.pending[reqID]; ok {
		close(pr.done)
		delete(s.pending, reqID)
	}
}

// routeResponse delivers a response frame to the waiting relay handler
// (spec §6.1, v5.1). It blocks when the consumer is slow instead of
// dropping frames — a dropped chunk would silently truncate a streaming
// (SSE) response; backpressure propagates to the tunnel client via the
// bounded sendQ. Delivery aborts when the consumer unregisters (done) or
// the session closes.
func (s *Session) routeResponse(f *proto.Frame) {
	s.pendingMu.RLock()
	pr, ok := s.pending[f.ReqID]
	s.pendingMu.RUnlock()
	if !ok {
		return
	}
	select {
	case pr.ch <- f:
	case <-pr.done:
	}
}

// close tears the session down exactly once: it closes the connection,
// the send queue (ending the writer) and every pending mailbox (their
// consumers see a nil frame and fail the request with 502), then marks
// the tunnel offline in the registry and server counter.
func (s *Session) close() {
	if s.closed.Swap(true) {
		return
	}
	s.conn.Close()
	close(s.sendQ)

	s.pendingMu.Lock()
	for _, pr := range s.pending {
		close(pr.done)
		close(pr.ch)
	}
	s.pending = make(map[uint32]*pendingResp)
	s.pendingMu.Unlock()

	if s.tun != nil {
		if s.tun.session.CompareAndSwap(s, nil) {
			s.server.onlineCount.Add(-1)
			slog.Info("tunnel offline", "tunnel", s.tun.ID, "client", s.remoteAddr)
		}
	}
}

// touch records receive activity and re-arms the idle read deadline; it is
// called for every incoming frame and pong so an active but quiet tunnel
// (e.g. mid-SSE) is never reaped.
func (s *Session) touch() {
	s.lastRx.Store(time.Now().Unix())
	_ = s.conn.SetReadDeadline(time.Now().Add(readDeadlineIdle))
}

// tunID returns the session's tunnel ID, or "" before the hello handshake
// bound the session to a tunnel. Used in log lines.
func (s *Session) tunID() string {
	if s.tun == nil {
		return ""
	}
	return s.tun.ID
}
