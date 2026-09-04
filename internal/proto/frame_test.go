package proto

import "testing"

// TestFrameRoundtrip verifies the frame codec contract (spec §5): a frame
// with payload fields survives a Marshal → Unmarshal roundtrip unchanged.
//
// Given  a response_start frame with reqId, status and multi-value headers;
// When  it is marshaled and unmarshaled again;
// Then   every field comes back identical.
func TestFrameRoundtrip(t *testing.T) {
	in := &Frame{
		Type:    "response_start",
		ReqID:   42,
		Status:  200,
		Headers: map[string][]string{"Content-Type": {"application/json"}, "X-Any": {"v"}},
	}
	b, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out Frame
	if err := Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Type != in.Type || out.ReqID != in.ReqID || out.Status != in.Status {
		t.Fatalf("basic fields mismatch: %+v vs %+v", out, in)
	}
	if len(out.Headers) != 2 || out.Headers["Content-Type"][0] != "application/json" {
		t.Fatalf("headers mismatch: %+v", out.Headers)
	}
}

// TestFrameHelloRoundtrip verifies the hello handshake frame (spec §3.1
// step 2) roundtrips with all handshake fields intact.
//
// Given  a hello frame with version, desiredId, secret and client metadata;
// When  it is marshaled and unmarshaled;
// Then   all handshake fields come back identical.
func TestFrameHelloRoundtrip(t *testing.T) {
	in := &Frame{
		Type:       "hello",
		Version:    HelloVersion,
		DesiredId:  "ab12cd34",
		Secret:     "s3cr3t",
		ClientType: "native-client",
		ExtVersion: "1.0.0",
	}
	b, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out Frame
	if err := Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Version != HelloVersion || out.DesiredId != "ab12cd34" || out.Secret != "s3cr3t" ||
		out.ClientType != "native-client" || out.ExtVersion != "1.0.0" {
		t.Fatalf("hello fields mismatch: %+v", out)
	}
}

// TestUnmarshalIgnoresUnknownFields verifies the forward-compatibility rule
// (spec §5): unknown fields from a newer peer must not break decoding.
//
// Given  a frame payload carrying an extra, unrecognized field;
// When   it is unmarshaled;
// Then   decoding succeeds and the known fields are intact.
func TestUnmarshalIgnoresUnknownFields(t *testing.T) {
	data := `{"type":"ping","ts":1735689600,"futureField":"whatever"}`
	var f Frame
	if err := Unmarshal([]byte(data), &f); err != nil {
		t.Fatalf("Unmarshal should tolerate unknown fields: %v", err)
	}
	if f.Type != "ping" || f.TS != 1735689600 {
		t.Fatalf("fields mismatch: %+v", f)
	}
}

// TestUnmarshalRejectsGarbage verifies malformed payloads surface as errors
// instead of being silently swallowed.
//
// Given  a payload that is not valid JSON;
// When   it is unmarshaled;
// Then   Unmarshal returns a non-nil error.
func TestUnmarshalRejectsGarbage(t *testing.T) {
	var f Frame
	if err := Unmarshal([]byte("not json"), &f); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}
