package main

// deps — git/docker presence and (on Linux) installation for `init`.
//
// install.sh never installed these tools (it preflight-fails when they are
// missing), so there is no curl|sh precedent to align with: on Linux init
// uses the distro package manager (apt docker.io / dnf docker), prints every
// command before running it, and uses sudo only for the package/service
// commands when not already root. On macOS (and unsupported platforms) it
// only instructs — it never claims to have installed anything. Failures are
// warnings, matching init's best-effort label setup: init still writes .env
// and reports the tools as outstanding in its final summary.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type depState string

const (
	depOK        depState = "ok"
	depInstalled depState = "installed"
	depMissing   depState = "still missing"
	depNoDaemon  depState = "daemon not running"
)

type depStatus struct {
	Tool   string
	State  depState
	Detail string
}

// outstanding reports whether the tool still needs user action.
func (s depStatus) outstanding() bool {
	return s.State == depMissing || s.State == depNoDaemon
}

// depEnv is the seam between dependency logic and the host, so tests can
// fake tool presence, installs and platforms without touching the real box.
type depEnv struct {
	goos     string
	lookPath func(string) (string, error)
	// run is for short captured probes (git/docker --version, docker info).
	run func(timeout time.Duration, name string, args ...string) (int, string)
	// runInstall is for long, user-visible installs/service starts: output
	// streams to out and stdin stays attached so sudo can prompt.
	runInstall func(name string, args ...string) (int, string)
	readFile   func(string) ([]byte, error)
	euid       int
	out        io.Writer
}

func defaultDepEnv() depEnv {
	out := io.Writer(os.Stdout)
	return depEnv{
		goos:     runtime.GOOS,
		lookPath: exec.LookPath,
		run: func(timeout time.Duration, name string, args ...string) (int, string) {
			return runCmd(timeout, "", nil, name, args...)
		},
		runInstall: func(name string, args ...string) (int, string) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdin = os.Stdin
			var buf bytes.Buffer
			cmd.Stdout = io.MultiWriter(out, &buf)
			cmd.Stderr = io.MultiWriter(out, &buf)
			err := cmd.Run()
			if ctx.Err() == context.DeadlineExceeded {
				return -1, buf.String() + "\n[TIMEOUT]"
			}
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					return ee.ExitCode(), buf.String()
				}
				return -1, buf.String() + "\n[ERROR] " + err.Error()
			}
			return 0, buf.String()
		},
		readFile: os.ReadFile,
		euid:     os.Geteuid(),
		out:      out,
	}
}

// ensureInitDeps is the init-facing entry point; a package var so tests can
// stub it and never trigger a real install.
var ensureInitDeps = func(skip bool) []depStatus {
	if skip {
		return nil
	}
	return ensureDeps(defaultDepEnv())
}

// linuxPlan is the resolved Linux install method.
type linuxPlan struct {
	family  string   // "debian" | "rhel" | "" (unknown)
	pm      string   // apt-get | dnf | yum | "" (unavailable)
	prefix  []string // ["sudo"] when non-root and sudo exists, else nil
	sudoOK  bool     // root, or sudo available
	aptSeen bool     // apt-get update already attempted
}

func parseOSReleaseFamily(content string) string {
	kv := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			kv[k] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	ids := strings.Fields(strings.ToLower(kv["ID"] + " " + kv["ID_LIKE"]))
	for _, id := range ids {
		switch id {
		case "debian", "ubuntu":
			return "debian"
		case "rhel", "fedora", "centos", "rocky", "almalinux":
			return "rhel"
		}
	}
	return ""
}

func resolveLinuxPlan(env depEnv) linuxPlan {
	p := linuxPlan{}
	if env.euid == 0 {
		p.sudoOK = true
	} else if _, err := env.lookPath("sudo"); err == nil {
		p.prefix = []string{"sudo"}
		p.sudoOK = true
	}
	if b, err := env.readFile("/etc/os-release"); err == nil {
		p.family = parseOSReleaseFamily(string(b))
	}
	switch p.family {
	case "debian":
		if _, err := env.lookPath("apt-get"); err == nil {
			p.pm = "apt-get"
		}
	case "rhel":
		for _, pm := range []string{"dnf", "yum"} {
			if _, err := env.lookPath(pm); err == nil {
				p.pm = pm
				break
			}
		}
	}
	return p
}

func (p linuxPlan) full(cmd []string) []string {
	return append(append([]string{}, p.prefix...), cmd...)
}

func (p linuxPlan) cmdString(cmd []string) string {
	return strings.Join(p.full(cmd), " ")
}

// installCmds returns the (unprefixed) package-install commands for a tool,
// excluding `apt-get update`, which ensureAptUpdate runs once, shared.
func (p linuxPlan) installCmds(tool string) [][]string {
	pkg := map[string]map[string]string{
		"debian": {"git": "git", "docker": "docker.io"},
		"rhel":   {"git": "git", "docker": "docker"},
	}
	if p.pm == "" {
		return nil
	}
	name, ok := pkg[p.family][tool]
	if !ok {
		return nil
	}
	return [][]string{{p.pm, "install", "-y", name}}
}

type depChecker struct {
	env  depEnv
	plan linuxPlan // linux only
}

func (c *depChecker) printf(format string, a ...any) {
	fmt.Fprintf(c.env.out, format+"\n", a...)
}

// runPrinted prints the exact command, then runs it streaming.
func (c *depChecker) runPrinted(cmd []string) int {
	c.printf("  $ %s", strings.Join(cmd, " "))
	rc, _ := c.env.runInstall(cmd[0], cmd[1:]...)
	return rc
}

func (c *depChecker) ensureAptUpdate() {
	if c.plan.family != "debian" || c.plan.aptSeen {
		return
	}
	c.plan.aptSeen = true
	if rc := c.runPrinted(c.plan.full([]string{"apt-get", "update"})); rc != 0 {
		c.printf("  warning: apt-get update exited %d; trying the install with existing package lists", rc)
	}
}

// manualInstall reports (without running anything) the exact commands the
// user should run, and returns a still-missing status carrying them.
func (c *depChecker) manualInstall(tool string, cmds [][]string, why string) depStatus {
	var strs []string
	for _, cmd := range cmds {
		strs = append(strs, c.plan.cmdString(cmd))
	}
	detail := why
	if len(strs) > 0 {
		detail += "; run: " + strings.Join(strs, " && ")
		for _, s := range strs {
			c.printf("  $ %s", s)
		}
	}
	c.printf("  %s: not installed — %s", tool, detail)
	return depStatus{tool, depMissing, detail}
}

// installLinux installs tool via the distro package manager. ok=false means
// the caller should fall back to a manual-install status.
func (c *depChecker) installLinux(tool string) (depStatus, bool) {
	cmds := c.plan.installCmds(tool)
	if c.env.goos != "linux" || c.plan.family == "" || len(cmds) == 0 {
		return c.manualInstall(tool, cmds,
			"unsupported or undetected Linux distro — install "+tool+" with your package manager"), true
	}
	if !c.plan.sudoOK {
		all := cmds
		if c.plan.family == "debian" {
			all = append([][]string{{"apt-get", "update"}}, cmds...)
		}
		return c.manualInstall(tool, all, "sudo is unavailable"), true
	}
	c.printf("  %s: not found — installing via %s (%s family)", tool, c.plan.pm, c.plan.family)
	c.ensureAptUpdate()
	for _, cmd := range cmds {
		if rc := c.runPrinted(c.plan.full(cmd)); rc != 0 {
			return depStatus{tool, depMissing,
				fmt.Sprintf("install command failed (exit %d): %s", rc, c.plan.cmdString(cmd))}, true
		}
	}
	return depStatus{}, false
}

func probeGit(env depEnv) (string, bool) {
	if _, err := env.lookPath("git"); err != nil {
		return "", false
	}
	rc, out := env.run(10*time.Second, "git", "--version")
	if rc != 0 {
		return "", false
	}
	return strings.TrimSpace(firstLine(out, 80)), true
}

func probeDockerCLI(env depEnv) (string, bool) {
	if _, err := env.lookPath("docker"); err != nil {
		return "", false
	}
	rc, out := env.run(10*time.Second, "docker", "--version")
	if rc != 0 {
		return "", true // CLI present, version probe failed
	}
	return strings.TrimSpace(firstLine(out, 80)), true
}

func probeDockerDaemon(env depEnv) bool {
	rc, _ := env.run(15*time.Second, "docker", "info")
	return rc == 0
}

// tryStartDocker attempts systemctl, then the service fallback, on Linux.
func (c *depChecker) tryStartDocker() bool {
	if c.env.goos != "linux" {
		return false
	}
	var candidates [][]string
	if _, err := c.env.lookPath("systemctl"); err == nil {
		candidates = append(candidates, []string{"systemctl", "enable", "--now", "docker"})
	}
	if _, err := c.env.lookPath("service"); err == nil {
		candidates = append(candidates, []string{"service", "docker", "start"})
	}
	if len(candidates) == 0 {
		c.printf("  docker: daemon not running and no systemctl/service found to start it")
		return false
	}
	if !c.plan.sudoOK {
		for _, cmd := range candidates {
			c.printf("  $ %s", c.plan.cmdString(cmd))
		}
		c.printf("  docker: sudo unavailable — run the command above to start the daemon")
		return false
	}
	for _, cmd := range candidates {
		c.runPrinted(c.plan.full(cmd))
		if probeDockerDaemon(c.env) {
			return true
		}
	}
	return probeDockerDaemon(c.env)
}

func (c *depChecker) ensureGit() depStatus {
	if ver, ok := probeGit(c.env); ok {
		return depStatus{"git", depOK, ver}
	}
	switch c.env.goos {
	case "darwin":
		c.printf("  git: not found — install the Xcode Command Line Tools: xcode-select --install")
		return depStatus{"git", depMissing, "install the Xcode Command Line Tools: xcode-select --install"}
	case "linux":
		if st, handled := c.installLinux("git"); handled {
			if st.State == depMissing {
				return st
			}
		}
		if ver, ok := probeGit(c.env); ok {
			return depStatus{"git", depInstalled, ver}
		}
		return depStatus{"git", depMissing, "still not found on PATH after install"}
	default:
		return depStatus{"git", depMissing, "install git manually for " + c.env.goos}
	}
}

func (c *depChecker) ensureDocker() depStatus {
	ver, present := probeDockerCLI(c.env)
	if !present {
		switch c.env.goos {
		case "darwin":
			c.printf("  docker: not found — install Docker Desktop: https://www.docker.com/products/docker-desktop")
			return depStatus{"docker", depMissing, "install Docker Desktop from https://www.docker.com/products/docker-desktop and start it"}
		case "linux":
			if st, handled := c.installLinux("docker"); handled && st.State == depMissing {
				return st
			}
			ver, present = probeDockerCLI(c.env)
			if !present {
				return depStatus{"docker", depMissing, "still not found on PATH after install"}
			}
			if c.tryStartDocker() || probeDockerDaemon(c.env) {
				return depStatus{"docker", depInstalled, ver + "; daemon reachable"}
			}
			return depStatus{"docker", depNoDaemon, ver + "; installed but daemon not running — start it (e.g. sudo systemctl enable --now docker), then run `minifactory doctor`"}
		default:
			return depStatus{"docker", depMissing, "install Docker manually for " + c.env.goos}
		}
	}
	if probeDockerDaemon(c.env) {
		state := depOK
		if ver == "" {
			ver = "docker CLI present"
		}
		return depStatus{"docker", state, ver + "; daemon reachable"}
	}
	// CLI present, daemon down.
	if c.env.goos == "linux" && c.tryStartDocker() {
		return depStatus{"docker", depOK, ver + "; daemon started and reachable"}
	}
	if c.env.goos == "darwin" {
		return depStatus{"docker", depNoDaemon, ver + "; daemon not running — start Docker Desktop, then run `minifactory doctor`"}
	}
	return depStatus{"docker", depNoDaemon, ver + "; daemon not running — start it (e.g. sudo systemctl enable --now docker), then run `minifactory doctor`"}
}

// ensureDeps checks (and on Linux installs) git and Docker, printing every
// command it runs. It never fails hard: outstanding tools come back as
// still-missing / daemon-not-running statuses.
func ensureDeps(env depEnv) []depStatus {
	if env.out == nil {
		env.out = io.Discard
	}
	c := &depChecker{env: env}
	if env.goos == "linux" {
		c.plan = resolveLinuxPlan(env)
	}
	return []depStatus{c.ensureGit(), c.ensureDocker()}
}
