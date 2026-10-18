package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T, review string) string {
	t.Helper()
	dir := t.TempDir()
	work = dir
	runTests = func() ([]byte, error) { return []byte("ok"), nil }
	os.WriteFile(filepath.Join(dir, "task.json"), []byte(`{"title":"t"}`), 0644)
	if review != "" {
		os.WriteFile(filepath.Join(dir, "review.json"), []byte(review), 0644)
	}
	return dir
}

func result(t *testing.T, dir string) (files []string, summary string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Changed []string `json:"changed_files"`
		Passed  bool     `json:"tests_passed"`
		Summary string   `json:"summary"`
	}
	if err := json.Unmarshal(b, &r); err != nil || !r.Passed {
		t.Fatalf("bad result: %v %s", err, b)
	}
	return r.Changed, r.Summary
}

func TestFirstPass(t *testing.T) {
	dir := setup(t, "")
	if run() != 0 {
		t.Fatal("run failed")
	}
	files, sum := result(t, dir)
	if strings.Join(files, ",") != "factory_addon.py,test_addon.py" {
		t.Fatalf("files %v", files)
	}
	if strings.Contains(sum, "REVISION") {
		t.Fatal("first pass looks like revision")
	}
	if _, err := os.Stat(filepath.Join(dir, "REVISION")); err == nil {
		t.Fatal("REVISION written on first pass")
	}
}

func TestRevisionPass(t *testing.T) {
	dir := setup(t, `{"comments":"please fix X"}`)
	for i := 1; i <= 2; i++ {
		if run() != 0 {
			t.Fatal("run failed")
		}
		_, sum := result(t, dir)
		if !strings.Contains(sum, "REVISION pass") {
			t.Fatalf("summary %q", sum)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "REVISION"))
		if !strings.Contains(string(b), "please fix X") || strings.Count(string(b), "revision pass #") != i {
			t.Fatalf("REVISION content %q", b)
		}
	}
}
