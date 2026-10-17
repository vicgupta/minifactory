package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRepo builds a temp git repo with one committed file and returns its path.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	run("add", "a.txt")
	run("commit", "-qm", "init")
	return dir
}

func TestChangedFilesMatch(t *testing.T) {
	dir := gitRepo(t)
	// Agent changes: modify a.txt, add b.txt (untracked).
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644)

	claimed := []any{"a.txt", "b.txt"}
	if !changedFilesMatch(dir, claimed, false) {
		t.Error("exact claim should match")
	}
	// Normalization: ./ prefix and redundant segments are cleaned.
	if !changedFilesMatch(dir, []any{"./a.txt", "sub/../b.txt"}, false) {
		t.Error("normalized claim should match")
	}
	// Missing entry fails.
	if changedFilesMatch(dir, []any{"a.txt"}, false) {
		t.Error("claim missing b.txt should fail")
	}
	// Extra entry fails.
	if changedFilesMatch(dir, []any{"a.txt", "b.txt", "c.txt"}, false) {
		t.Error("claim with extra c.txt should fail")
	}
	// Wrong entry fails.
	if changedFilesMatch(dir, []any{"a.txt", "zzz.txt"}, false) {
		t.Error("claim naming an unchanged file should fail")
	}
	// Non-list claim fails.
	if changedFilesMatch(dir, "a.txt", false) {
		t.Error("non-list claim should fail")
	}
	// Non-string entry fails.
	if changedFilesMatch(dir, []any{"a.txt", 42}, false) {
		t.Error("non-string entry should fail")
	}
	// Factory metadata files are invisible to the check.
	os.WriteFile(filepath.Join(dir, "task.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "result.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "size.json"), []byte("{}"), 0o644)
	if !changedFilesMatch(dir, claimed, false) {
		t.Error("factory metadata must be excluded from the git side")
	}
}

func TestChangedFilesMatchGitignoreTouched(t *testing.T) {
	dir := gitRepo(t)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a2\n"), 0o644)
	// Orchestrator hygiene touched .gitignore; the agent didn't.
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("__pycache__/\n"), 0o644)
	if changedFilesMatch(dir, []any{"a.txt"}, false) {
		t.Error("untouched-.gitignore must count as a change when not excluded")
	}
	if !changedFilesMatch(dir, []any{"a.txt"}, true) {
		t.Error("touched .gitignore must be excluded from the check")
	}
}

func TestEnsureGitignoreReportsTouched(t *testing.T) {
	dir := t.TempDir()
	if touched := ensureGitignore(dir); !touched {
		t.Error("first call should report touched (file created)")
	}
	if touched := ensureGitignore(dir); touched {
		t.Error("second call should report untouched (nothing missing)")
	}
	// Removing one rule makes the next call touch the file again.
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("__pycache__/\n"), 0o644)
	if touched := ensureGitignore(dir); !touched {
		t.Error("call after rule removal should report touched")
	}
}
