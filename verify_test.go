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

func TestChangedFilesDiff(t *testing.T) {
	dir := gitRepo(t)
	// Agent changes: modify a.txt, add b.txt (untracked).
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644)

	claimed := []any{"a.txt", "b.txt"}
	onlyClaimed, onlyActual, err := changedFilesDiff(dir, claimed, false)
	if err != nil || len(onlyClaimed) != 0 || len(onlyActual) != 0 {
		t.Errorf("exact claim: onlyClaimed=%v onlyActual=%v err=%v", onlyClaimed, onlyActual, err)
	}
	// Normalization: ./ prefix and redundant segments are cleaned.
	onlyClaimed, onlyActual, _ = changedFilesDiff(dir, []any{"./a.txt", "sub/../b.txt"}, false)
	if len(onlyClaimed) != 0 || len(onlyActual) != 0 {
		t.Errorf("normalized claim should match: onlyClaimed=%v onlyActual=%v", onlyClaimed, onlyActual)
	}
	// Missing entry -> surfaces in onlyActual.
	_, onlyActual, _ = changedFilesDiff(dir, []any{"a.txt"}, false)
	if len(onlyActual) != 1 || onlyActual[0] != "b.txt" {
		t.Errorf("claim missing b.txt: onlyActual=%v, want [b.txt]", onlyActual)
	}
	// Extra entry -> surfaces in onlyClaimed.
	onlyClaimed, _, _ = changedFilesDiff(dir, []any{"a.txt", "b.txt", "c.txt"}, false)
	if len(onlyClaimed) != 1 || onlyClaimed[0] != "c.txt" {
		t.Errorf("claim with extra c.txt: onlyClaimed=%v, want [c.txt]", onlyClaimed)
	}
	// Wrong entry -> claimed-only; the real b.txt -> git-only.
	onlyClaimed, onlyActual, _ = changedFilesDiff(dir, []any{"a.txt", "zzz.txt"}, false)
	if len(onlyClaimed) != 1 || onlyClaimed[0] != "zzz.txt" || len(onlyActual) != 1 || onlyActual[0] != "b.txt" {
		t.Errorf("claim naming zzz.txt: onlyClaimed=%v onlyActual=%v", onlyClaimed, onlyActual)
	}
	// Non-list claim -> nothing claimed, both files in onlyActual.
	_, onlyActual, _ = changedFilesDiff(dir, "a.txt", false)
	if len(onlyActual) != 2 {
		t.Errorf("non-list claim: onlyActual=%v, want both files", onlyActual)
	}
	// Non-string entry is ignored (a.txt + 42 => 42 dropped).
	_, onlyActual, _ = changedFilesDiff(dir, []any{"a.txt", 42}, false)
	if len(onlyActual) != 1 || onlyActual[0] != "b.txt" {
		t.Errorf("non-string entry: onlyActual=%v, want [b.txt]", onlyActual)
	}
	// Factory metadata files are invisible to the check.
	os.WriteFile(filepath.Join(dir, "task.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "result.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "size.json"), []byte("{}"), 0o644)
	onlyClaimed, onlyActual, _ = changedFilesDiff(dir, claimed, false)
	if len(onlyClaimed) != 0 || len(onlyActual) != 0 {
		t.Errorf("factory metadata must be excluded: onlyClaimed=%v onlyActual=%v", onlyClaimed, onlyActual)
	}
}

func TestChangedFilesDiffGitignoreTouched(t *testing.T) {
	dir := gitRepo(t)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a2\n"), 0o644)
	// Orchestrator hygiene touched .gitignore; the agent didn't.
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("__pycache__/\n"), 0o644)
	// gitignoreTouched=false: .gitignore surfaces as a git-only change.
	_, onlyActual, _ := changedFilesDiff(dir, []any{"a.txt"}, false)
	if len(onlyActual) != 1 || onlyActual[0] != ".gitignore" {
		t.Errorf("untouched-.gitignore: onlyActual=%v, want [.gitignore]", onlyActual)
	}
	// gitignoreTouched=true: .gitignore is excluded, so the sets match.
	onlyClaimed, onlyActual, _ := changedFilesDiff(dir, []any{"a.txt"}, true)
	if len(onlyClaimed) != 0 || len(onlyActual) != 0 {
		t.Errorf("touched .gitignore must be excluded: onlyClaimed=%v onlyActual=%v", onlyClaimed, onlyActual)
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
