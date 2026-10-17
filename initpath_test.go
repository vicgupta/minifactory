package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupFakeExe creates home/.local/bin/minifactory (executable) and returns exe path.
func setupFakeExe(t *testing.T, home string) string {
	t.Helper()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "minifactory")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func countMarkers(t *testing.T, home string, names ...string) {
	t.Helper()
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(home, n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		if c := strings.Count(string(b), profileMarker); c != 1 {
			t.Errorf("%s: marker count=%d want 1:\n%s", n, c, b)
		}
		if c := strings.Count(string(b), pathExportLine); c != 1 {
			t.Errorf("%s: export count=%d want 1", n, c)
		}
	}
}

func TestApplyInitPathProfileAddedOnceAndProcessPath(t *testing.T) {
	home := t.TempDir()
	exe := setupFakeExe(t, home)
	t.Setenv("PATH", "/usr/bin:/bin")
	r1 := applyInitPath(exe, home, "/bin/bash", "")
	if r1.State != "profile updated" || len(r1.Changed) != 2 {
		t.Fatalf("first: %+v want profile updated 2 files", r1)
	}
	countMarkers(t, home, ".profile", ".bashrc")
	// (b): PATH inside process gains bin dir and LookPath resolves to exe
	if !pathContainsDir(os.Getenv("PATH"), filepath.Dir(exe)) {
		t.Errorf("(b) PATH %q missing bin dir", os.Getenv("PATH"))
	}
	if r1.Resolved != exe {
		t.Errorf("Resolved=%q want %q", r1.Resolved, exe)
	}
	// Second run: still once. Reset PATH to simulate fresh shell without it.
	t.Setenv("PATH", "/usr/bin:/bin")
	// t.Setenv restores after test, but within we need manual set
	os.Setenv("PATH", "/usr/bin:/bin")
	r2 := applyInitPath(exe, home, "/bin/bash", "")
	if r2.State != "already set" || len(r2.Changed) != 0 {
		t.Fatalf("second: %+v want already set no changes", r2)
	}
	countMarkers(t, home, ".profile", ".bashrc")
}

func TestApplyInitPathAlreadyOnPathNoFileChange(t *testing.T) {
	home := t.TempDir()
	exe := setupFakeExe(t, home)
	binDir := filepath.Dir(exe)
	t.Setenv("PATH", binDir+":/usr/bin")
	r := applyInitPath(exe, home, "/bin/bash", "")
	if r.State != "already set" {
		t.Fatalf("%+v want already set", r)
	}
	for _, n := range []string{".profile", ".bashrc"} {
		if _, err := os.Stat(filepath.Join(home, n)); !os.IsNotExist(err) {
			t.Errorf("%s should not have been created when already on PATH", n)
		}
	}
	if r.Resolved != exe {
		t.Errorf("Resolved=%q want %q", r.Resolved, exe)
	}
}

func TestApplyInitPathZshProfileChoice(t *testing.T) {
	home := t.TempDir()
	exe := setupFakeExe(t, home)
	os.Setenv("PATH", "/usr/bin")
	r := applyInitPath(exe, home, "/bin/zsh", "")
	if r.State != "profile updated" {
		t.Fatalf("%+v", r)
	}
	countMarkers(t, home, ".profile", ".zshrc")
	if _, err := os.Stat(filepath.Join(home, ".bashrc")); !os.IsNotExist(err) {
		t.Error(".bashrc should not be written for zsh")
	}
}

func TestApplyInitPathHomeUnsetWarnsNoCrash(t *testing.T) {
	exe := "/tmp/whatever/.local/bin/minifactory"
	// Use non-empty exe parent; (b) still applies, profile skipped
	os.Setenv("PATH", "/usr/bin")
	r := applyInitPath(exe, "", "/bin/bash", "")
	if r.State != "warning" || !strings.Contains(r.Detail, "HOME") {
		t.Fatalf("%+v want HOME warning", r)
	}
	if !pathContainsDir(os.Getenv("PATH"), filepath.Dir(exe)) {
		t.Errorf("(b) should still prepend bin dir when HOME unset, PATH=%q", os.Getenv("PATH"))
	}
	if r.manualExportLine() == "" {
		t.Error("manual export line missing")
	}
}

func TestApplyInitPathNonstandardBinDirNoProfileWrite(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	exe := filepath.Join(dir, "minifactory")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Setenv("PATH", "/usr/bin")
	r := applyInitPath(exe, home, "/bin/bash", "")
	if r.State != "skipped" || !strings.Contains(r.Detail, "outside the standard") {
		t.Fatalf("%+v want nonstandard skipped", r)
	}
	for _, n := range []string{".profile", ".bashrc"} {
		if _, err := os.Stat(filepath.Join(home, n)); !os.IsNotExist(err) {
			t.Errorf("%s must not be written for nonstandard bin dir", n)
		}
	}
	// (b) still applies for this run
	if !pathContainsDir(os.Getenv("PATH"), dir) {
		t.Errorf("(b) PATH missing nonstandard bin dir")
	}
}

func TestApplyInitPathBinDirOverride(t *testing.T) {
	home := t.TempDir()
	custom := filepath.Join(t.TempDir(), "custombin")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(custom, "minifactory")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Setenv("PATH", "/usr/bin")
	r := applyInitPath(exe, home, "/bin/bash", custom)
	if r.State != "profile updated" || !r.Standard {
		t.Fatalf("%+v want profile updated for BIN_DIR override", r)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".profile"))
	if !strings.Contains(string(b), `export PATH="`+custom+`:$PATH"`) {
		t.Errorf("override export line missing:\n%s", b)
	}
	if r.manualExportLine() != `export PATH="`+custom+`:$PATH"` {
		t.Errorf("manual export=%q", r.manualExportLine())
	}
}

func TestApplyInitPathProfileNotWritableWarns(t *testing.T) {
	home := t.TempDir()
	exe := setupFakeExe(t, home)
	// .profile as a directory makes ReadFile fail (not IsNotExist)
	if err := os.Mkdir(filepath.Join(home, ".profile"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Setenv("PATH", "/usr/bin")
	r := applyInitPath(exe, home, "/bin/bash", "")
	if r.State != "warning" || r.Err == nil {
		t.Fatalf("%+v want warning with Err", r)
	}
	if !strings.Contains(r.manualExportLine(), "export PATH=") {
		t.Errorf("manual export missing: %q", r.manualExportLine())
	}
}

func TestApplyInitPathDifferentResolvedWarnsLegacy(t *testing.T) {
	home := t.TempDir()
	exe := setupFakeExe(t, home)
	oldLegacy := legacyBinaryExists
	legacyBinaryExists = func() bool { return true }
	defer func() { legacyBinaryExists = oldLegacy }()
	oldLook := initLookPath
	initLookPath = func(name string) (string, error) { return legacyBinaryPath, nil }
	defer func() { initLookPath = oldLook }()
	os.Setenv("PATH", "/usr/bin")
	r := applyInitPath(exe, home, "/bin/bash", "")
	if !strings.Contains(r.Warning, legacyBinaryPath) || !strings.Contains(r.Warning, "rm "+legacyBinaryPath) {
		t.Errorf("warning should reuse legacy wording, got %q", r.Warning)
	}
	if r.Resolved != legacyBinaryPath {
		t.Errorf("Resolved=%q", r.Resolved)
	}
}

func TestInitSkipPathDoesNotCallEnsurer(t *testing.T) {
	called := false
	old := ensurePathInInitFn
	ensurePathInInitFn = func() pathResult { called = true; return pathResult{State: "already set"} }
	defer func() { ensurePathInInitFn = old }()
	dir := filepath.Join(t.TempDir(), "factory")
	if rc := initFactoryWithOptions(dir, true, true); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if called {
		t.Error("--skip-path called the PATH ensurer")
	}
	// And via cmdInit flag parsing
	called = false
	dir2 := filepath.Join(t.TempDir(), "factory")
	if rc := cmdInit([]string{"--dir", dir2, "--skip-path", "--skip-deps"}); rc != 0 {
		t.Fatalf("cmdInit rc=%d", rc)
	}
	if called {
		t.Error("cmdInit --skip-path called the PATH ensurer")
	}
}

func TestEnsurePathInInitUsesExeSeamAndTempHome(t *testing.T) {
	home := t.TempDir()
	exe := setupFakeExe(t, home)
	// symlink to exe to prove EvalSymlinks handling
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "minifactory-link")
	if err := os.Symlink(exe, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	oldExe := initExecutable
	initExecutable = func() (string, error) { return link, nil }
	defer func() { initExecutable = oldExe }()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("BIN_DIR", "")
	os.Setenv("PATH", "/usr/bin")
	r := ensurePathInInit()
	if r.Exe != exe {
		t.Errorf("Exe=%q want resolved %q", r.Exe, exe)
	}
	if r.State != "profile updated" {
		t.Fatalf("%+v want profile updated", r)
	}
	countMarkers(t, home, ".profile", ".bashrc")
}
