package client

import (
	"path/filepath"
	"testing"
)

// TestNormalizeServer verifies the server address normalization contract
// (spec §3.5.3): https/http/bare-host inputs all become the canonical
// wss://host/ws/tunnel WebSocket URL, and empty input stays empty.
//
//	Given  server addresses in every accepted spelling;
//	When   NormalizeServer runs;
//	Then   each maps to its canonical wss://…/ws/tunnel form.
func TestNormalizeServer(t *testing.T) {
	cases := map[string]string{
		"https://relay.example.com":         "wss://relay.example.com/ws/tunnel",
		"http://relay.example.com":          "ws://relay.example.com/ws/tunnel",
		"relay.example.com":                 "wss://relay.example.com/ws/tunnel",
		"wss://relay.example.com":           "wss://relay.example.com/ws/tunnel",
		"ws://relay.example.com":            "ws://relay.example.com/ws/tunnel",
		"wss://relay.example.com/":          "wss://relay.example.com/ws/tunnel",
		"wss://relay.example.com/ws/tunnel": "wss://relay.example.com/ws/tunnel",
		"":                                  "",
	}
	for in, want := range cases {
		if got := NormalizeServer(in); got != want {
			t.Errorf("NormalizeServer(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsValidTarget verifies the target validation contract (spec §6.5):
// targets must start with http:// or https://.
//
//	Given  target URLs of various schemes;
//	When   IsValidTarget runs;
//	Then   only http(s) prefixes pass.
func TestIsValidTarget(t *testing.T) {
	valid := []string{"http://192.168.1.10:8080", "https://example.com"}
	invalid := []string{"", "ftp://example.com", "example.com"}
	for _, v := range valid {
		if !IsValidTarget(v) {
			t.Errorf("IsValidTarget(%q) = false, want true", v)
		}
	}
	for _, v := range invalid {
		if IsValidTarget(v) {
			t.Errorf("IsValidTarget(%q) = true, want false", v)
		}
	}
}

// TestIsValidCustomID verifies the custom ID gate (spec §3.1): exactly 8
// lowercase alphanumerics.
//
//	Given  candidate custom IDs of varying length, case and charset;
//	When   IsValidCustomID runs;
//	Then   only the canonical ones pass.
func TestIsValidCustomID(t *testing.T) {
	valid := []string{"abcd1234", "00000000", "zzzzzzzz"}
	invalid := []string{"", "abcd123", "abcd12345", "ABCD1234", "abcd-123", "abcd_123"}
	for _, v := range valid {
		if !IsValidCustomID(v) {
			t.Errorf("IsValidCustomID(%q) = false, want true", v)
		}
	}
	for _, v := range invalid {
		if IsValidCustomID(v) {
			t.Errorf("IsValidCustomID(%q) = true, want false", v)
		}
	}
}

// TestConfigSaveLoadRoundtrip verifies spec §6.5: the config file survives
// a save/load roundtrip with every field intact.
//
//	Given  a fully populated config;
//	When   it is saved and loaded back;
//	Then   the loaded config equals the original.
func TestConfigSaveLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	in := &Config{
		Server:   "wss://relay.example.com/ws/tunnel",
		Target:   "http://192.168.1.10:8080",
		CustomID: "abcd1234",
		TunnelID: "wxyz9876",
		Secret:   "s3cr3t-value",
	}
	if err := SaveConfig(path, in); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	out, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if *out != *in {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", out, in)
	}
}

// TestLoadConfigMissingFile verifies the first-run experience (spec §3.5
// step 1): a missing config file yields an empty config, not an error.
//
//	Given  no config file on disk;
//	When   LoadConfig runs;
//	Then   an empty config is returned with a nil error.
func TestLoadConfigMissingFile(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing config must not error: %v", err)
	}
	if cfg == nil || cfg.Server != "" {
		t.Fatalf("missing config must yield an empty config, got %+v", cfg)
	}
}
