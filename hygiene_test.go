package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanPythonCache(t *testing.T) {
	dir := t.TempDir()
	// nested __pycache__ with .pyc files, plus a stray .pyc and a .pyo
	mustWrite := func(p, body string) {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(dir, "pkg", "__pycache__", "mod.cpython-312.pyc"), "x")
	mustWrite(filepath.Join(dir, "pkg", "mod.py"), "print('hi')")
	mustWrite(filepath.Join(dir, "stray.pyc"), "x")
	mustWrite(filepath.Join(dir, "old.pyo"), "x")
	mustWrite(filepath.Join(dir, "keep.txt"), "keep")

	cleanPythonCache(dir)

	for _, gone := range []string{
		filepath.Join(dir, "pkg", "__pycache__"),
		filepath.Join(dir, "stray.pyc"),
		filepath.Join(dir, "old.pyo"),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed", gone)
		}
	}
	for _, keep := range []string{
		filepath.Join(dir, "pkg", "mod.py"),
		filepath.Join(dir, "keep.txt"),
	} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("expected %s to survive: %v", keep, err)
		}
	}
}

func TestEnsureGitignore(t *testing.T) {
	dir := t.TempDir()
	ensureGitignore(dir)
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"__pycache__/", "*.py[cod]", "*$py.class"} {
		if !strings.Contains(string(b), rule) {
			t.Errorf("expected .gitignore to contain %q", rule)
		}
	}

	// second run must not duplicate rules
	ensureGitignore(dir)
	b2, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	for _, rule := range []string{"__pycache__/", "*.py[cod]", "*$py.class"} {
		if strings.Count(string(b2), rule) != 1 {
			t.Errorf("expected exactly one occurrence of %q", rule)
		}
	}

	// existing entries are preserved, missing ones appended
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, ".gitignore"), []byte("*.log\n__pycache__/\n"), 0644)
	ensureGitignore(dir2)
	b3, _ := os.ReadFile(filepath.Join(dir2, ".gitignore"))
	s := string(b3)
	if !strings.Contains(s, "*.log") {
		t.Error("existing *.log entry was lost")
	}
	if strings.Count(s, "__pycache__/") != 1 {
		t.Error("__pycache__/ was duplicated")
	}
	if !strings.Contains(s, "*.py[cod]") {
		t.Error("missing rule was not appended")
	}
}
