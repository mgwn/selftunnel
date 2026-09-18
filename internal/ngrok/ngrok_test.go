package ngrok

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStartRequiresAuthtoken(t *testing.T) {
	m := &Manager{}
	if _, err := m.Start(context.Background(), "", 8080); err == nil {
		t.Fatal("Start with an empty authtoken should fail")
	}
	if m.Running() {
		t.Fatal("Manager must not be running after a failed Start")
	}
}

func TestStartRejectsBadAuthtoken(t *testing.T) {
	m := &Manager{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A malformed token fails during agent auth or endpoint provisioning;
	// either way the Manager must stay stopped and report the ngrok error.
	_, err := m.Start(ctx, "not-a-real-token", 8080)
	if err == nil {
		t.Skip("ngrok cloud unexpectedly accepted the bogus token (network-dependent)")
	}
	if m.Running() {
		t.Fatal("Manager must not be running after a rejected Start")
	}
	if !strings.Contains(err.Error(), "ngrok:") {
		t.Fatalf("error should be namespaced: %v", err)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	m := &Manager{}
	m.Stop()
	m.Stop()
	if m.Running() || m.PublicURL() != "" {
		t.Fatal("stopped Manager must report not running and no URL")
	}
}
