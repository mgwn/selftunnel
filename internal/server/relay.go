package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mgwn/selftunnel/internal/proto"
)

// relayTimeout bounds the wait for the first response frame (spec §3.2
// step 4): if response_start does not arrive within this window, the caller
// gets 504 and the client receives cancel. It applies to the first frame
// ONLY — the streaming phase afterwards uses streamIdleTimeout and has no
// total-duration cap.
//
// Both values are package vars (not consts) so white-box tests can lower
// them; production code must treat them as immutable.
var relayTimeout = 120 * time.Second

// streamIdleTimeout caps a stalled streaming response (spec §3.2 step 4):
// it is reset by every received response frame, so an actively producing
// stream (SSE/LLM replies) never times out; only silence this long is cut —
// the client receives cancel and the caller's connection is truncated.
var streamIdleTimeout = 300 * time.Second

// chunkSize is the raw body size per request_chunk/response_chunk frame,
// before base64 encoding (spec §3.2 step 2).
const chunkSize = 256 * 1024

// hopByHopHeaders lists the header names that must never be forwarded
// between the caller and the target (RFC 9110 hop-by-hop set). They are
// stripped in both directions; everything else passes through unchanged.
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// handleRelay serves ANY /t/{tunnelID}/* (spec §3.2): it converts one HTTP
// request into request_* frames for the tunnel's online session and streams
// the response_* frames back to the caller, flushing per chunk so SSE works.
//
// Error mapping: unknown/malformed tunnelID → 404; tunnel offline or the
// session dies mid-relay → 502; no response_start within relayTimeout →
// 504; a stream silent for streamIdleTimeout → cancel + truncated
// connection. Failure is always fast — the handler never queues and waits
// for a client to come back.
func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	id, path := splitTunnelPath(r.URL.Path)
	if !validTunnelID(id) {
		http.NotFound(w, r)
		return
	}

	tun := s.registry.Get(id)
	if tun == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "tunnel not found"})
		return
	}

	sess := tun.Session()
	if sess == nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "tunnel offline"})
		return
	}

	reqID := sess.NextReqID()
	pr := sess.RegisterPending(reqID)
	defer sess.UnregisterPending(reqID)

	if r.Body != nil {
		defer r.Body.Close()
	}

	hasBody := r.Body != nil && r.Body != http.NoBody
	start := &proto.Frame{
		Type:    "request_start",
		ReqID:   reqID,
		Method:  r.Method,
		Path:    path,
		Query:   r.URL.RawQuery,
		Headers: filterHeaders(r.Header),
		HasBody: hasBody,
	}
	// The upload phase carries no relayTimeout: only the wait for the
	// first response frame is capped (spec §3.2 step 4); a slow caller may
	// take as long as it likes to stream its request body up.
	if err := sess.sendWithContext(r.Context(), start); err != nil {
		maybeSendCancel(sess, reqID)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "tunnel unreachable"})
		return
	}
	tun.reqCount.Add(1)

	if hasBody {
		if err := streamBody(r.Context(), sess, reqID, r.Body); err != nil {
			maybeSendCancel(sess, reqID)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to stream request body"})
			return
		}
	}

	flusher, _ := w.(http.Flusher)

	// Stage one (spec §3.2 step 4): wait for the first response frame,
	// capped by relayTimeout. pr.done fires when the session tears down
	// (its mailbox closes, so pr.ch never delivers again).
	waitCtx, cancel := context.WithTimeout(r.Context(), relayTimeout)
	defer cancel()

	select {
	case f := <-pr.ch:
		if f.Type == "response_end" {
			// The target failed before any response_start (spec §3.2
			// step 3): the status has not been written, so the error
			// can still surface as a 502.
			msg := f.Error
			if msg == "" {
				msg = "request failed"
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": msg})
			return
		}
		if f.Type != "response_start" {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "unexpected frame"})
			return
		}
		writeResponseHeaders(w, f.Status, f.Headers)
		if flusher != nil {
			flusher.Flush()
		}

	case <-pr.done:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "tunnel offline"})
		return

	case <-waitCtx.Done():
		maybeSendCancel(sess, reqID)
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": "request timeout"})
		return
	}

	// Stage two (spec §3.2 step 4): stream chunks until response_end with
	// NO total-duration cap — only streamIdleTimeout of silence is cut,
	// and every received frame resets the idle timer.
	idle := time.NewTimer(streamIdleTimeout)
	defer idle.Stop()

	for {
		select {
		case f := <-pr.ch:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(streamIdleTimeout)
			switch f.Type {
			case "response_chunk":
				data, err := base64.StdEncoding.DecodeString(f.Data)
				if err != nil {
					slog.Warn("bad response chunk", "reqId", reqID, "err", err)
					return
				}
				w.Write(data)
				if flusher != nil {
					flusher.Flush()
				}
			case "response_end":
				if f.Error != "" {
					slog.Warn("response end error", "reqId", reqID, "err", f.Error)
				}
				return
			default:
				return
			}

		case <-pr.done:
			slog.Warn("tunnel dropped mid-stream", "tunnel", id, "reqId", reqID)
			return

		case <-idle.C:
			maybeSendCancel(sess, reqID)
			slog.Warn("stream idle timeout", "tunnel", id, "reqId", reqID, "idle", streamIdleTimeout)
			truncateResponse(w)
			return

		case <-r.Context().Done():
			maybeSendCancel(sess, reqID)
			return
		}
	}
}

// splitTunnelPath splits "/t/{id}/rest" into the tunnel ID and the target
// path ("/" if absent). It performs no validation; pair with validTunnelID.
func splitTunnelPath(path string) (id, rest string) {
	rest = strings.TrimPrefix(path, "/t/")
	parts := strings.SplitN(rest, "/", 2)
	id = parts[0]
	if len(parts) == 2 {
		rest = "/" + parts[1]
	} else {
		rest = "/"
	}
	return
}

// validTunnelID reports whether id matches the canonical tunnelID format
// (idRe: 8 characters from [a-z0-9]).
func validTunnelID(id string) bool {
	return idRe.MatchString(id)
}

// filterHeaders copies h without the hop-by-hop headers; all other headers
// (including Cookie, Authorization and custom ones) pass through unchanged.
func filterHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string)
	for k, vv := range h {
		if hopByHopHeaders[strings.ToLower(k)] {
			continue
		}
		out[k] = vv
	}
	return out
}

// writeResponseHeaders applies a response_start frame to w: hop-by-hop
// headers and Content-Length are stripped (the body is chunk-streamed), a
// zero status is normalized to 502, then the status is written.
func writeResponseHeaders(w http.ResponseWriter, status int, headers map[string][]string) {
	if status == 0 {
		status = http.StatusBadGateway
	}
	for k, vv := range headers {
		if hopByHopHeaders[strings.ToLower(k)] || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
}

// streamBody reads the caller's request body and forwards it as
// request_chunk frames (chunkSize each) followed by one request_end frame
// (spec §3.2 step 2). It returns on the first send failure or read error;
// the caller decides the error response.
func streamBody(ctx context.Context, sess *Session, reqID uint32, body io.ReadCloser) error {
	buf := make([]byte, chunkSize)
	seq := 0
	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			f := &proto.Frame{
				Type:  "request_chunk",
				ReqID: reqID,
				Seq:   seq,
				Data:  base64.StdEncoding.EncodeToString(chunk),
			}
			if err2 := sess.sendWithContext(ctx, f); err2 != nil {
				return err2
			}
			seq++
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return sess.sendWithContext(ctx, &proto.Frame{Type: "request_end", ReqID: reqID})
}

// maybeSendCancel best-effort sends a cancel frame for reqID so the client
// stops working on a relay the caller has already given up on (spec §5
// cancel semantics). Errors are ignored — the session may be gone.
func maybeSendCancel(sess *Session, reqID uint32) {
	_ = sess.send(&proto.Frame{Type: "cancel", ReqID: reqID})
}

// truncateResponse forcibly ends a chunked response that is being cut short
// (spec §3.2 step 4, streaming idle timeout): merely returning from the
// handler would send the terminating zero-length chunk and make the
// truncated body look complete to the caller, so the raw connection is
// hijacked and closed instead. When hijacking is unavailable (HTTP/2), it
// falls back to a plain return.
func truncateResponse(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}

// writeJSON writes a JSON error body with the given status; used for all
// immediate (non-relayed) relay-endpoint failures.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
