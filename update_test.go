package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for _, c := range []struct {
		in                  string
		major, minor, patch int
		ok                  bool
	}{
		{"v0.0.21", 0, 0, 21, true},
		{"0.0.21", 0, 0, 21, true},
		{"v1.12.3", 1, 12, 3, true},
		{"v1.2", 0, 0, 0, false},
		{"foo", 0, 0, 0, false},
		{"v1.2.x", 0, 0, 0, false},
		{"", 0, 0, 0, false},
	} {
		major, minor, patch, ok := parseVersion(c.in)
		if major != c.major || minor != c.minor || patch != c.patch || ok != c.ok {
			t.Errorf("parseVersion(%q) = %d,%d,%d,%v, want %d,%d,%d,%v",
				c.in, major, minor, patch, ok, c.major, c.minor, c.patch, c.ok)
		}
	}
}

func TestVersionNewer(t *testing.T) {
	for _, c := range []struct {
		have, want string
		newer      bool
	}{
		{"0.0.20", "0.0.21", true},
		{"0.0.20", "v0.0.21", true},
		{"0.0.21", "0.0.21", false},
		{"0.0.22", "0.0.21", false},
		{"0.9.9", "1.0.0", true},
		{"1.0.0", "0.9.9", false},
		{"bogus", "0.0.21", false},
		{"0.0.20", "bogus", false},
	} {
		if got := versionNewer(c.have, c.want); got != c.newer {
			t.Errorf("versionNewer(%q, %q) = %v, want %v", c.have, c.want, got, c.newer)
		}
	}
}

func TestUpdateAssetName(t *testing.T) {
	if got := updateAssetName("v0.0.21", "linux", "amd64"); got != "minifactory-0.0.21-linux-amd64.tar.gz" {
		t.Errorf("got %q", got)
	}
	if got := updateAssetName("0.0.21", "darwin", "arm64"); got != "minifactory-0.0.21-darwin-arm64.tar.gz" {
		t.Errorf("got %q", got)
	}
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.bin")
	content := []byte("hello minifactory")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	hexSum := hex.EncodeToString(sum[:])
	sums := hexSum + "  f.bin\n" + "deadbeef  other.bin\n"
	if err := verifyChecksum(p, sums, "f.bin"); err != nil {
		t.Errorf("valid checksum rejected: %v", err)
	}
	if err := verifyChecksum(p, "00"+hexSum[2:]+"  f.bin\n", "f.bin"); err == nil {
		t.Error("tampered checksum accepted")
	}
	if err := verifyChecksum(p, sums, "missing.bin"); err == nil {
		t.Error("missing entry accepted")
	}
}

func makeTestTarGz(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "test.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractTarGz(t *testing.T) {
	src := makeTestTarGz(t, map[string]string{"minifactory": "binary-bytes"})
	dest := t.TempDir()
	files, err := extractTarGz(src, dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "minifactory" {
		t.Fatalf("unexpected files: %v", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil || string(b) != "binary-bytes" {
		t.Errorf("extracted content wrong: %q, %v", b, err)
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	src := makeTestTarGz(t, map[string]string{"../evil": "x"})
	if _, err := extractTarGz(src, t.TempDir()); err == nil {
		t.Error("path traversal accepted")
	} else if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("unexpected error: %v", err)
	}
}
