package main

// workerimage — the factory-worker-go image for `init`.
//
// install.sh builds the image from a source checkout:
//   docker build -q -t factory-worker-go "$APP_DIR/worker/"
// A released binary is installed alone in ~/.local/bin with no checkout
// beside it, so init cannot rely on finding worker/ on disk. The worker
// context is tiny (Dockerfile 810B + stub.go 2.5KB, checked in the repo),
// so it is embedded in the binary and extracted to a temp dir at build
// time. Init builds only when the image is missing; an existing image is
// never rebuilt or pulled.

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed worker/Dockerfile worker/stub.go
var workerContextFS embed.FS

// extractWorkerContext writes the embedded worker/ context to a fresh temp
// dir and returns it plus a cleanup func. This is the default depEnv
// workerContext seam.
func extractWorkerContext() (string, func(), error) {
	dir, err := os.MkdirTemp("", "minifactory-worker-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	for _, name := range []string{"Dockerfile", "stub.go"} {
		b, err := workerContextFS.ReadFile("worker/" + name)
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0644); err != nil {
			cleanup()
			return "", func() {}, err
		}
	}
	return dir, cleanup, nil
}

// probeWorkerImage reports the image's id/size when present as the user.
func probeWorkerImage(env depEnv) (string, bool) {
	rc, out := env.run(15*time.Second, "docker", "image", "inspect", workerImage,
		"--format", "{{.Id}} {{.Size}}")
	if rc != 0 {
		return "", false
	}
	parts := strings.Fields(strings.TrimSpace(out))
	if len(parts) == 0 {
		return workerImage, true
	}
	detail := workerImage + " (" + parts[0]
	if len(parts) >= 2 {
		if n, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			detail += ", " + humanBytes(n)
		}
	}
	return detail + ")", true
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return firstLine(l, 200)
		}
	}
	return ""
}

// ensureWorkerImage builds factory-worker-go when it is missing. It runs
// only when docker is usable as the invoking user right now; every other
// docker outcome (missing, daemon down, group/stale-session) skips with a
// one-line reason. Warn-and-continue: a failed build never fails init.
func (c *depChecker) ensureWorkerImage() depStatus {
	const tool = "worker image"
	if _, err := c.env.lookPath("docker"); err != nil {
		c.printf("  worker image: skipped (docker unavailable)")
		return depStatus{tool, depSkipped, "skipped (docker unavailable)"}
	}
	kind, _ := probeDockerDaemonDetail(c.env)
	switch kind {
	case dockerProbePermissionDenied:
		if c.env.goos != "linux" {
			// No docker-group concept off Linux (Docker Desktop).
			c.printf("  worker image: skipped (docker daemon not usable as this user)")
			return depStatus{tool, depSkipped, "skipped (docker daemon not usable as this user)"}
		}
		username := c.env.usernameOr("")
		c.printf("  worker image: skipped — building needs a working docker session for %s; log out and back in, or run `newgrp docker`, then re-run `minifactory init`", username)
		return depStatus{tool, depSkipped, "skipped — daemon not usable without sudo in this session; log out and back in, or run `newgrp docker`, then re-run `minifactory init` (" + dockerGroupRemedy(username) + ")"}
	case dockerProbeUnreachable:
		c.printf("  worker image: skipped (docker daemon not running)")
		return depStatus{tool, depSkipped, "skipped (docker daemon not running)"}
	}
	if detail, present := probeWorkerImage(c.env); present {
		return depStatus{tool, depPresent, detail}
	}
	// Missing: extract the embedded context and build, printing the exact
	// command first and streaming output like the other init commands.
	ctxDir, cleanup, err := c.env.workerContext()
	if err != nil {
		c.printf("  warning: cannot prepare the embedded worker build context: %v", err)
		return depStatus{tool, depBuildFailed, fmt.Sprintf("cannot prepare worker build context: %v; from a repo checkout run `docker build -t %s worker/`", err, workerImage)}
	}
	defer cleanup()
	cmd := []string{"docker", "build", "-t", workerImage, ctxDir}
	c.printf("  worker image: %s missing — building from the embedded worker context", workerImage)
	c.printf("  $ %s", strings.Join(cmd, " "))
	rc, out := c.env.runInstall(cmd[0], cmd[1:]...)
	if rc != 0 {
		last := lastNonEmptyLine(out)
		if last == "" {
			last = fmt.Sprintf("exit %d", rc)
		}
		rerun := "docker build -t " + workerImage + " worker/"
		c.printf("  warning: worker image build failed (exit %d): %s", rc, last)
		c.printf("  re-run `minifactory init`, or from a repo checkout: %s", rerun)
		return depStatus{tool, depBuildFailed, fmt.Sprintf("%s; re-run `minifactory init` or `%s` from a repo checkout", last, rerun)}
	}
	if detail, present := probeWorkerImage(c.env); present {
		return depStatus{tool, depBuilt, detail}
	}
	c.printf("  warning: worker image build reported success but `docker image inspect %s` still fails", workerImage)
	return depStatus{tool, depBuildFailed, "build ran but image still not present; re-run `minifactory init` or `docker build -t " + workerImage + " worker/` from a repo checkout"}
}
