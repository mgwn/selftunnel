// Package ngrok embeds public exposure via the official ngrok Go SDK
// (golang.ngrok.com/ngrok/v2): the tunnel runs in-process — no external
// binary to download or supervise. The Manager starts an HTTPS endpoint
// forwarding to the local relay port with the operator's authtoken and
// reports the assigned public URL directly.
package ngrok

import (
	"context"
	"errors"
	"fmt"
	"sync"

	sdk "golang.ngrok.com/ngrok/v2"
)

// Manager controls one embedded ngrok endpoint at a time. All methods are
// safe for concurrent use.
type Manager struct {
	mu        sync.Mutex
	agent     sdk.Agent
	fwd       sdk.EndpointForwarder
	publicURL string
}

// Start connects to the ngrok cloud with the given authtoken, opens an
// HTTPS endpoint forwarding to localPort, and returns the assigned public
// URL once ngrok has provisioned it. The endpoint is bound to ctx: when
// ctx is cancelled, or Stop is called, the tunnel closes.
func (m *Manager) Start(ctx context.Context, authtoken string, localPort int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fwd != nil {
		return "", errors.New("ngrok: already running")
	}
	if authtoken == "" {
		return "", errors.New("ngrok: authtoken is required")
	}

	agent, err := sdk.NewAgent(sdk.WithAuthtoken(authtoken))
	if err != nil {
		return "", fmt.Errorf("ngrok: agent setup failed: %w", err)
	}
	fwd, err := agent.Forward(ctx,
		sdk.WithUpstream(fmt.Sprintf("http://127.0.0.1:%d", localPort)),
		sdk.WithURL("https://"),
	)
	if err != nil {
		_ = agent.Disconnect()
		return "", fmt.Errorf("ngrok: %w", err)
	}

	url := fwd.URL().String()
	m.agent, m.fwd, m.publicURL = agent, fwd, url
	return url, nil
}

// Stop closes the endpoint and disconnects the agent. Stop on a stopped
// Manager is a no-op.
func (m *Manager) Stop() {
	m.mu.Lock()
	agent, fwd := m.agent, m.fwd
	m.agent, m.fwd, m.publicURL = nil, nil, ""
	m.mu.Unlock()
	if fwd == nil {
		return
	}
	_ = fwd.Close()
	_ = agent.Disconnect()
}

// Running reports whether an ngrok endpoint is active.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fwd != nil
}

// PublicURL returns the assigned public URL, or "" while not running.
func (m *Manager) PublicURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.publicURL
}
