// Package ngrok manages the ngrok agent as an external binary subprocess
// (spec §3.8.2): it downloads the official stable binary for the current
// platform, starts an HTTP tunnel to a local port with the operator's
// authtoken, discovers the assigned public URL through the local agent
// API, and stops the subprocess on demand. ngrok is not a Go dependency —
// the dependency whitelist of spec §1 is unchanged by this package.
package ngrok

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// agentAPIURL is the ngrok agent's local web interface, queried for the
// assigned public URL (spec §3.8.2).
const agentAPIURL = "http://127.0.0.1:4040/api/tunnels"

// startTimeout bounds how long Start waits for the public URL to appear.
const startTimeout = 20 * time.Second

// downloadURL returns the official stable-archive URL for goos/goarch.
func downloadURL(goos, goarch string) (string, error) {
	ext := "tgz"
	if goos == "darwin" || goos == "windows" {
		ext = "zip"
	}
	switch goos + "/" + goarch {
	case "darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64":
		return fmt.Sprintf("https://bin.equinox.io/c/4VmDzA7iaHb/ngrok-stable-%s-%s.%s", goos, goarch, ext), nil
	}
	return "", fmt.Errorf("ngrok: unsupported platform %s/%s", goos, goarch)
}

// binaryName is the executable name on the current platform.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "ngrok.exe"
	}
	return "ngrok"
}

// EnsureBinary makes sure the ngrok binary for the current platform
// exists in dir (downloading and extracting the official stable archive
// on first use) and returns its path. An existing binary is reused as-is.
func EnsureBinary(dir string) (string, error) {
	path := filepath.Join(dir, binaryName())
	if st, err := os.Stat(path); err == nil && !st.IsDir() && st.Size() > 0 {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	url, err := downloadURL(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	resp, err := http.Get(url) //nolint:noctx // bounded by the http client default below
	if err != nil {
		return "", fmt.Errorf("ngrok: download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ngrok: download failed: %s", resp.Status)
	}
	archive, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ngrok: download failed: %w", err)
	}
	tmp := path + ".tmp"
	if err := extractBinary(archive, url, tmp); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// extractBinary writes the single ngrok executable contained in archive
// (a zip or tar.gz, chosen by the URL suffix) to dst.
func extractBinary(archive []byte, url, dst string) error {
	if strings.HasSuffix(url, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return fmt.Errorf("ngrok: bad zip archive: %w", err)
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			content, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return err
			}
			return os.WriteFile(dst, content, 0755)
		}
		return errors.New("ngrok: empty zip archive")
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("ngrok: bad tgz archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return errors.New("ngrok: empty tgz archive")
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		return writeAll(dst, tr)
	}
}

// writeAll streams r into dst.
func writeAll(dst string, r io.Reader) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, r)
	return err
}

// parseTunnels extracts the first https public URL from an agent-API
// /api/tunnels response body.
func parseTunnels(body []byte) (string, error) {
	var resp struct {
		Tunnels []struct {
			PublicURL string `json:"public_url"`
		} `json:"tunnels"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("ngrok: bad agent API response: %w", err)
	}
	for _, t := range resp.Tunnels {
		if strings.HasPrefix(t.PublicURL, "https://") {
			return t.PublicURL, nil
		}
	}
	return "", errors.New("ngrok: no https tunnel in agent API response")
}

// Manager controls one ngrok subprocess at a time. All methods are safe
// for concurrent use.
type Manager struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	done      chan struct{}
	publicURL string
}

// Start launches ngrok as an HTTP tunnel to localPort using the given
// authtoken and blocks until the public URL is available from the agent
// API (or startTimeout elapses). The subprocess is bound to ctx: when
// ctx is cancelled, or Stop is called, ngrok terminates.
func (m *Manager) Start(ctx context.Context, binaryPath, authtoken string, localPort int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil {
		return "", errors.New("ngrok: already running")
	}
	if authtoken == "" {
		return "", errors.New("ngrok: authtoken is required")
	}
	if _, err := os.Stat(binaryPath); err != nil {
		return "", fmt.Errorf("ngrok: binary not found at %s", binaryPath)
	}

	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, binaryPath,
		"http", fmt.Sprintf("%d", localPort),
		"--authtoken", authtoken,
		"--log", "stdout")
	// ngrok diagnostics are kept for error reporting only.
	var diag bytes.Buffer
	cmd.Stdout = &diag
	cmd.Stderr = &diag
	if err := cmd.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("ngrok: failed to start: %w", err)
	}

	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	url, err := m.awaitPublicURL(runCtx, done)
	if err != nil {
		cancel()
		<-done
		tail := diag.String()
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		if tail != "" {
			err = fmt.Errorf("%w (ngrok log tail: %s)", err, strings.TrimSpace(tail))
		}
		return "", err
	}

	m.cmd, m.cancel, m.done, m.publicURL = cmd, cancel, done, url
	return url, nil
}

// awaitPublicURL polls the local agent API until an https tunnel appears,
// the process exits, the context is done or startTimeout elapses.
func (m *Manager) awaitPublicURL(ctx context.Context, done <-chan struct{}) (string, error) {
	deadline := time.Now().Add(startTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-done:
			return "", errors.New("ngrok: process exited before the tunnel was up")
		case <-time.After(500 * time.Millisecond):
		}
		resp, err := client.Get(agentAPIURL)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		if url, err := parseTunnels(body); err == nil {
			return url, nil
		}
	}
	return "", errors.New("ngrok: timed out waiting for the public URL")
}

// Stop terminates the ngrok subprocess and waits for it to exit (up to
// 5s). Stop on a stopped Manager is a no-op.
func (m *Manager) Stop() {
	m.mu.Lock()
	cmd, cancel, done := m.cmd, m.cancel, m.done
	m.cmd, m.cancel, m.done, m.publicURL = nil, nil, nil, ""
	m.mu.Unlock()
	if cmd == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
	}
}

// Running reports whether an ngrok subprocess is active.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmd != nil
}

// PublicURL returns the assigned public URL, or "" while not running.
func (m *Manager) PublicURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.publicURL
}
