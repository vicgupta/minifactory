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
	ensurePathInInitFn = func() pathResult {
		return pathResult{State: "skipped", Detail: "test stub"}
	}
	os.Exit(m.Run())
}

type fakeHost struct {
	present      map[string]bool
	daemon       bool
	permDenied   bool
	failInstall  bool
	failGroupadd bool
	installWorks bool
	startWorks   bool
	installs     [][]string
	probes       []string
	user         string
	groups       []string
	groupExists  bool
	imagePresent bool
	imageVersion string
	failBuild    bool
	outBuf       *bytes.Buffer
}

func (f *fakeHost) inGroup(name string) bool {
	for _, g := range f.groups {
		if g == name {
			return true
		}
	}
	return false
}

func newFakeHost(present ...string) *fakeHost {
	f := &fakeHost{present: map[string]bool{}, installWorks: true, startWorks: true,
		user: "vic", groups: []string{"vic"}, groupExists: true, imagePresent: true,
		imageVersion: workerImageVersion}
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
		case name == "docker" && len(args) > 0 && args[0] == "info" && f.present["docker"] && f.permDenied:
			return 1, "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock\n"
		case name == "docker" && len(args) > 0 && args[0] == "info" && f.present["docker"] && f.daemon:
			return 0, "Server Version: 99.0.0\n"
		case name == "id" && len(args) == 2 && args[0] == "-nG":
			return 0, strings.Join(f.groups, " ") + "\n"
		case name == "getent" && len(args) == 2 && args[0] == "group" && args[1] == "docker":
			if f.groupExists {
				return 0, "docker:x:999:vic\n"
			}
			return 2, ""
		case name == "docker" && len(args) > 1 && args[0] == "image" && args[1] == "inspect" && f.imagePresent:
			return 0, "sha256:fakeimageid123|123456789|" + f.imageVersion + "\n"
		case name == "sg" && f.inGroup("docker"):
			return 0, "Server Version: 99.0.0\n"
		}
		return 1, "fake failure"
	}
	install := func(name string, args ...string) (int, string) {
		full := append([]string{name}, args...)
		f.installs = append(f.installs, full)
		joined0 := strings.Join(full, " ")
		if f.failGroupadd && strings.Contains(joined0, "groupadd") {
			return 1, "fake groupadd failure"
		}
		if f.failInstall {
			return 1, "fake install failure"
		}
		if strings.Contains(joined0, "docker build") {
			if f.failBuild {
				return 1, "fake build output\ndaemon exploded building image"
			}
			f.imagePresent = true
			f.imageVersion = workerImageVersion
		}
		if strings.Contains(joined0, "groupadd docker") {
			f.groupExists = true
		}
		if strings.Contains(joined0, "usermod -aG docker") && !f.inGroup("docker") {
			f.groups = append(f.groups, "docker")
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
	f.outBuf = &out
	return depEnv{
		goos:          goos,
		lookPath:      look,
		run:           probe,
		runInstall:    install,
		readFile:      func(string) ([]byte, error) { return []byte(osRelease), nil },
		euid:          euid,
		username:      func() string { return f.user },
		workerContext: func() (string, func(), error) { return "/fake/worker", func() {}, nil },
		out:           &out,
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
	if got[0].State != depOK || got[1].State != depReady {
		t.Fatalf("statuses = %+v, want git ok and docker ready", got)
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
	for _, st := range got[:2] {
		if st.State != depMissing {
			t.Errorf("%s = %+v, want still missing with manual instruction", st.Tool, st)
		}
	}
	if got[2].State != depSkipped {
		t.Errorf("worker image = %+v, want skipped when docker unavailable", got[2])
	}
}

func TestDepsDockerDaemonDownStarts(t *testing.T) {
	f := newFakeHost("git", "docker", "sudo", "systemctl")
	f.daemon = false
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[1].State != depReady || !strings.Contains(got[1].Detail, "daemon started") {
		t.Fatalf("docker = %+v, want ready after daemon start", got[1])
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

// ---- docker group membership -------------------------------------------

func installsJoined(f *fakeHost) string { return fmt.Sprint(f.installs) }

func TestDockerGroupProbePassNoCommands(t *testing.T) {
	f := newFakeHost("git", "docker", "sudo", "sg")
	f.daemon = true
	f.groups = []string{"vic", "docker"}
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[1].State != depReady {
		t.Fatalf("docker = %+v, want ready", got[1])
	}
	if len(f.installs) != 0 {
		t.Fatalf("probe-pass ran commands: %v", f.installs)
	}
	if got[2].State != depPresent || !strings.Contains(got[2].Detail, "factory-worker-go") {
		t.Errorf("worker image = %+v, want present", got[2])
	}
}

func TestDockerGroupPermissionDeniedAddsUser(t *testing.T) {
	f := newFakeHost("git", "docker", "sudo", "sg")
	f.daemon = true
	f.permDenied = true // groups = [vic], not docker
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[1].State != depGroupAdded {
		t.Fatalf("docker = %+v, want group membership added", got[1])
	}
	if !strings.Contains(installsJoined(f), "sudo usermod -aG docker vic") {
		t.Errorf("installs = %v, want sudo usermod -aG docker vic", f.installs)
	}
	out := f.outBuf.String()
	if !strings.Contains(out, "$ sudo usermod -aG docker vic") {
		t.Errorf("output missing printed usermod command:\n%s", out)
	}
	if !strings.Contains(out, "$ sg docker -c \"docker version\"") {
		t.Errorf("output missing printed sg verification:\n%s", out)
	}
	for _, want := range []string{"log out and back in", "`newgrp docker`", "`docker ps` will work without sudo", "for vic"} {
		if !strings.Contains(got[1].Detail, want) {
			t.Errorf("detail %q missing %q", got[1].Detail, want)
		}
	}
	if !got[1].outstanding() {
		t.Error("group-added status should be outstanding (session action needed)")
	}
	// Image build must be skipped: this session still cannot use docker.
	if got[2].State != depSkipped || !strings.Contains(got[2].Detail, "newgrp docker") {
		t.Errorf("worker image = %+v, want skipped with newgrp wording", got[2])
	}
}

func TestDockerGroupAlreadyInGroupStaleSession(t *testing.T) {
	f := newFakeHost("git", "docker", "sudo", "sg")
	f.permDenied = true
	f.groups = []string{"vic", "docker"}
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[1].State != depGroupStale {
		t.Fatalf("docker = %+v, want stale-session status", got[1])
	}
	if strings.Contains(installsJoined(f), "usermod") {
		t.Errorf("stale session re-ran usermod pointlessly: %v", f.installs)
	}
	for _, want := range []string{"membership is set for vic", "this session doesn't have it yet", "`newgrp docker`", "log out and back in"} {
		if !strings.Contains(got[1].Detail, want) {
			t.Errorf("detail %q missing %q", got[1].Detail, want)
		}
	}
}

func TestDockerGroupDaemonDownNoUsermod(t *testing.T) {
	f := newFakeHost("git", "docker", "sudo", "systemctl")
	f.daemon = false
	f.startWorks = false
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[1].State != depNoDaemon {
		t.Fatalf("docker = %+v, want daemon not running", got[1])
	}
	if strings.Contains(installsJoined(f), "usermod") || strings.Contains(installsJoined(f), "groupadd") {
		t.Errorf("daemon-down ran membership commands: %v", f.installs)
	}
	if got[2].State != depSkipped || !strings.Contains(got[2].Detail, "daemon not running") {
		t.Errorf("worker image = %+v, want skipped (daemon not running)", got[2])
	}
}

func TestDockerGroupRootSkips(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.permDenied = true
	got := ensureDeps(f.env("linux", 0, debianRelease))
	if len(f.installs) != 0 {
		t.Fatalf("root ran commands: %v", f.installs)
	}
	if !strings.Contains(f.outBuf.String(), "root needs no `docker` group membership") {
		t.Errorf("output missing root skip line:\n%s", f.outBuf.String())
	}
	if got[1].State != depNotUsable {
		t.Errorf("docker = %+v, want still not usable for root+permission-denied", got[1])
	}
}

func TestDockerGroupMissingAfterFreshInstallGroupaddFirst(t *testing.T) {
	f := newFakeHost("git", "sudo", "apt-get", "systemctl", "sg") // docker absent
	f.permDenied = true
	f.groupExists = false
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	joined := installsJoined(f)
	gi, ui := strings.Index(joined, "groupadd docker"), strings.Index(joined, "usermod -aG docker vic")
	if gi < 0 || ui < 0 || gi > ui {
		t.Fatalf("installs = %v, want groupadd before usermod", f.installs)
	}
	if got[1].State != depGroupAdded {
		t.Errorf("docker = %+v, want group membership added", got[1])
	}
}

func TestDockerGroupMissingGroupaddFails(t *testing.T) {
	f := newFakeHost("git", "sudo", "apt-get", "systemctl")
	f.permDenied = true
	f.groupExists = false
	f.failGroupadd = true
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[1].State != depNotUsable {
		t.Fatalf("docker = %+v, want still not usable", got[1])
	}
	for _, want := range []string{"sudo groupadd docker", "sudo usermod -aG docker vic"} {
		if !strings.Contains(got[1].Detail, want) {
			t.Errorf("detail %q missing manual %q", got[1].Detail, want)
		}
	}
}

func TestDockerGroupUsermodFailsWarnsManual(t *testing.T) {
	f := newFakeHost("git", "docker", "sudo")
	f.permDenied = true
	f.failInstall = true // usermod (the only runInstall) fails
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[1].State != depNotUsable || !strings.Contains(got[1].Detail, "sudo usermod -aG docker vic") {
		t.Fatalf("docker = %+v, want still-not-usable with exact manual command", got[1])
	}
}

func TestDockerGroupNoSudoManual(t *testing.T) {
	f := newFakeHost("git", "docker") // non-root, no sudo
	f.permDenied = true
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if len(f.installs) != 0 {
		t.Fatalf("no-sudo ran commands: %v", f.installs)
	}
	if got[1].State != depNotUsable || !strings.Contains(got[1].Detail, "sudo usermod -aG docker vic") {
		t.Errorf("docker = %+v, want manual usermod", got[1])
	}
	if !strings.Contains(f.outBuf.String(), "$ sudo usermod -aG docker vic") {
		t.Errorf("output missing printed manual command:\n%s", f.outBuf.String())
	}
}

func TestDockerGroupDarwinNoGroupStep(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.permDenied = true
	got := ensureDeps(f.env("darwin", 1000, ""))
	if len(f.installs) != 0 || strings.Contains(f.outBuf.String(), "usermod") {
		t.Errorf("darwin touched docker group: installs=%v out=%s", f.installs, f.outBuf.String())
	}
	if got[1].State != depNoDaemon || !strings.Contains(got[1].Detail, "Docker Desktop") {
		t.Errorf("docker = %+v, want Docker Desktop daemon guidance", got[1])
	}
}

// ---- worker image --------------------------------------------------------

func TestWorkerImagePresentNoBuild(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.daemon = true
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[2].State != depPresent {
		t.Fatalf("worker image = %+v, want present", got[2])
	}
	if strings.Contains(installsJoined(f), "docker build") {
		t.Errorf("present image was rebuilt: %v", f.installs)
	}
}

func TestWorkerImageMissingBuilds(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.daemon = true
	f.imagePresent = false
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[2].State != depBuilt || !strings.Contains(got[2].Detail, "factory-worker-go") {
		t.Fatalf("worker image = %+v, want built", got[2])
	}
	joined := installsJoined(f)
	if !strings.Contains(joined, "docker build -t factory-worker-go /fake/worker") {
		t.Errorf("installs = %v, want docker build -t factory-worker-go /fake/worker", f.installs)
	}
	if !strings.Contains(f.outBuf.String(), "$ docker build -t factory-worker-go /fake/worker") {
		t.Errorf("output missing printed build command:\n%s", f.outBuf.String())
	}
}

func TestWorkerImageBuildFailsWarns(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.daemon = true
	f.imagePresent = false
	f.failBuild = true
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[2].State != depBuildFailed || !got[2].outstanding() {
		t.Fatalf("worker image = %+v, want build failed (outstanding)", got[2])
	}
	for _, want := range []string{"daemon exploded building image", "docker build -t factory-worker-go worker/"} {
		if !strings.Contains(got[2].Detail, want) {
			t.Errorf("detail %q missing %q", got[2].Detail, want)
		}
	}
}

func TestWorkerImageDockerAbsentSkipped(t *testing.T) {
	f := newFakeHost("git", "sudo", "apt-get")
	f.failInstall = true // docker install fails -> docker unavailable
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[2].State != depSkipped || !strings.Contains(got[2].Detail, "docker unavailable") {
		t.Errorf("worker image = %+v, want skipped (docker unavailable)", got[2])
	}
	if strings.Contains(installsJoined(f), "docker build") {
		t.Errorf("build attempted without docker: %v", f.installs)
	}
}
