package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadEnvFromMissing(t *testing.T) {
	m, found := loadEnvFrom(t.TempDir())
	if found {
		t.Fatal("expected found=false for a dir with no .env")
	}
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %v", m)
	}
}

func TestLoadEnvFromParses(t *testing.T) {
	dir := t.TempDir()
	content := "# comment\n\nGITHUB_TOKEN=abc123\nGITHUB_REPO = vicgupta/mf-test\nQUOTED=\"hello\"\nEMPTY=\nBADLINE\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	m, found := loadEnvFrom(dir)
	if !found {
		t.Fatal("expected found=true for a readable .env")
	}
	if m["GITHUB_TOKEN"] != "abc123" {
		t.Errorf("GITHUB_TOKEN = %q", m["GITHUB_TOKEN"])
	}
	if m["GITHUB_REPO"] != "vicgupta/mf-test" {
		t.Errorf("GITHUB_REPO = %q", m["GITHUB_REPO"])
	}
	if m["QUOTED"] != "hello" {
		t.Errorf("QUOTED = %q", m["QUOTED"])
	}
	if v, ok := m["EMPTY"]; !ok || v != "" {
		t.Errorf("EMPTY = %q, %v", v, ok)
	}
	if _, ok := m["BADLINE"]; ok {
		t.Error("BADLINE should be skipped")
	}
}

func TestCheckEnvFor(t *testing.T) {
	oldBase, oldFound := baseDir, envFound
	defer func() { baseDir, envFound = oldBase, oldFound }()
	baseDir = "/tmp/nonexistent-dir"

	envFound = false
	for _, cmd := range []string{"poll", "run-once", "sync"} {
		if err := checkEnvFor(cmd); err == nil {
			t.Errorf("checkEnvFor(%q) with no .env: expected error", cmd)
		}
	}
	for _, cmd := range []string{"init", "doctor", "list", "logs", "retry", "version", "issue"} {
		if err := checkEnvFor(cmd); err != nil {
			t.Errorf("checkEnvFor(%q) with no .env: unexpected error: %v", cmd, err)
		}
	}

	envFound = true
	for _, cmd := range []string{"poll", "run-once", "sync", "list", "retry"} {
		if err := checkEnvFor(cmd); err != nil {
			t.Errorf("checkEnvFor(%q) with .env: unexpected error: %v", cmd, err)
		}
	}
}

func TestStripLogFlag(t *testing.T) {
	cases := []struct {
		in    []string
		want  []string
		found bool
	}{
		{[]string{"minifactory", "--log", "run-once"}, []string{"minifactory", "run-once"}, true},
		{[]string{"minifactory", "run-once", "--log"}, []string{"minifactory", "run-once"}, true},
		{[]string{"minifactory", "poll", "-log"}, []string{"minifactory", "poll"}, true},
		{[]string{"minifactory", "—log", "sync"}, []string{"minifactory", "sync"}, true},
		{[]string{"minifactory", "list"}, []string{"minifactory", "list"}, false},
		{[]string{"minifactory", "--json", "list"}, []string{"minifactory", "--json", "list"}, false},
	}
	for _, c := range cases {
		got, found := stripLogFlag(append([]string(nil), c.in...))
		if found != c.found {
			t.Errorf("stripLogFlag(%v): found=%v, want %v", c.in, found, c.found)
		}
		if len(got) != len(c.want) {
			t.Errorf("stripLogFlag(%v) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("stripLogFlag(%v) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

func TestTracefGating(t *testing.T) {
	old := verboseLog
	defer func() { verboseLog = old }()

	// Enabled: emits a timestamped line on stdout.
	verboseLog = true
	out := captureStdout(t, func() { tracef("hello %s", "world") })
	if !strings.Contains(out, "hello world") {
		t.Errorf("tracef with --log: got %q, want it to contain the message", out)
	}
	if !strings.Contains(out, time.Now().Format("2006-01-02")) {
		t.Errorf("tracef with --log: got %q, want a timestamp", out)
	}

	// Disabled: silent.
	verboseLog = false
	out = captureStdout(t, func() { tracef("hello %s", "world") })
	if out != "" {
		t.Errorf("tracef without --log: got %q, want silence", out)
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}
