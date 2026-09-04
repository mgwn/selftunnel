// Package client implements the selftunnel client (spec §3.5–§3.7): it
// dials out to the relay server, keeps a long-lived WebSocket alive with
// pings, forwards relayed HTTP requests to the configured intranet target
// and streams the responses back as frames. The same Client type powers
// both the CLI (selftunnel-client) and the GUI (selftunnel-client-gui) entry points;
// UI integration happens exclusively through the Options callbacks.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mgwn/selftunnel/internal/proto"
)

// Client tuning constants (spec §3.5, §6.3):
//
//	chunkSize      raw body bytes per request/response chunk frame
//	maxConcurrent  in-flight target requests (semaphore, spec §9.12)
//	pingInterval   keepalive ping cadence; the read deadline is 2× this
//	maxBackoff     reconnect backoff ceiling (1s→2s→…→60s, ±20% jitter)
const (
	chunkSize      = 256 * 1024
	maxConcurrent  = 8
	pingInterval   = 25 * time.Second
	maxBackoff     = 60 * time.Second
	initialBackoff = 1 * time.Second
)

// Config is the client's persisted state, stored as config.json next to the
// binary (spec §6.5). TunnelID and Secret are owned by the client: after a
// successful handshake they are written back so the same ID can be
// reclaimed after a restart. Secret grants ID ownership — never share it.
type Config struct {
	Server   string `json:"server"`
	Target   string `json:"target"`
	CustomID string `json:"customId"`
	TunnelID string `json:"tunnelId"`
	Secret   string `json:"secret"`
}

// Status describes the high-level connection state, surfaced to the UI via
// Options.OnStatusChange.
type Status int32

const (
	StatusDisconnected Status = iota
	StatusConnecting
	StatusOnline
	StatusBackoff
)

// String returns the lowercase status name used in logs and the GUI.
func (s Status) String() string {
	switch s {
	case StatusConnecting:
		return "connecting"
	case StatusOnline:
		return "online"
	case StatusBackoff:
		return "backoff"
	default:
		return "disconnected"
	}
}

// Options configures the client behaviour and optional callbacks.
//
//	ServerInsecure / TargetInsecure  skip TLS verification (debugging only)
//	ClientCert / ClientKey           mTLS client certificate for the target
//	Debug                           emit one debug log per forwarded request
//	OnStatusChange                  called on every status transition
//	OnLog                           called for every log line; level is one
//	                                of "info" / "warn" / "debug" and attrs
//	                                are slog-style key/value pairs
type Options struct {
	ServerInsecure bool
	TargetInsecure bool
	ClientCert     string
	ClientKey      string
	Debug          bool
	OnStatusChange func(Status)
	OnLog          func(level, msg string, attrs ...any)
}

// pendingReq assembles one relayed request on the client side: the
// request_start frame plus the decoded body chunks, keyed by chunk Seq.
// A request without a body is dispatched immediately by handleRequestStart;
// otherwise it waits for request_end. Guarded by Client.pMu.
type pendingReq struct {
	start  *proto.Frame
	chunks map[int][]byte
}

// Client is a portable selftunnel client. Lifecycle: construct with New,
// run with Run (blocking, with reconnect loop), stop with Stop or by
// cancelling the Run context. All exported getters/setters are safe for
// concurrent use; cfg mutations are guarded by mu.
type Client struct {
	cfg        *Config
	configPath string
	opts       Options

	dialer     websocket.Dialer
	httpClient *http.Client

	conn    *websocket.Conn
	sendMu  sync.Mutex
	pending map[uint32]*pendingReq
	pMu     sync.Mutex
	active  map[uint32]context.CancelFunc
	aMu     sync.Mutex
	sem     chan struct{}

	status  atomic.Int32
	tunID   atomic.Value // string
	stop    atomic.Bool
	recon   atomic.Bool
	closed  atomic.Bool
	backoff time.Duration
	mu      sync.RWMutex // guards cfg fields that can change at runtime
}

// New creates a client from an (already loaded/validated) config. opts
// callbacks may be nil. A TLS setup error does not fail construction — the
// client fails fast on Run instead; callers normally validate earlier.
func New(cfg *Config, opts Options) *Client {
	tlsCfg, err := makeTLSConfig(opts.TargetInsecure, opts.ClientCert, opts.ClientKey)
	if err != nil {
		// Return a client that will fail fast on Run; callers normally validate earlier.
		tlsCfg = &tls.Config{InsecureSkipVerify: opts.TargetInsecure}
	}
	serverTLS := &tls.Config{InsecureSkipVerify: opts.ServerInsecure}

	c := &Client{
		cfg:  cfg,
		opts: opts,
		dialer: websocket.Dialer{
			ReadBufferSize:  64 * 1024,
			WriteBufferSize: 64 * 1024,
			TLSClientConfig: serverTLS,
		},
		httpClient: &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
		pending: make(map[uint32]*pendingReq),
		active:  make(map[uint32]context.CancelFunc),
		sem:     make(chan struct{}, maxConcurrent),
		backoff: initialBackoff,
	}
	return c
}

// SetConfigPath sets the path config.json is written to after every
// successful handshake (empty = do not persist). Call before Run.
func (c *Client) SetConfigPath(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.configPath = path
}

// Run blocks until the context is cancelled or Stop is called, driving the
// connect → read → reconnect loop (spec §3.5.5): failures back off
// exponentially (1s→…→60s, ±20% jitter) and retry forever. It returns
// ctx.Err() on context cancellation, nil on Stop, so callers can tell a
// deliberate shutdown from an aborted one.
func (c *Client) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			c.close("shutting down")
			return ctx.Err()
		default:
		}

		c.setStatus(StatusConnecting)
		err := c.connect(ctx)
		if err == nil {
			c.backoff = initialBackoff
			c.setStatus(StatusOnline)
			c.readLoop(ctx)
			c.close("connection lost")
		} else if ctx.Err() == nil && !c.stop.Load() {
			c.log("warn", "connect failed", "err", err)
		}

		if ctx.Err() != nil || c.stop.Load() {
			c.close("stopped")
			return nil
		}
		c.setStatus(StatusBackoff)
		if err := c.sleepBackoff(ctx); err != nil {
			return err
		}
	}
}

// Stop cleanly shuts down the client: it unblocks Run (which returns nil)
// and closes the current connection. Safe to call more than once.
func (c *Client) Stop() {
	c.stop.Store(true)
	c.close("stopped")
}

// Reconnect drops the current connection and reconnects immediately,
// bypassing the backoff delay on the next Run iteration. Used by the GUI
// after configuration changes.
func (c *Client) Reconnect() {
	c.recon.Store(true)
	c.close("reconnect requested")
}

// Status returns the current connection status.
func (c *Client) Status() Status {
	return Status(c.status.Load())
}

// TunnelID returns the allocated tunnel ID (empty if not online).
func (c *Client) TunnelID() string {
	v, _ := c.tunID.Load().(string)
	return v
}

// Target returns the current target URL.
func (c *Client) Target() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.Target
}

// SetTarget updates the target URL for subsequent requests.
func (c *Client) SetTarget(target string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.Target = strings.TrimSpace(target)
}

// Server returns the configured relay server URL.
func (c *Client) Server() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.Server
}

// SetServer updates the relay server URL; takes effect on next reconnect.
func (c *Client) SetServer(server string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.Server = NormalizeServer(server)
}

// CustomID returns the configured desired tunnel ID.
func (c *Client) CustomID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.CustomID
}

// SetCustomID updates the desired tunnel ID; takes effect on next reconnect.
func (c *Client) SetCustomID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.CustomID = strings.ToLower(strings.TrimSpace(id))
}

// setStatus stores the new status and notifies OnStatusChange, if set.
func (c *Client) setStatus(s Status) {
	c.status.Store(int32(s))
	if c.opts.OnStatusChange != nil {
		c.opts.OnStatusChange(s)
	}
}

// log forwards one log line to the OnLog callback, if set; attrs are
// slog-style alternating key/value pairs.
func (c *Client) log(level, msg string, attrs ...any) {
	if c.opts.OnLog != nil {
		c.opts.OnLog(level, msg, attrs...)
	}
}

// debugLog emits a debug-level log only when Debug is enabled — used for
// the per-request debug output (spec: the -debug feature).
func (c *Client) debugLog(msg string, attrs ...any) {
	if c.opts.Debug {
		c.log("debug", msg, attrs...)
	}
}

// connect performs one connection attempt (spec §3.5): dial the server
// WebSocket, send hello with the desired ID and ownership secret, await
// hello_ack, persist the returned tunnelID/secret and engage sleep
// prevention. Any failure returns an error for Run's backoff loop; the
// connection is closed before returning.
func (c *Client) connect(ctx context.Context) error {
	c.mu.RLock()
	server := c.cfg.Server
	desiredID := c.cfg.CustomID
	secret := c.cfg.Secret
	c.mu.RUnlock()

	headers := http.Header{}
	headers.Set("Origin", "native-client://selftunnel")

	conn, resp, err := c.dialer.DialContext(ctx, server, headers)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("%w (status %d)", err, resp.StatusCode)
		}
		return err
	}
	c.conn = conn

	hello := &proto.Frame{
		Type:       "hello",
		Version:    proto.HelloVersion,
		DesiredId:  desiredID,
		Secret:     secret,
		ClientType: "native-client",
		ExtVersion: "1.0.0",
	}
	if err := c.sendFrame(hello); err != nil {
		conn.Close()
		return err
	}

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, data, err := conn.ReadMessage()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return err
	}
	var ack proto.Frame
	if err := proto.Unmarshal(data, &ack); err != nil {
		conn.Close()
		return err
	}
	if ack.Type != "hello_ack" || !ack.Ok {
		conn.Close()
		return fmt.Errorf("hello rejected: %s", ack.Error)
	}

	c.mu.Lock()
	c.cfg.TunnelID = ack.TunnelID
	if ack.Secret != "" {
		c.cfg.Secret = ack.Secret
	}
	if c.configPath != "" {
		if err := SaveConfig(c.configPath, c.cfg); err != nil {
			c.log("warn", "failed to save config", "err", err)
		}
	}
	c.mu.Unlock()
	c.tunID.Store(ack.TunnelID)

	if err := PreventSleep(); err != nil {
		c.log("warn", "failed to prevent system sleep", "err", err)
	}

	c.log("info", "tunnel online", "tunnelID", ack.TunnelID, "reused", ack.IDReused, "desiredDenied", ack.DesiredDenied)
	return nil
}

// readLoop consumes frames from the server until the connection dies, the
// context is cancelled, or nothing arrives for 2× pingInterval (spec
// §6.3). It answers pings, feeds request frames to the dispatch machinery,
// and runs the keepalive ping goroutine — which is bounded by the done
// channel so a reconnect never leaks duplicate pingers.
func (c *Client) readLoop(ctx context.Context) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	// Bounds the ping goroutine to this connection: without it every
	// reconnect would leak a goroutine sending duplicate pings.
	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			select {
			case <-ctx.Done():
				// Close the connection so a pending ReadMessage in the read
				// loop unblocks immediately; otherwise cancellation (SIGINT/
				// SIGTERM) would linger until the next frame arrives.
				c.close("shutting down")
				return
			case <-done:
				return
			case <-ping.C:
				if err := c.sendFrame(&proto.Frame{Type: "ping", TS: time.Now().Unix()}); err != nil {
					c.close("ping failed")
					return
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = c.conn.SetReadDeadline(time.Now().Add(2 * pingInterval))
		_, data, err := c.conn.ReadMessage()
		_ = c.conn.SetReadDeadline(time.Time{})
		if err != nil {
			if ctx.Err() != nil || c.stop.Load() || c.closed.Load() {
				return
			}
			c.debugLog("read error", "err", err)
			return
		}

		var f proto.Frame
		if err := proto.Unmarshal(data, &f); err != nil {
			continue
		}

		switch f.Type {
		case "ping":
			_ = c.sendFrame(&proto.Frame{Type: "pong", TS: f.TS})
		case "request_start":
			c.handleRequestStart(&f)
		case "request_chunk":
			c.handleRequestChunk(&f)
		case "request_end":
			c.handleRequestEnd(&f)
		case "cancel":
			c.handleCancel(f.ReqID)
		case "set_target":
			if IsValidTarget(f.Target) {
				c.SetTarget(f.Target)
			}
			_ = c.sendFrame(&proto.Frame{Type: "set_target_ack", Ok: IsValidTarget(f.Target)})
		case "get_stats":
			_ = c.sendFrame(&proto.Frame{Type: "stats", Forwarded: 0, Target: c.Target()})
		default:
			// ignore unknown frames
		}
	}
}

// handleRequestStart registers a new in-flight request. Body-less requests
// are dispatched to the target immediately; otherwise chunks accumulate
// until request_end arrives (handleRequestEnd dispatches).
func (c *Client) handleRequestStart(f *proto.Frame) {
	c.pMu.Lock()
	c.pending[f.ReqID] = &pendingReq{
		start:  f,
		chunks: make(map[int][]byte),
	}
	c.pMu.Unlock()

	if !f.HasBody {
		c.dispatch(f.ReqID)
	}
}

// handleRequestChunk decodes one base64 body chunk into the pending
// request. Chunks for unknown reqIDs and undecodable payloads are dropped.
func (c *Client) handleRequestChunk(f *proto.Frame) {
	c.pMu.Lock()
	p, ok := c.pending[f.ReqID]
	c.pMu.Unlock()
	if !ok {
		return
	}
	data, err := base64.StdEncoding.DecodeString(f.Data)
	if err != nil {
		return
	}
	p.chunks[f.Seq] = data
}

// handleRequestEnd marks a request complete and dispatches it to the
// target. Unknown reqIDs are ignored (e.g. already cancelled).
func (c *Client) handleRequestEnd(f *proto.Frame) {
	c.pMu.Lock()
	_, ok := c.pending[f.ReqID]
	c.pMu.Unlock()
	if !ok {
		return
	}
	c.dispatch(f.ReqID)
}

// dispatch assembles the accumulated body chunks of reqID (in Seq order)
// and hands the request to forward in a new goroutine, releasing the read
// loop immediately.
func (c *Client) dispatch(reqID uint32) {
	c.pMu.Lock()
	p, ok := c.pending[reqID]
	if !ok {
		c.pMu.Unlock()
		return
	}
	delete(c.pending, reqID)
	c.pMu.Unlock()

	var body []byte
	// Chunk seq starts at 0 and increments contiguously (protocol guarantee);
	// assembling in index order is sufficient.
	for i := 0; i < len(p.chunks); i++ {
		body = append(body, p.chunks[i]...)
	}

	go c.forward(reqID, p.start, body)
}

// forward executes one relayed request against the target (spec §6.3):
// it takes a semaphore slot (maxConcurrent), rebuilds the HTTP request
// (method, path, query, headers — Host becomes req.Host), streams the
// target's response back as response_start/chunks/end, and answers 502 via
// sendError when the target is unreachable or the request cannot be built.
// Per-request debug lines are emitted when Options.Debug is set.
func (c *Client) forward(reqID uint32, start *proto.Frame, body []byte) {
	c.sem <- struct{}{}
	defer func() { <-c.sem }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c.aMu.Lock()
	c.active[reqID] = cancel
	c.aMu.Unlock()
	defer func() {
		c.aMu.Lock()
		delete(c.active, reqID)
		c.aMu.Unlock()
	}()

	started := time.Now()
	target := strings.TrimSuffix(c.Target(), "/")
	if !IsValidTarget(target) {
		c.debugLog("request failed", "reqId", reqID, "method", start.Method, "err", "no target configured")
		c.sendError(reqID, errors.New("no target configured"))
		return
	}

	path := start.Path
	if path == "" {
		path = "/"
	}
	url := target + path
	if start.Query != "" {
		url = url + "?" + start.Query
	}

	c.debugLog("forwarding request", "reqId", reqID, "method", start.Method, "url", url, "bodyBytes", len(body))

	req, err := http.NewRequestWithContext(ctx, start.Method, url, bytes.NewReader(body))
	if err != nil {
		c.debugLog("request failed", "reqId", reqID, "method", start.Method, "url", url, "err", err)
		c.sendError(reqID, err)
		return
	}

	for k, vv := range start.Headers {
		if strings.EqualFold(k, "Host") && len(vv) > 0 {
			req.Host = vv[0]
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.debugLog("request failed", "reqId", reqID, "method", start.Method, "url", url, "err", err, "duration", time.Since(started))
		c.sendError(reqID, err)
		return
	}
	defer resp.Body.Close()

	headers := make(map[string][]string)
	for k, vv := range resp.Header {
		headers[k] = vv
	}
	if err := c.sendFrame(&proto.Frame{Type: "response_start", ReqID: reqID, Status: resp.StatusCode, Headers: headers}); err != nil {
		return
	}

	buf := make([]byte, chunkSize)
	seq := 0
	sent := int64(0)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			sent += int64(n)
			if err2 := c.sendFrame(&proto.Frame{
				Type:  "response_chunk",
				ReqID: reqID,
				Seq:   seq,
				Data:  base64.StdEncoding.EncodeToString(chunk),
			}); err2 != nil {
				return
			}
			seq++
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			c.debugLog("response aborted", "reqId", reqID, "status", resp.StatusCode, "respBytes", sent, "err", err)
			_ = c.sendFrame(&proto.Frame{Type: "response_end", ReqID: reqID, Error: err.Error()})
			return
		}
	}
	_ = c.sendFrame(&proto.Frame{Type: "response_end", ReqID: reqID})
	c.debugLog("request done", "reqId", reqID, "method", start.Method, "url", url, "status", resp.StatusCode, "respBytes", sent, "duration", time.Since(started))
}

// sendError answers a relayed request with a synthetic 502 response
// (empty headers + response_end carrying the error), used whenever the
// target request could not be executed.
func (c *Client) sendError(reqID uint32, err error) {
	_ = c.sendFrame(&proto.Frame{Type: "response_start", ReqID: reqID, Status: http.StatusBadGateway, Headers: map[string][]string{}})
	_ = c.sendFrame(&proto.Frame{Type: "response_end", ReqID: reqID, Error: err.Error()})
}

// handleCancel implements the cancel frame (spec §5): it drops any pending
// body assembly for reqID and cancels an in-flight target request via its
// registered context cancel function.
func (c *Client) handleCancel(reqID uint32) {
	c.pMu.Lock()
	delete(c.pending, reqID)
	c.pMu.Unlock()

	c.aMu.Lock()
	cancel := c.active[reqID]
	delete(c.active, reqID)
	c.aMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// sendFrame serializes and writes one frame to the WebSocket. A single
// mutex serializes writers (the read loop, ping goroutine and forward
// goroutines all send); each write gets a 60s deadline — a 256KB chunk is
// ~350KB after base64, which can exceed 10s on slow links (v5.1).
func (c *Client) sendFrame(f *proto.Frame) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.conn == nil {
		return errors.New("not connected")
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
	return c.conn.WriteJSON(f)
}

// close tears the current connection down exactly once: it releases the
// sleep inhibitor, sends a best-effort WebSocket close and closes the
// socket. The reconnect loop in Run decides what happens next.
func (c *Client) close(reason string) {
	if c.closed.Swap(true) {
		return
	}
	if err := AllowSleep(); err != nil {
		c.log("warn", "failed to allow system sleep", "err", err)
	}
	c.log("info", "closing connection", "reason", reason)
	if c.conn != nil {
		_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(5*time.Second))
		c.conn.Close()
	}
}

// sleepBackoff waits before the next reconnect attempt: exponential
// doubling capped at maxBackoff with ±20% jitter (spec §3.5.5). A pending
// Reconnect request skips the wait. It returns ctx.Err() if the context
// is cancelled while waiting.
func (c *Client) sleepBackoff(ctx context.Context) error {
	if c.recon.Load() {
		c.recon.Store(false)
		return nil
	}
	c.backoff = min(c.backoff*2, maxBackoff)
	// ±20% jitter
	delay := c.backoff - c.backoff/5 + time.Duration(rand.Int63n(int64(c.backoff/5*2+1)))
	c.log("info", "reconnecting", "delay", delay)
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// LoadConfig loads the config file at path; a missing file yields an empty
// config rather than an error (first run). Malformed JSON is an error.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SaveConfig writes cfg atomically (temp file + rename) with 0600
// permissions — the file carries the ownership secret.
func SaveConfig(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NormalizeServer converts https/http/host input into the canonical
// wss://host/ws/tunnel WebSocket URL (spec §3.5.3). An empty input stays
// empty.
func NormalizeServer(url string) string {
	s := strings.TrimSpace(url)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "https://") {
		s = "wss://" + s[8:]
	} else if strings.HasPrefix(s, "http://") {
		s = "ws://" + s[7:]
	} else if !strings.HasPrefix(s, "wss://") && !strings.HasPrefix(s, "ws://") {
		s = "wss://" + s
	}
	s = strings.TrimSuffix(s, "/")
	if !strings.HasSuffix(s, "/ws/tunnel") {
		s = s + "/ws/tunnel"
	}
	return s
}

// IsValidTarget reports whether t looks like an http(s) URL — the
// documented contract is a prefix check; full URL validity is enforced
// later by net/http when the request is built.
func IsValidTarget(t string) bool {
	return strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://")
}

// IsValidCustomID reports whether id is 8 lowercase alnum characters,
// matching the server's tunnelID format (spec §3.1).
func IsValidCustomID(id string) bool {
	if len(id) != 8 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// makeTLSConfig builds the TLS configuration for the target connection:
// optional InsecureSkipVerify (-insecure) and an optional client
// certificate for mTLS. The relay connection uses a separate, simpler
// config (see New).
func makeTLSConfig(insecure bool, certFile, keyFile string) (*tls.Config, error) {
	cfg := &tls.Config{InsecureSkipVerify: insecure}
	if certFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}
