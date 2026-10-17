package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultBinaryPathIsUserLocal(t *testing.T) {
	if got, want := defaultBinaryPath("/home/u"), "/home/u/.local/bin/minifactory"; got != want {
		t.Errorf("defaultBinaryPath = %q, want %q", got, want)
	}
	if got := defaultBinDir("/home/u"); got != "/home/u/.local/bin" {
		t.Errorf("defaultBinDir = %q", got)
	}
	for _, bad := range []string{"/usr/local/bin", "/usr/bin"} {
		if strings.HasPrefix(defaultBinaryPath("/home/u"), bad) {
			t.Errorf("default binary path %q is in protected dir %q", defaultBinaryPath("/home/u"), bad)
		}
	}
}

func TestResolveBinDirOverrideWins(t *testing.T) {
	if got := resolveBinDir("/opt/custom/bin", "/home/u"); got != "/opt/custom/bin" {
		t.Errorf("explicit override lost: %q", got)
	}
	if got := resolveBinDir("", "/home/u"); got != "/home/u/.local/bin" {
		t.Errorf("empty override should default to user-local, got %q", got)
	}
	if got := resolveBinDir("  ", "/home/u"); got != "/home/u/.local/bin" {
		t.Errorf("blank override should default to user-local, got %q", got)
	}
}

func TestPathContainsDir(t *testing.T) {
	p := "/usr/bin:/home/u/.local/bin:/bin"
	if !pathContainsDir(p, "/home/u/.local/bin") {
		t.Error("expected ~/.local/bin to be found on PATH")
	}
	if pathContainsDir(p, "/home/u/bin") {
		t.Error("unexpected PATH hit")
	}
	if pathContainsDir("", "/home/u/.local/bin") {
		t.Error("empty PATH must not match")
	}
}

func TestEnsurePathBlockIdempotent(t *testing.T) {
	once, added := ensurePathBlock("export EDITOR=vim\n")
	if !added {
		t.Fatal("first run should add the block")
	}
	if strings.Count(once, profileMarker) != 1 || strings.Count(once, pathExportLine) != 1 {
		t.Fatalf("want exactly one marked block:\n%s", once)
	}
	twice, added2 := ensurePathBlock(once)
	if added2 || twice != once {
		t.Errorf("second run must be a no-op (added=%v):\n%s", added2, twice)
	}
	// A profile that already exports the line by hand is left alone.
	pre := "export PATH=\"$HOME/.local/bin:$PATH\"\n"
	if out, added := ensurePathBlock(pre); added || out != pre {
		t.Errorf("pre-existing export must be a no-op, added=%v out=%q", added, out)
	}
}

func TestEnsurePathInProfilesIdempotentAndShellChoice(t *testing.T) {
	home := t.TempDir()
	changed, err := ensurePathInProfiles(home, "/bin/bash")
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 {
		t.Fatalf("bash: want .profile + .bashrc changed, got %v", changed)
	}
	changed2, err := ensurePathInProfiles(home, "/bin/bash")
	if err != nil {
		t.Fatal(err)
	}
	if len(changed2) != 0 {
		t.Errorf("second run must change nothing, got %v", changed2)
	}
	for _, name := range []string{".profile", ".bashrc"} {
		b, _ := os.ReadFile(filepath.Join(home, name))
		if strings.Count(string(b), profileMarker) != 1 {
			t.Errorf("%s: want exactly one marker:\n%s", name, b)
		}
	}
	zhome := t.TempDir()
	if _, err := ensurePathInProfiles(zhome, "/bin/zsh"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(zhome, ".zshrc")); err != nil {
		t.Errorf("zsh profile not written: %v", err)
	}
	if got := profilePaths("/home/u", "/bin/zsh"); len(got) != 2 || got[1] != "/home/u/.zshrc" {
		t.Errorf("profilePaths zsh = %v", got)
	}
}

func TestAssessBinaryLocation(t *testing.T) {
	home := "/home/u"
	if st, _ := assessBinaryLocation("/home/u/.local/bin/minifactory", home, "/usr/bin:/home/u/.local/bin"); st != statusOK {
		t.Errorf("user-local on PATH should be ok, got %s", st)
	}
	if st, d := assessBinaryLocation(legacyBinaryPath, home, "/usr/local/bin:/usr/bin"); st != statusWarn || !strings.Contains(d, "/home/u/.local/bin") {
		t.Errorf("legacy location should warn toward user-local, got %s: %s", st, d)
	}
	if st, _ := assessBinaryLocation("/home/u/.local/bin/minifactory", home, "/usr/bin"); st != statusWarn {
		t.Errorf("user-local off PATH should warn, got %s", st)
	}
}

func TestLegacyBinaryWarning(t *testing.T) {
	if w := legacyBinaryWarning("/home/u/.local/bin/minifactory", true); !strings.Contains(w, legacyBinaryPath) || !strings.Contains(w, "rm "+legacyBinaryPath) {
		t.Errorf("want removal guidance for stale legacy copy, got %q", w)
	}
	if w := legacyBinaryWarning(legacyBinaryPath, true); w != "" {
		t.Errorf("running from legacy path is handled by assess, not the stale-copy warning: %q", w)
	}
	if w := legacyBinaryWarning("/home/u/.local/bin/minifactory", false); w != "" {
		t.Errorf("no legacy copy, want no warning, got %q", w)
	}
}

func TestCronBlockUsesAbsoluteUserLocalPath(t *testing.T) {
	exe := defaultBinaryPath("/home/u")
	b := cronBlock("* * * * *", exe, "/home/u/.minifactory")
	if !strings.Contains(b, "/home/u/.local/bin/minifactory run-once") {
		t.Errorf("cron block must call the absolute user-local binary:\n%s", b)
	}
	if strings.Contains(b, "/usr/local/bin") {
		t.Errorf("cron block must never point at /usr/local/bin:\n%s", b)
	}
}

func TestSystemdUnitUsesUserLocalBinary(t *testing.T) {
	b, err := os.ReadFile("minifactory-go.service")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, ".local/bin/minifactory") {
		t.Errorf("systemd ExecStart must call the user-local binary by absolute path:\n%s", s)
	}
	if strings.Contains(s, "/usr/local/bin") || strings.Contains(s, "/root/minifactory-go/minifactory") {
		t.Errorf("systemd unit still points at the old binary location:\n%s", s)
	}
}

func TestNoUsrLocalDefaultInInstallerAndDocs(t *testing.T) {
	// The legacy path may be *mentioned* for the stale-copy warning, but it
	// must never appear as an install destination or default.
	for _, f := range []string{"install.sh", "README.md", "minifactory-go.service"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"cp minifactory /usr/local/bin", "cp minifactory ~/.local/bin/   # or /usr/local/bin", "BIN_DIR:-/usr/local/bin", ":-/usr/local/bin"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %q as an install default/destination", f, bad)
			}
		}
	}
	inst, _ := os.ReadFile("install.sh")
	if !strings.Contains(string(inst), ".local/bin") {
		t.Error("install.sh must default the binary to ~/.local/bin (BIN_DIR)")
	}
}

func TestPipedInstallerContract(t *testing.T) {
	b, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"curl -fsSL https://raw.githubusercontent.com/vicgupta/minifactory/main/install.sh | sh",
		"api.github.com/repos/$REPO/releases/latest",
		"minifactory-${ver}-${os}-${arch}.tar.gz",
		"MINIFACTORY_SKIP_INIT",
		"MINIFACTORY_RELEASE_TAG",
		"/dev/tty",
		"--skip-deps",
		"checksum mismatch",
		"unsupported OS",
		"unsupported architecture",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("install.sh missing piped-installer contract %q", want)
		}
	}
	if !strings.HasPrefix(s, "#!/bin/sh") {
		t.Error("install.sh must start with a POSIX sh shebang for curl|sh (dash)")
	}
	// Download placement must be the user-local default, never sudo/cp into
	// a system bin dir.
	for _, bad := range []string{"sudo cp", "cp \"$1\" /usr", "BIN_DIR:-/usr"} {
		if strings.Contains(s, bad) {
			t.Errorf("install.sh contains forbidden placement %q", bad)
		}
	}
	readme, _ := os.ReadFile("README.md")
	if !strings.Contains(string(readme), "curl -fsSL https://raw.githubusercontent.com/vicgupta/minifactory/main/install.sh | sh") {
		t.Error("README must publish the exact one-liner")
	}
}
