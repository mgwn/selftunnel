// Package e2e wires the real server and client packages together over
// loopback HTTP/WebSocket connections and verifies the relay behaviour
// required by specs/selftunnel-Spec.md §9 (acceptance criteria).
//
// Cases that need a GUI, a second machine, real network outages, or
// multi-minute durations are covered by tests/ai/TEST-CASES.md instead.
//
// Every test follows the same shape (BDD): a harness (newHarness) provides
// a real relay server and echo target on loopback; startClient connects a
// real client; the test then acts as the public API caller. Subtest names
// spell out the individual checks.
package e2e

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"selftunnel/internal/client"
	"selftunnel/internal/server"
)

// httpClient bounds every e2e request so a hung relay fails the test
// instead of blocking the suite forever.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// tunnelIDPattern mirrors the canonical tunnelID format (spec §3.1).
var tunnelIDPattern = regexp.MustCompile(`^[a-z0-9]{8}$`)

// --- test target -----------------------------------------------------------

// echoResp is the JSON body the /echo endpoint returns: exactly what the
// target saw, so the tests can assert on the full forwarding path.
type echoResp struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Query   string `json:"query"`
	Probe   string `json:"probe"`
	BodyLen int    `json:"bodyLen"`
}

// newEchoTarget builds the loopback target service: /echo reflects what it
// received, /raw echoes the body verbatim (binary roundtrips), /code
// returns a fixed 201 + custom header (response passthrough), /sse streams
// paced events, /who returns the marker (client isolation).
func newEchoTarget(marker string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(echoResp{
			Method:  r.Method,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Probe:   r.Header.Get("X-Probe"),
			BodyLen: len(body),
		})
	})
	mux.HandleFunc("/raw", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, r.Body)
	})
	mux.HandleFunc("/code", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		chunks := 5
		if v := r.URL.Query().Get("chunks"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
				chunks = n
			}
		}
		delay := 25 * time.Millisecond
		if v := r.URL.Query().Get("delayms"); v != "" {
			if d, err := strconv.Atoi(v); err == nil && d >= 0 && d <= 1000 {
				delay = time.Duration(d) * time.Millisecond
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			_, _ = fmt.Fprintf(w, "event: tick\ndata: %d\n\n", i)
			flusher.Flush()
			time.Sleep(delay)
		}
	})
	mux.HandleFunc("/who", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(marker))
	})
	return mux
}

// --- harness ---------------------------------------------------------------

// harness owns one relay server and its default echo target for a test.
type harness struct {
	relay  *httptest.Server
	target *httptest.Server
}

// newHarness starts a real relay server (fresh registry in a temp dir) and
// an echo target on loopback; both are closed when the test ends.
func newHarness(t *testing.T, debugServer bool) *harness {
	t.Helper()
	reg := server.NewRegistry(t.TempDir())
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	srv := server.New(reg, "", []string{"native-client://"}, debugServer)
	relay := httptest.NewServer(srv.Handler())
	target := httptest.NewServer(newEchoTarget("target-A"))
	t.Cleanup(relay.Close)
	t.Cleanup(target.Close)
	return &harness{relay: relay, target: target}
}

// testClient wraps a running client and captures the attributes of its
// "tunnel online" log line (reused / desiredDenied) for assertions.
type testClient struct {
	*client.Client
	cfg    *client.Config
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	attrs map[string]any
}

// startClient connects a real client (customID, targetURL, optional
// ownership secret) to the harness relay and waits until it is online.
// The client is stopped automatically when the test ends.
func (h *harness) startClient(t *testing.T, customID, targetURL, secret string) *testClient {
	t.Helper()
	cfg := &client.Config{
		Server:   client.NormalizeServer(h.relay.URL),
		Target:   targetURL,
		CustomID: customID,
		Secret:   secret,
	}
	tc := &testClient{cfg: cfg, attrs: map[string]any{}}
	tc.Client = client.New(cfg, client.Options{
		OnLog: func(level, msg string, attrs ...any) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			for i := 0; i+1 < len(attrs); i += 2 {
				if k, ok := attrs[i].(string); ok {
					tc.attrs[k] = attrs[i+1]
				}
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	tc.cancel = cancel
	tc.done = make(chan struct{})
	go func() {
		_ = tc.Run(ctx)
		close(tc.done)
	}()
	t.Cleanup(cancel)
	tc.waitOnline(t)
	return tc
}

// waitOnline blocks until the client reports online with a tunnelID, or
// fails the test after 5s.
func (tc *testClient) waitOnline(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tc.Status() == client.StatusOnline && tc.TunnelID() != "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("client %q did not come online within 5s (status=%s)", tc.cfg.CustomID, tc.Status())
}

// attr returns the last captured log attribute value for key.
func (tc *testClient) attr(key string) any {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.attrs[key]
}

// boolAttr asserts the captured attribute key is a bool and returns it.
func (tc *testClient) boolAttr(t *testing.T, key string) bool {
	t.Helper()
	v, ok := tc.attr(key).(bool)
	if !ok {
		t.Fatalf("client %q: log attribute %q missing or not bool (got %v)", tc.cfg.CustomID, key, tc.attr(key))
	}
	return v
}

// stop cancels the client context and waits for Run to return — this is
// the same path SIGINT/SIGTERM take in the CLI (prompt shutdown).
func (tc *testClient) stop(t *testing.T) {
	t.Helper()
	tc.cancel()
	select {
	case <-tc.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("client %q did not stop within 5s", tc.cfg.CustomID)
	}
}

// tunnelURL returns the public relay entry point of the client's tunnel.
func (h *harness) tunnelURL(tc *testClient) string {
	return h.relay.URL + "/t/" + tc.TunnelID()
}

// --- request helpers -------------------------------------------------------

// do issues one HTTP request through the shared test client and fails the
// test on transport errors; the caller closes the response.
func do(t *testing.T, method, url string, body []byte, hdr map[string]string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// readBody drains and closes a response, failing the test on read errors.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// --- tests -----------------------------------------------------------------

// TestE2ERelay verifies the core relay contract (spec §9.1 routes, §9.6
// forwarding — reduced, §9.8 SSE — short variant, §9.12 concurrency).
//
//	Given  one online client with a custom ID and an echo target;
//	When   public callers hit the relay endpoints with various methods,
//	       headers, bodies, streams and concurrency;
//	Then   routing answers exactly as specified: healthz reports the
//	       tunnel, unknown routes/tunnels are 404, every method/echo/
//	       binary/SSE request roundtrips byte-faithfully, and 16 parallel
//	       requests never cross wires.
func TestE2ERelay(t *testing.T) {
	h := newHarness(t, false)
	tc := h.startClient(t, "e2etest1", h.target.URL, "")
	base := h.tunnelURL(tc)

	// spec §9.1: the server exposes exactly the three documented routes.
	t.Run("healthz", func(t *testing.T) {
		body := readBody(t, do(t, "GET", h.relay.URL+"/healthz", nil, nil))
		if !strings.Contains(body, `"ok":true`) {
			t.Fatalf("healthz body = %q", body)
		}
	})

	t.Run("unknown-route-404", func(t *testing.T) {
		resp := do(t, "GET", h.relay.URL+"/nope", nil, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("unknown-tunnel-404", func(t *testing.T) {
		resp := do(t, "GET", h.relay.URL+"/t/zzzzzz99/", nil, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if body := readBody(t, resp); !strings.Contains(body, "tunnel not found") {
			t.Fatalf("body = %q", body)
		}
	})

	// spec §9.6 (reduced to loopback size): method, path, query, headers
	// and body must arrive at the target unchanged.
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "OPTIONS", "DELETE"} {
		t.Run("method-"+m, func(t *testing.T) {
			var body []byte
			if m == "POST" || m == "PUT" || m == "PATCH" {
				body = []byte("payload-for-" + m)
			}
			resp := do(t, m, base+"/echo?mode="+m, body, map[string]string{"X-Probe": "probe-" + m})
			var e echoResp
			err := json.NewDecoder(resp.Body).Decode(&e)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if e.Method != m {
				t.Errorf("method = %q, want %q", e.Method, m)
			}
			if e.Path != "/echo" {
				t.Errorf("path = %q, want /echo", e.Path)
			}
			if e.Query != "mode="+m {
				t.Errorf("query = %q, want %q", e.Query, "mode="+m)
			}
			if e.Probe != "probe-"+m {
				t.Errorf("X-Probe = %q, want %q", e.Probe, "probe-"+m)
			}
			if body != nil && e.BodyLen != len(body) {
				t.Errorf("bodyLen = %d, want %d", e.BodyLen, len(body))
			}
		})
	}

	t.Run("head", func(t *testing.T) {
		resp := do(t, "HEAD", base+"/echo", nil, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})

	// spec §9.6: the response side (status code, headers, body) passes
	// through unchanged.
	t.Run("response-passthrough", func(t *testing.T) {
		resp := do(t, "GET", base+"/code", nil, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		if resp.Header.Get("X-Custom") != "yes" {
			t.Fatalf("X-Custom = %q, want yes", resp.Header.Get("X-Custom"))
		}
	})

	// spec §9.7 (reduced to 1MB): chunked bodies survive both directions
	// byte-for-byte.
	t.Run("binary-roundtrip-1mb", func(t *testing.T) {
		payload := make([]byte, 1<<20)
		if _, err := rand.Read(payload); err != nil {
			t.Fatalf("rand: %v", err)
		}
		resp := do(t, "POST", base+"/raw", payload, nil)
		out, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		sum := md5.Sum(payload)
		got := md5.Sum(out)
		if hex.EncodeToString(sum[:]) != hex.EncodeToString(got[:]) {
			t.Fatalf("1MB body roundtrip mismatch: sent md5 %s, received md5 %s (len %d)", hex.EncodeToString(sum[:]), hex.EncodeToString(got[:]), len(out))
		}
	})

	// spec §9.8 (short variant): events arrive complete, in order, and
	// the per-chunk flush preserves the target's pacing.
	t.Run("sse-stream", func(t *testing.T) {
		start := time.Now()
		body := readBody(t, do(t, "GET", base+"/sse?chunks=5&delayms=25", nil, nil))
		elapsed := time.Since(start)
		for i := 0; i < 5; i++ {
			if !strings.Contains(body, fmt.Sprintf("data: %d", i)) {
				t.Fatalf("SSE body missing event %d: %q", i, body)
			}
		}
		if elapsed < 5*25*time.Millisecond {
			t.Fatalf("SSE stream completed in %v; pacing was not preserved (chunked flush broken?)", elapsed)
		}
	})

	// spec §9.12 (doubled): interleaved requests never cross wires.
	t.Run("concurrency-16", func(t *testing.T) {
		const n = 16
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req, _ := http.NewRequest("GET", fmt.Sprintf("%s/echo?i=%d", base, i), nil)
				req.Header.Set("X-Probe", fmt.Sprintf("p-%d", i))
				resp, err := httpClient.Do(req)
				if err != nil {
					errs <- err
					return
				}
				var e echoResp
				err = json.NewDecoder(resp.Body).Decode(&e)
				resp.Body.Close()
				if err != nil {
					errs <- err
					return
				}
				if e.Query != fmt.Sprintf("i=%d", i) || e.Probe != fmt.Sprintf("p-%d", i) {
					errs <- fmt.Errorf("cross-talk: request %d got query=%q probe=%q", i, e.Query, e.Probe)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})
}

// TestE2EOfflineFastFail verifies spec §9.9's fast-fail half: once the
// client disconnects, relayed requests must fail fast with 502 instead of
// queueing and waiting for the client to return.
//
//	Given  an online tunnel whose client then stops;
//	When   a caller hits the tunnel URL;
//	Then   the relay answers 502 "tunnel offline" promptly (the registry
//	       entry survives — the ID stays reserved for reuse).
func TestE2EOfflineFastFail(t *testing.T) {
	h := newHarness(t, false)
	tc := h.startClient(t, "e2etest2", h.target.URL, "")
	base := h.tunnelURL(tc)
	tc.stop(t)

	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := httpClient.Get(base + "/echo")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusBadGateway {
				if !strings.Contains(string(body), "tunnel offline") {
					t.Fatalf("502 body = %q, want tunnel offline", body)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("tunnel did not fail fast with 502 after client stop (last status=%v err=%v)", resp, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestE2ETargetUnreachable verifies spec §9.11's first clause: a target
// that cannot be reached surfaces as 502 to the caller.
//
//	Given  an online tunnel whose target points at a dead port;
//	When   a caller hits the tunnel URL;
//	Then   the relay answers 502 (the client's synthetic error response).
func TestE2ETargetUnreachable(t *testing.T) {
	h := newHarness(t, false)
	tc := h.startClient(t, "e2etest3", "http://127.0.0.1:1", "")
	resp := do(t, "GET", h.tunnelURL(tc)+"/echo", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// TestE2EIDReuse verifies spec §9.3: after a restart with the persisted
// secret, the client must reclaim the same tunnelID (idReused=true) and
// the tunnel must keep working.
//
//	Given  a client that connected once with custom ID "reuseid1" and
//	       persisted its ownership secret;
//	When   that client stops and a second client reconnects presenting
//	       the same desired ID and secret;
//	Then   the server confirms the same tunnelID, reports reused=true,
//	       and relayed requests succeed through the reclaimed tunnel.
func TestE2EIDReuse(t *testing.T) {
	h := newHarness(t, false)
	tc1 := h.startClient(t, "reuseid1", h.target.URL, "")
	if tc1.TunnelID() != "reuseid1" {
		t.Fatalf("tunnelID = %q, want reuseid1", tc1.TunnelID())
	}
	secret := tc1.cfg.Secret
	if secret == "" {
		t.Fatal("client must have persisted a secret after first connect")
	}
	tc1.stop(t)

	tc2 := h.startClient(t, "reuseid1", h.target.URL, secret)
	if tc2.TunnelID() != "reuseid1" {
		t.Fatalf("tunnelID after restart = %q, want reuseid1", tc2.TunnelID())
	}
	if !tc2.boolAttr(t, "reused") {
		t.Error("expected reused=true in tunnel-online log")
	}

	resp := do(t, "GET", h.tunnelURL(tc2)+"/who", nil, nil)
	defer resp.Body.Close()
	if body := readBody(t, resp); body != "target-A" {
		t.Fatalf("relayed request after reuse got %q", body)
	}
}

// TestE2ECustomIDDenied verifies spec §9.4: a second client without the
// ownership secret must be downgraded to a random ID, while the rightful
// owner keeps its tunnel untouched.
//
//	Given  a client online with custom ID "denied01";
//	When   a second client requests the same custom ID without the secret;
//	Then   it receives a different, valid random ID and logs
//	       desiredDenied=true, and the owner's tunnel still serves
//	       requests under the original ID.
func TestE2ECustomIDDenied(t *testing.T) {
	h := newHarness(t, false)
	owner := h.startClient(t, "denied01", h.target.URL, "")
	if owner.TunnelID() != "denied01" {
		t.Fatalf("owner tunnelID = %q, want denied01", owner.TunnelID())
	}

	squatter := h.startClient(t, "denied01", h.target.URL, "")
	if squatter.TunnelID() == "denied01" {
		t.Fatal("client without secret must not get the taken custom ID")
	}
	if !tunnelIDPattern.MatchString(squatter.TunnelID()) {
		t.Fatalf("downgraded tunnelID %q is invalid", squatter.TunnelID())
	}
	if !squatter.boolAttr(t, "desiredDenied") {
		t.Error("expected desiredDenied=true in tunnel-online log")
	}
	if owner.TunnelID() != "denied01" {
		t.Fatalf("owner lost its ID after squat attempt: %q", owner.TunnelID())
	}

	resp := do(t, "GET", h.tunnelURL(owner)+"/who", nil, nil)
	defer resp.Body.Close()
	if body := readBody(t, resp); body != "target-A" {
		t.Fatalf("owner tunnel broken after squat attempt: %q", body)
	}
}

// TestE2EClientIsolation verifies spec §9.5: two clients online at the
// same time reach their own targets without cross-talk.
//
//	Given  two online clients with distinct custom IDs and distinct
//	       targets (markers "target-A" and "target-B");
//	When   callers hit both tunnel URLs;
//	Then   each request returns its own target's marker.
func TestE2EClientIsolation(t *testing.T) {
	h := newHarness(t, false)
	targetB := httptest.NewServer(newEchoTarget("target-B"))
	defer targetB.Close()

	tcA := h.startClient(t, "isola001", h.target.URL, "")
	tcB := h.startClient(t, "isola002", targetB.URL, "")
	if tcA.TunnelID() == tcB.TunnelID() {
		t.Fatal("two clients must not share a tunnelID")
	}

	respA := do(t, "GET", h.tunnelURL(tcA)+"/who", nil, nil)
	if body := readBody(t, respA); body != "target-A" {
		t.Fatalf("client A got %q, want target-A", body)
	}
	respB := do(t, "GET", h.tunnelURL(tcB)+"/who", nil, nil)
	if body := readBody(t, respB); body != "target-B" {
		t.Fatalf("client B got %q, want target-B", body)
	}
}

// TestE2EDebugServer verifies the -debug feature end-to-end: with debug
// enabled the server logs one arrival line and one completion line per
// request, and serving through the middleware still works.
//
//	Given  a relay server started with debug=true and an online client;
//	When   one request is relayed through the tunnel;
//	Then   the response is correct and the captured log contains the
//	       request line (method, path) and the request-done line (status).
func TestE2EDebugServer(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	log.SetOutput(&lockedLogWriter{mu: &mu, buf: &buf})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	h := newHarness(t, true)
	tc := h.startClient(t, "dbgmode1", h.target.URL, "")
	resp := do(t, "GET", h.tunnelURL(tc)+"/who", nil, nil)
	if body := readBody(t, resp); body != "target-A" {
		t.Fatalf("debug middleware broke serving: %q", body)
	}

	mu.Lock()
	defer mu.Unlock()
	logs := buf.String()
	for _, want := range []string{
		"request method=GET",
		"path=/t/dbgmode1/who",
		"request done",
		"status=200",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("debug log missing %q; got:\n%s", want, logs)
		}
	}
}

// lockedLogWriter is a mutex-guarded buffer for capturing slog's default
// output (which writes through the standard log package).
type lockedLogWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *lockedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
