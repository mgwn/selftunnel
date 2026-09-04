package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
