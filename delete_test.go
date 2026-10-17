package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePasswd(t *testing.T) {
	text := "root:x:0:0:root:/root:/bin/bash\n" +
		"minfactory:x:995:983::/home/minfactory:/usr/sbin/nologin\n" +
		"vic:x:1000:1000::/home/vic:/bin/bash\n" +
		"badline\n"
	users := parsePasswd(text)
	if len(users) != 3 {
		t.Fatalf("got %d users, want 3", len(users))
	}
	if users[1].name != "minfactory" || users[1].home != "/home/minfactory" || users[1].uid != 995 {
		t.Errorf("unexpected parse: %+v", users[1])
	}
}

func TestIsNologinShell(t *testing.T) {
	for _, s := range []string{"/usr/sbin/nologin", "nologin", "/bin/false", "false"} {
		if !isNologinShell(s) {
			t.Errorf("isNologinShell(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"/bin/bash", "/bin/sh", ""} {
		if isNologinShell(s) {
			t.Errorf("isNologinShell(%q) = true, want false", s)
		}
	}
}

func TestManagedDeleteEntries(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"data", "logs", "work"} {
		if err := os.Mkdir(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{".env", ".env.bak.20261017-120000", "cycle.lock", "minifactory", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := managedDeleteEntries(dir)
	has := func(suffix string) bool {
		for _, p := range got {
			if strings.HasSuffix(p, suffix) {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"data", "logs", "work", ".env", ".env.bak.20261017-120000", "cycle.lock"} {
		if !has(want) {
			t.Errorf("managedDeleteEntries missing %q: %v", want, got)
		}
	}
	for _, notWant := range []string{"minifactory", "notes.txt"} {
		if has(notWant) {
			t.Errorf("managedDeleteEntries must not include %q: %v", notWant, got)
		}
	}
}

func TestDeletePlanEmpty(t *testing.T) {
	dir := t.TempDir()
	p := planDeleteAll(dir, false, "")
	if !p.empty() {
		t.Errorf("fresh temp dir should plan nothing: %+v", p)
	}
}
