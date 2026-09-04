// Package proto defines the tunnel framing protocol (spec §5): every
// WebSocket message exchanged between the relay server and a tunnel client
// is exactly one Frame, JSON-encoded as a single text message. Binary
// payloads (request and response bodies) are base64-encoded inside the Data
// field; all other fields are routing and correlation metadata.
package proto

import "encoding/json"

// HelloVersion is the protocol version carried in the hello frame (spec
// §3.1); both sides must agree on it — the server rejects a mismatch with
// hello_ack{ok:false,"error":"version mismatch"}. The line started at 1
// with the 0.1.0 release.
const HelloVersion = 1

// Frame is the single JSON-encoded WebSocket message used by both sides.
//
// Contract (spec §5):
//   - exactly one Frame per WebSocket text message, max 4MB;
//   - receivers must ignore unknown Type values and unknown fields
//     (forward compatibility);
//   - ReqID correlates the frames of one relayed HTTP request; the server
//     assigns it and it is unique within a session;
//   - Seq orders the body chunks of one ReqID (0-based, contiguous);
//   - a request is request_start [+ request_chunk* + request_end]; a
//     response is response_start + response_chunk* + response_end.
//
// Which fields are meaningful depends on Type; the comments below group
// fields by the frame kinds that use them.
type Frame struct {
	Type string `json:"type"`

	// hello (client → server): handshake sent right after WebSocket
	// upgrade. Version must match the server's protocol version.
	Version    int    `json:"version,omitempty"`
	DesiredId  string `json:"desiredId,omitempty"`
	Secret     string `json:"secret,omitempty"`
	ClientType string `json:"clientType,omitempty"`
	ExtVersion string `json:"extVersion,omitempty"`

	// hello_ack / set_target_ack: Ok is false only on rejection.
	Ok bool `json:"ok,omitempty"`

	// hello_ack: the server's answer to hello.
	TunnelID      string `json:"tunnelID,omitempty"` // final ID (may differ from DesiredId)
	Error         string `json:"error,omitempty"`    // rejection reason; also response_end failures
	IDReused      bool   `json:"idReused,omitempty"`
	DesiredDenied bool   `json:"desiredDenied,omitempty"`

	// ping / pong: Unix-second timestamps for liveness.
	TS int64 `json:"ts,omitempty"`

	// set_target / stats: the client's current target base URL and the
	// total number of requests forwarded through this tunnel.
	Target    string `json:"target,omitempty"`
	Forwarded uint64 `json:"forwarded,omitempty"`

	// request / response payload frames.
	ReqID   uint32              `json:"reqId,omitempty"`
	Method  string              `json:"method,omitempty"` // request_start: HTTP method
	Path    string              `json:"path,omitempty"`   // request_start: target path (without tunnel prefix)
	Query   string              `json:"query,omitempty"`  // request_start: raw query string
	Headers map[string][]string `json:"headers,omitempty"`
	HasBody bool                `json:"hasBody,omitempty"` // request_start: body frames follow
	Seq     int                 `json:"seq,omitempty"`     // chunk index within one ReqID
	Data    string              `json:"data,omitempty"`    // base64-encoded chunk payload
	Status  int                 `json:"status,omitempty"`  // response_start: HTTP status code
}

// Marshal encodes f as one JSON document, suitable for a single WebSocket
// text message. Fields left at their zero value are omitted, keeping frames
// small on the wire.
func Marshal(f *Frame) ([]byte, error) {
	return json.Marshal(f)
}

// Unmarshal decodes one received frame payload into f. Unknown fields are
// ignored, so a newer peer may add fields without breaking older ones —
// the forward-compatibility rule of spec §5.
func Unmarshal(data []byte, f *Frame) error {
	return json.Unmarshal(data, f)
}
