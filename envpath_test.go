package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func existsIn(paths ...string) func(string) bool {
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	return func(p string) bool { return set[p] }
}

func TestResolveEnvFilePrecedence(t *testing.T) {
	cwd := "/work/vps"
	home := "/home/u"
	none := existsIn()
	withCwdEnv := existsIn(filepath.Join(cwd, ".env"))

	cases := []struct {
		name      string
		dirOver   string
		homeOver  string
		exists    func(string) bool
		cwd, home string
		want      string
	}{
		{"override MINIFACTORY_DIR wins over cwd and home", "/srv/factory", "/elsewhere", withCwdEnv, cwd, home, "/srv/factory/.env"},
		{"MINIFACTORY_HOME wins over cwd and default home", "", "/srv/mf-home", withCwdEnv, cwd, home, "/srv/mf-home/.env"},
		{"cwd .env beats default home", "", "", withCwdEnv, cwd, home, "/work/vps/.env"},
		{"default is ~/.minifactory/.env", "", "", none, cwd, home, "/home/u/.minifactory/.env"},
		{"HOME unset falls back to cwd .env", "", "", none, cwd, "", "/work/vps/.env"},
		{"HOME unset and no cwd still falls back to cwd, not exe dir", "", "", none, cwd, "", "/work/vps/.env"},
	}
	for _, c := range cases {
		if got := resolveEnvFileWith(c.cwd, c.home, c.dirOver, c.homeOver, c.exists); got != c.want {
			t.Errorf("%s: resolveEnvFileWith = %q, want %q", c.name, got, c.want)
		}
		if got := resolveBaseDirWith(c.cwd, c.home, c.dirOver, c.homeOver, c.exists); got != filepath.Dir(c.want) {
			t.Errorf("%s: resolveBaseDirWith = %q, want %q", c.name, got, filepath.Dir(c.want))
		}
	}
}

func TestResolveNeverUsesExecutableDir(t *testing.T) {
	// The resolver takes no executable path at all: with a binary that
	// would live in /usr/local/bin, the default must still be the home dir.
	got := resolveEnvFileWith("/some/cwd", "/home/u", "", "", existsIn())
	if strings.HasPrefix(got, "/usr/local/bin") || strings.Contains(got, "/usr/local/bin") {
		t.Fatalf("resolved into executable/system dir: %q", got)
	}
	if got != "/home/u/.minifactory/.env" {
		t.Fatalf("got %q, want /home/u/.minifactory/.env", got)
	}
}

func TestResolveWithRealFilesystemCwdEnv(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".env"), []byte("GITHUB_TOKEN=x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := resolveBaseDir(cwd, home); got != cwd {
		t.Errorf("resolveBaseDir with cwd .env = %q, want cwd %q", got, cwd)
	}
	// Without a cwd .env, the same home resolves to ~/.minifactory.
	cwd2 := t.TempDir()
	if got, want := resolveBaseDir(cwd2, home), filepath.Join(home, ".minifactory"); got != want {
		t.Errorf("resolveBaseDir without cwd .env = %q, want %q", got, want)
	}
}

func TestInitCreatesHomeDir0700AndEnv0600(t *testing.T) {
	target := filepath.Join(t.TempDir(), ".minifactory")
	if rc := initFactory(target); rc != 0 {
		t.Fatalf("initFactory() = %d, want 0", rc)
	}
	if st, err := os.Stat(target); err != nil || st.Mode().Perm() != 0700 {
		t.Errorf("factory dir mode = %v, %v; want 0700", st.Mode().Perm(), err)
	}
	envPath := filepath.Join(target, ".env")
	if st, err := os.Stat(envPath); err != nil || st.Mode().Perm() != 0600 {
		t.Errorf(".env mode = %v, %v; want 0600 at %s", st.Mode().Perm(), err, envPath)
	}
}
