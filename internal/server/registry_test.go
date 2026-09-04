package server

import (
	"regexp"
	"testing"
)

// idPattern mirrors the canonical tunnelID format (spec §3.1).
var idPattern = regexp.MustCompile(`^[a-z0-9]{8}$`)

// TestRegisterRandomID verifies spec §3.1 step 3 (empty desiredId): a
// random 8-character ID is allocated and a fresh secret delivered.
//
//	Given  an empty registry and a hello with no desired ID;
//	When   Register runs;
//	Then   the ID matches ^[a-z0-9]{8}$, is neither reused nor denied,
//	       and a non-empty secret is returned exactly once.
func TestRegisterRandomID(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	tun, reused, denied, secret, err := reg.Register("", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !idPattern.MatchString(tun.ID) {
		t.Fatalf("random ID %q does not match ^[a-z0-9]{8}$", tun.ID)
	}
	if reused || denied {
		t.Fatalf("fresh registration must not be reused/denied: reused=%v denied=%v", reused, denied)
	}
	if secret == "" {
		t.Fatal("fresh registration must deliver a secret")
	}
}

// TestRegisterCustomID verifies spec §3.1 step 3 (desired ID, unclaimed):
// the desired ID is honoured and a secret is delivered with it.
//
//	Given  an empty registry and a valid desired ID;
//	When   Register runs;
//	Then   the tunnel gets exactly that ID plus a fresh secret.
func TestRegisterCustomID(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	tun, _, _, secret, err := reg.Register("abcd1234", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if tun.ID != "abcd1234" {
		t.Fatalf("expected custom ID abcd1234, got %q", tun.ID)
	}
	if secret == "" {
		t.Fatal("first allocation of a custom ID must deliver a secret")
	}
}

// TestRegisterReuseWithSecret verifies spec §3.1 step 4 (ID reuse):
// presenting the same desired ID with its secret reclaims the tunnel.
//
//	Given  a tunnel already registered under abcd1234;
//	When   Register runs again with the same ID and the correct secret;
//	Then   the same tunnel is returned with reused=true and no new secret.
func TestRegisterReuseWithSecret(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	_, _, _, secret, err := reg.Register("abcd1234", "")
	if err != nil {
		t.Fatalf("first Register: %v", err)
	}

	tun, reused, denied, newSecret, err := reg.Register("abcd1234", secret)
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}
	if tun.ID != "abcd1234" || !reused || denied {
		t.Fatalf("expected reuse of abcd1234, got id=%q reused=%v denied=%v", tun.ID, reused, denied)
	}
	if newSecret != "" {
		t.Fatal("reuse must not re-deliver the secret")
	}
}

// TestRegisterDeniedWrongSecret verifies spec §3.1 step 3 (downgrade) and
// §9.4: a wrong secret cannot take a claimed custom ID — the client is
// silently downgraded to a random ID.
//
//	Given  a tunnel already registered under abcd1234;
//	When   Register runs again with that ID but a wrong secret;
//	Then   a different valid random ID is allocated, denied=true and
//	       reused=false (the client still gets a tunnel).
func TestRegisterDeniedWrongSecret(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	if _, _, _, _, err := reg.Register("abcd1234", ""); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	tun, reused, denied, _, err := reg.Register("abcd1234", "wrong-secret")
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}
	if tun.ID == "abcd1234" {
		t.Fatal("wrong secret must not take the custom ID")
	}
	if !idPattern.MatchString(tun.ID) {
		t.Fatalf("downgraded ID %q is invalid", tun.ID)
	}
	if reused || !denied {
		t.Fatalf("expected denied=true reused=false, got denied=%v reused=%v", denied, reused)
	}
}

// TestRegisterLowercasesDesired verifies spec §3.1 step 3: desired IDs are
// lowercased before validation, so mixed-case input is still honoured.
//
//	Given  a desired ID in uppercase;
//	When   Register runs;
//	Then   the tunnel is registered under the lowercased form.
func TestRegisterLowercasesDesired(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	tun, _, _, _, err := reg.Register("ABCD1234", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if tun.ID != "abcd1234" {
		t.Fatalf("expected desired ID to be lowercased to abcd1234, got %q", tun.ID)
	}
}

// TestRegisterInvalidDesiredFallsBackToRandom verifies spec §3.1 step 3:
// malformed desired IDs are discarded in favour of a random allocation
// instead of being rejected.
//
//	Given  desired IDs that are too short, too long or contain symbols;
//	When   Register runs for each;
//	Then   every resulting ID is a valid random 8-character ID.
func TestRegisterInvalidDesiredFallsBackToRandom(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	for _, desired := range []string{"short", "waytoolongid123", "UPPER!@#"} {
		tun, _, _, _, err := reg.Register(desired, "")
		if err != nil {
			t.Fatalf("Register(%q): %v", desired, err)
		}
		if !idPattern.MatchString(tun.ID) {
			t.Fatalf("invalid desired %q must fall back to a random 8-char ID, got %q", desired, tun.ID)
		}
	}
}

// TestRegistrySaveLoad verifies spec §6.5: registrations persist across
// restarts — ID, secret hash and target survive; sessions do not.
//
//	Given  a registry with one tunnel, its secret and a target, saved;
//	When   a fresh registry loads the same data directory;
//	Then   the tunnel is back with its target and can verify the secret,
//	       has no session, and the counts match.
func TestRegistrySaveLoad(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	_, _, _, secret, err := reg.Register("abcd1234", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg.Get("abcd1234").SetTarget("http://192.168.1.10:8080")
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reg2 := NewRegistry(dir)
	if err := reg2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := reg2.Get("abcd1234")
	if got == nil {
		t.Fatal("tunnel abcd1234 missing after reload")
	}
	if got.Target() != "http://192.168.1.10:8080" {
		t.Fatalf("target not persisted: %q", got.Target())
	}
	if !got.verifySecret(secret) {
		t.Fatal("secret hash not persisted correctly")
	}
	if got.Session() != nil {
		t.Fatal("sessions must not be persisted")
	}
	if reg2.Count() != reg.Count() {
		t.Fatalf("count mismatch after reload: %d vs %d", reg2.Count(), reg.Count())
	}
}
