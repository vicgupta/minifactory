package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain stubs the init dependency step for the whole test binary so no
// test (initFactory, cmdInit, ...) can trigger a real package install on
// the host running the tests. Deps logic itself is tested via fake depEnvs.
func TestMain(m *testing.M) {
	ensureInitDeps = func(bool) []depStatus {
		return []depStatus{
			{"git", depOK, "git version 0.0.0 (test stub)"},
			{"docker", depOK, "Docker version 0.0.0 (test stub); daemon reachable"},
		}
	}
	os.Exit(m.Run())
}

type fakeHost struct {
	present      map[string]bool
	daemon       bool
	failInstall  bool
	installWorks bool
	startWorks   bool
	installs     [][]string
	probes       []string
}

func newFakeHost(present ...string) *fakeHost {
	f := &fakeHost{present: map[string]bool{}, installWorks: true, startWorks: true}
	for _, p := range present {
		f.present[p] = true
	}
	return f
}

func (f *fakeHost) env(goos string, euid int, osRelease string) depEnv {
	var out bytes.Buffer
	look := func(name string) (string, error) {
		// Package/service managers count as present unless explicitly faked out
		// via the present map conventions used in tests: apt-get/dnf/sudo are
		// present when listed, systemctl/service likewise.
		if f.present[name] {
			return "/fake/" + name, nil
		}
		return "", fmt.Errorf("not found")
	}
	probe := func(_ time.Duration, name string, args ...string) (int, string) {
		f.probes = append(f.probes, name+" "+strings.Join(args, " "))
		switch {
		case name == "git" && f.present["git"]:
			return 0, "git version 2.99.0\n"
		case name == "docker" && len(args) > 0 && args[0] == "--version" && f.present["docker"]:
			return 0, "Docker version 99.0.0, build fake\n"
		case name == "docker" && len(args) > 0 && args[0] == "info" && f.present["docker"] && f.daemon:
			return 0, "Server Version: 99.0.0\n"
		}
		return 1, "fake failure"
	}
	install := func(name string, args ...string) (int, string) {
		full := append([]string{name}, args...)
		f.installs = append(f.installs, full)
		if f.failInstall {
			return 1, "fake install failure"
		}
		joined := strings.Join(full, " ")
		if f.installWorks {
			if strings.Contains(joined, " install ") && strings.HasSuffix(joined, " git") {
				f.present["git"] = true
			}
			if strings.Contains(joined, " install ") && (strings.HasSuffix(joined, " docker.io") || strings.HasSuffix(joined, " docker")) {
				f.present["docker"] = true
			}
		}
		if (strings.Contains(joined, "systemctl") || strings.Contains(joined, "service docker start")) && f.startWorks {
			if f.present["docker"] {
				f.daemon = true
			}
		}
		return 0, ""
	}
	return depEnv{
		goos:       goos,
		lookPath:   look,
		run:        probe,
		runInstall: install,
		readFile:   func(string) ([]byte, error) { return []byte(osRelease), nil },
		euid:       euid,
		out:        &out,
	}
}

const debianRelease = "ID=debian\nID_LIKE=\n"
const ubuntuRelease = "ID=ubuntu\nID_LIKE=debian\n"
const fedoraRelease = "ID=fedora\nID_LIKE=rhel\n"
const alpineRelease = "ID=alpine\n"

func TestDepsAlreadyPresentNoInstall(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.daemon = true
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if len(f.installs) != 0 {
		t.Fatalf("installs attempted for present tools: %v", f.installs)
	}
	if got[0].State != depOK || got[1].State != depOK {
		t.Fatalf("statuses = %+v, want both ok", got)
	}
	if !strings.Contains(got[0].Detail, "2.99.0") || !strings.Contains(got[1].Detail, "daemon reachable") {
		t.Errorf("details missing versions/daemon: %+v", got)
	}
}

func TestDepsMissingInstallSucceeds(t *testing.T) {
	f := newFakeHost("sudo", "apt-get", "systemctl")
	got := ensureDeps(f.env("linux", 1000, ubuntuRelease))
	if got[0].State != depInstalled || got[1].State != depInstalled {
		t.Fatalf("statuses = %+v, want both installed", got)
	}
	joined := fmt.Sprint(f.installs)
	for _, want := range []string{"sudo apt-get update", "sudo apt-get install -y git", "sudo apt-get install -y docker.io", "systemctl"} {
		if !strings.Contains(joined, want) {
			t.Errorf("install commands %v missing %q", f.installs, want)
		}
	}
	if strings.Count(joined, "apt-get update") != 1 {
		t.Errorf("apt-get update should run once, got %v", f.installs)
	}
}

func TestDepsInstallFailsWarns(t *testing.T) {
	f := newFakeHost("sudo", "apt-get")
	f.failInstall = true
	got := ensureDeps(f.env("linux", 1000, debianRelease)) // must not panic
	if got[0].State != depMissing || got[1].State != depMissing {
		t.Fatalf("statuses = %+v, want both still missing", got)
	}
	if !strings.Contains(got[0].Detail, "install command failed") {
		t.Errorf("git detail = %q, want install-failure note", got[0].Detail)
	}
}

func TestDepsDarwinInstructsNoInstall(t *testing.T) {
	f := newFakeHost() // nothing present; any sudo/apt attempt would be recorded
	got := ensureDeps(f.env("darwin", 1000, ""))
	if len(f.installs) != 0 {
		t.Fatalf("macOS attempted installs: %v", f.installs)
	}
	if got[0].State != depMissing || !strings.Contains(got[0].Detail, "xcode-select --install") {
		t.Errorf("git = %+v, want Xcode CLT instruction", got[0])
	}
	if got[1].State != depMissing || !strings.Contains(got[1].Detail, "Docker Desktop") {
		t.Errorf("docker = %+v, want Docker Desktop instruction", got[1])
	}
}

func TestDepsNoSudoPrintsManualCommands(t *testing.T) {
	f := newFakeHost("apt-get") // non-root, no sudo
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if len(f.installs) != 0 {
		t.Fatalf("installs attempted without sudo: %v", f.installs)
	}
	if got[0].State != depMissing || !strings.Contains(got[0].Detail, "apt-get install -y git") {
		t.Errorf("git = %+v, want manual apt command", got[0])
	}
	if got[1].State != depMissing || !strings.Contains(got[1].Detail, "apt-get install -y docker.io") {
		t.Errorf("docker = %+v, want manual apt command", got[1])
	}
}

func TestDepsUnsupportedDistroInstructs(t *testing.T) {
	f := newFakeHost("sudo")
	got := ensureDeps(f.env("linux", 0, alpineRelease))
	if len(f.installs) != 0 {
		t.Fatalf("unsupported distro attempted installs: %v", f.installs)
	}
	for _, st := range got {
		if st.State != depMissing {
			t.Errorf("%s = %+v, want still missing with manual instruction", st.Tool, st)
		}
	}
}

func TestDepsDockerDaemonDownStarts(t *testing.T) {
	f := newFakeHost("git", "docker", "sudo", "systemctl")
	f.daemon = false
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[1].State != depOK || !strings.Contains(got[1].Detail, "daemon started") {
		t.Fatalf("docker = %+v, want ok after daemon start", got[1])
	}
}

func TestDepsDockerDaemonStaysDown(t *testing.T) {
	f := newFakeHost("git", "docker", "sudo", "systemctl")
	f.daemon = false
	f.startWorks = false
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[1].State != depNoDaemon || !got[1].outstanding() {
		t.Fatalf("docker = %+v, want daemon-not-running outstanding", got[1])
	}
}

func TestDepsRHELUsesDnf(t *testing.T) {
	f := newFakeHost("dnf", "systemctl") // root: no sudo prefix needed
	got := ensureDeps(f.env("linux", 0, fedoraRelease))
	if got[0].State != depInstalled || got[1].State != depInstalled {
		t.Fatalf("statuses = %+v, want installed via dnf", got)
	}
	joined := fmt.Sprint(f.installs)
	if !strings.Contains(joined, "dnf install -y git") || !strings.Contains(joined, "dnf install -y docker") {
		t.Errorf("installs = %v, want dnf git+docker", f.installs)
	}
	if strings.Contains(joined, "sudo") {
		t.Errorf("root installs should not use sudo: %v", f.installs)
	}
}

func TestParseOSReleaseFamily(t *testing.T) {
	for in, want := range map[string]string{
		debianRelease: "debian", ubuntuRelease: "debian",
		fedoraRelease: "rhel", "ID=rocky\nID_LIKE=\"rhel centos fedora\"\n": "rhel",
		alpineRelease: "", "": "",
	} {
		if got := parseOSReleaseFamily(in); got != want {
			t.Errorf("parseOSReleaseFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInitSkipDepsDoesNotCallEnsurer(t *testing.T) {
	called := false
	old := ensureInitDeps
	ensureInitDeps = func(bool) []depStatus { called = true; return nil }
	defer func() { ensureInitDeps = old }()
	dir := filepath.Join(t.TempDir(), "factory")
	if rc := initFactoryWithDeps(dir, true); rc != 0 {
		t.Fatalf("initFactoryWithDeps(skip) = %d, want 0", rc)
	}
	if called {
		t.Error("--skip-deps called the dependency ensurer")
	}
}

func TestInitMissingDepsStillSucceeds(t *testing.T) {
	old := ensureInitDeps
	ensureInitDeps = func(bool) []depStatus {
		return []depStatus{{"docker", depMissing, "fake missing"}}
	}
	defer func() { ensureInitDeps = old }()
	dir := filepath.Join(t.TempDir(), "factory")
	if rc := initFactory(dir); rc != 0 {
		t.Fatalf("initFactory with missing deps = %d, want 0 (warn-and-continue)", rc)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err != nil {
		t.Errorf(".env not written despite missing deps: %v", err)
	}
}
