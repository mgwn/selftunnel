package ngrok

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadURLPerPlatform(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"darwin", "amd64", "https://bin.equinox.io/c/4VmDzA7iaHb/ngrok-stable-darwin-amd64.zip"},
		{"darwin", "arm64", "https://bin.equinox.io/c/4VmDzA7iaHb/ngrok-stable-darwin-arm64.zip"},
		{"linux", "amd64", "https://bin.equinox.io/c/4VmDzA7iaHb/ngrok-stable-linux-amd64.tgz"},
		{"linux", "arm64", "https://bin.equinox.io/c/4VmDzA7iaHb/ngrok-stable-linux-arm64.tgz"},
		{"windows", "amd64", "https://bin.equinox.io/c/4VmDzA7iaHb/ngrok-stable-windows-amd64.zip"},
	}
	for _, c := range cases {
		got, err := downloadURL(c.goos, c.goarch)
		if err != nil {
			t.Errorf("downloadURL(%s, %s): %v", c.goos, c.goarch, err)
			continue
		}
		if got != c.want {
			t.Errorf("downloadURL(%s, %s) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
	for _, platform := range []struct{ goos, goarch string }{{"linux", "386"}, {"freebsd", "amd64"}} {
		if _, err := downloadURL(platform.goos, platform.goarch); err == nil {
			t.Errorf("downloadURL(%s, %s) should fail", platform.goos, platform.goarch)
		}
	}
}

func TestParseTunnels(t *testing.T) {
	body := []byte(`{"tunnels":[` +
		`{"name":"command_line","public_url":"http://abc.ngrok.io","proto":"http"},` +
		`{"name":"command_line","public_url":"https://xyz.ngrok-free.app","proto":"https"}]}`)
	got, err := parseTunnels(body)
	if err != nil {
		t.Fatalf("parseTunnels: %v", err)
	}
	if got != "https://xyz.ngrok-free.app" {
		t.Fatalf("parseTunnels = %q, want the https URL", got)
	}

	if _, err := parseTunnels([]byte(`{"tunnels":[]}`)); err == nil {
		t.Fatal("empty tunnel list should be an error")
	}
	if _, err := parseTunnels([]byte("not json")); err == nil {
		t.Fatal("garbage should be an error")
	}
}

func TestExtractBinaryZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, _ := zw.Create("ngrok")
	_, _ = fw.Write([]byte("#!/bin/sh\necho fake-ngrok\n"))
	_ = zw.Close()

	dst := filepath.Join(t.TempDir(), "ngrok")
	if err := extractBinary(buf.Bytes(), "https://example.com/ngrok-stable-darwin-arm64.zip", dst); err != nil {
		t.Fatalf("extractBinary(zip): %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "#!/bin/sh\necho fake-ngrok\n" {
		t.Fatalf("extracted content mismatch: %q err=%v", got, err)
	}
}

func TestExtractBinaryTgz(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "ngrok", Mode: 0o755, Size: int64(len("fake-binary"))}
	_ = tw.WriteHeader(hdr)
	_, _ = tw.Write([]byte("fake-binary"))
	_ = tw.Close()
	_ = gz.Close()

	dst := filepath.Join(t.TempDir(), "ngrok")
	if err := extractBinary(buf.Bytes(), "https://example.com/ngrok-stable-linux-amd64.tgz", dst); err != nil {
		t.Fatalf("extractBinary(tgz): %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "fake-binary" {
		t.Fatalf("extracted content mismatch: %q err=%v", got, err)
	}
}

func TestEnsureBinaryReusesExisting(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, binaryName())
	if err := os.WriteFile(existing, []byte("already-here"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := EnsureBinary(dir)
	if err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}
	if got != existing {
		t.Fatalf("EnsureBinary = %q, want the existing %q", got, existing)
	}
}
