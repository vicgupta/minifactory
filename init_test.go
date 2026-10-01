package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitFreshDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "factory")
	if rc := initFactory(target); rc != 0 {
		t.Fatalf("initFactory() = %d, want 0", rc)
	}
	for _, d := range []string{"data", "logs", "work"} {
		if st, err := os.Stat(filepath.Join(target, d)); err != nil || !st.IsDir() {
			t.Errorf("expected directory %s to exist", d)
		}
	}
	q, err := os.ReadFile(filepath.Join(target, "data", "queue.json"))
	if err != nil || string(q) != "[]\n" {
		t.Errorf("queue.json = %q, %v; want %q", q, err, "[]\n")
	}
	envPath := filepath.Join(target, ".env")
	st, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("expected .env to exist: %v", err)
	}
	if st.Mode().Perm() != 0600 {
		t.Errorf(".env mode = %o, want 600", st.Mode().Perm())
	}
	b, _ := os.ReadFile(envPath)
	for _, k := range []string{"GITHUB_TOKEN=", "GITHUB_REPO=", "CLAUDE_CODE_OAUTH_TOKEN=", "CODEX_TOKEN=", "OPENCODE_TOKEN=", "MAX_TURNS="} {
		if !strings.Contains(string(b), k) {
			t.Errorf(".env template missing key %q", k)
		}
	}
	// The freshly written template rereads to all-empty values.
	cfg, found := loadEnvFrom(target)
	if !found {
		t.Fatal("loadEnvFrom: expected the fresh .env to be found")
	}
	for _, k := range []string{"GITHUB_TOKEN", "GITHUB_REPO", "CLAUDE_CODE_OAUTH_TOKEN", "CODEX_TOKEN", "OPENCODE_TOKEN", "MAX_TURNS"} {
		if v, ok := cfg[k]; !ok || v != "" {
			t.Errorf("loadEnvFrom(%q) = %q, want empty", k, v)
		}
	}
}

func TestInitBacksUpOldEnv(t *testing.T) {
	dir := t.TempDir()
	if rc := initFactory(dir); rc != 0 {
		t.Fatalf("first initFactory() = %d, want 0", rc)
	}
	// Simulate a configured .env, then re-init.
	old := "GITHUB_TOKEN=secret123\nGITHUB_REPO=owner/repo\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if rc := initFactory(dir); rc != 0 {
		t.Fatalf("second initFactory() = %d, want 0", rc)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".env.bak.*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one backup, got %v, %v", matches, err)
	}
	bak, _ := os.ReadFile(matches[0])
	if string(bak) != old {
		t.Errorf("backup content = %q, want original %q", bak, old)
	}
	if st, _ := os.Stat(matches[0]); st.Mode().Perm() != 0600 {
		t.Errorf("backup mode = %o, want 600", st.Mode().Perm())
	}
	fresh, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if strings.Contains(string(fresh), "secret123") {
		t.Error("fresh .env still contains the old secret")
	}
	if !strings.Contains(string(fresh), "GITHUB_TOKEN=") {
		t.Error("fresh .env missing GITHUB_TOKEN key")
	}
}

func TestInitBacksUpAndResetsQueue(t *testing.T) {
	dir := t.TempDir()
	qpath := filepath.Join(dir, "data", "queue.json")
	if err := os.MkdirAll(filepath.Dir(qpath), 0755); err != nil {
		t.Fatal(err)
	}
	existing := `[{"id":"abc","state":"done"}]` + "\n"
	if err := os.WriteFile(qpath, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	if rc := initFactory(dir); rc != 0 {
		t.Fatalf("initFactory() = %d, want 0", rc)
	}
	// The old queue is preserved in a timestamped backup ...
	matches, err := filepath.Glob(filepath.Join(dir, "data", "queue.json.bak.*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one queue backup, got %v, %v", matches, err)
	}
	bak, _ := os.ReadFile(matches[0])
	if string(bak) != existing {
		t.Errorf("queue backup = %q, want original %q", bak, existing)
	}
	if st, _ := os.Stat(matches[0]); st.Mode().Perm() != 0644 {
		t.Errorf("queue backup mode = %o, want 644", st.Mode().Perm())
	}
	// ... and the live queue is reset to empty.
	q, _ := os.ReadFile(qpath)
	if string(q) != "[]\n" {
		t.Errorf("queue.json = %q, want %q (reset)", q, "[]\n")
	}
}

func TestInitRefusesLiveStateWithoutForce(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("GITHUB_TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if rc := cmdInit([]string{"--dir", dir}); rc != 2 {
		t.Fatalf("cmdInit() without --force = %d, want 2 (refusal)", rc)
	}
	// .env must be untouched.
	b, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if string(b) != "GITHUB_TOKEN=secret\n" {
		t.Errorf(".env was modified by refused init: %q", b)
	}
}

func TestInitForceProceedsOnLiveState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("GITHUB_TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if rc := cmdInit([]string{"--dir", dir, "--force"}); rc != 0 {
		t.Fatalf("cmdInit() with --force = %d, want 0", rc)
	}
}

func TestInitAllowsFreshDirWithoutForce(t *testing.T) {
	dir := t.TempDir()
	if rc := cmdInit([]string{"--dir", dir}); rc != 0 {
		t.Fatalf("cmdInit() on fresh dir = %d, want 0", rc)
	}
}

func TestInitWouldClobberEmptyTemplate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("GITHUB_TOKEN=\nGITHUB_REPO=\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if atRisk := initWouldClobber(dir); len(atRisk) != 0 {
		t.Errorf("initWouldClobber(empty template) = %v, want none", atRisk)
	}
}
