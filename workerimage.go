package main

// workerimage — the factory-worker-go image for `init`.
//
// install.sh builds the image from a source checkout:
//   docker build -q -t factory-worker-go "$APP_DIR/worker/"
// A released binary is installed alone in ~/.local/bin with no checkout
// beside it, so init cannot rely on finding worker/ on disk. The worker
// context is tiny (Dockerfile + stub.go, checked in the repo), so it is
// embedded in the binary and extracted to a temp dir at build time. Init
// builds when the image is missing or stale (its
// minifactory.worker-version label does not match workerImageVersion
// below; pre-label images have no label and so are always stale). A
// current image is never rebuilt or pulled.
//
// A normal multi-stage `docker build` pulls and retains the tagged
// builder-stage image (golang:...) locally. After a successful build —
// and also on the already-current path, to clean up images left by
// earlier builds — init removes that exact builder image on a
// best-effort basis. The ref is derived from the Dockerfile's
// builder-stage FROM line, never hardcoded a second time.

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed worker/Dockerfile worker/stub.go
var workerContextFS embed.FS

// workerImageLabel stamps every worker image built from worker/Dockerfile.
// workerImageVersion is the version this binary expects: bump it (and the
// LABEL in worker/Dockerfile, in the same change) whenever the Dockerfile
// changes, so existing installs rebuild a stale image on `init` instead of
// silently keeping e.g. an image with too-old Node. Old images built
// before labelling have no label (""), which never matches.
const workerImageLabel = "minifactory.worker-version"
const workerImageVersion = "3"

// workerImageVersionStale reports whether an image carrying version v
// should be rebuilt by this binary.
func workerImageVersionStale(v string) bool { return v != workerImageVersion }

// parseWorkerImageLabelsJSON extracts workerImageVersion from the
// `{{json .Config.Labels}}` output of `docker image inspect`.
func parseWorkerImageLabelsJSON(out string) string {
	var labels map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &labels); err != nil {
		return ""
	}
	return labels[workerImageLabel]
}

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

type workerImageInfo struct {
	detail  string
	version string
}

// parseWorkerImageInspect parses the pipe-separated
// `{{.Id}}|{{.Size}}|<worker-version>` inspect output. A missing label
// renders as "" (older Docker) or "<no value>", both meaning stale.
func parseWorkerImageInspect(out string) (workerImageInfo, bool) {
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return workerImageInfo{}, false
	}
	info := workerImageInfo{detail: workerImage + " (" + strings.TrimSpace(parts[0])}
	if len(parts) >= 2 {
		if n, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err == nil {
			info.detail += ", " + humanBytes(n)
		}
	}
	if len(parts) >= 3 {
		if v := strings.TrimSpace(parts[2]); v != "<no value>" {
			info.version = v
		}
	}
	info.detail += ")"
	return info, true
}

// probeWorkerImageInfo reports the image's id/size/version when present.
func probeWorkerImageInfo(env depEnv) (workerImageInfo, bool) {
	rc, out := env.run(15*time.Second, "docker", "image", "inspect", workerImage,
		"--format", "{{.Id}}|{{.Size}}|{{index .Config.Labels \""+workerImageLabel+"\"}}")
	if rc != 0 {
		return workerImageInfo{}, false
	}
	return parseWorkerImageInspect(out)
}

// parseBuilderImageRef returns the image ref of the first named
// (builder) stage in a Dockerfile: the FROM line carrying an `AS <name>`
// clause. Later stages (the shipped python image) have no need for a
// local tag from our build and are never returned: only a FROM with an
// AS clause qualifies, and the first such stage is the builder. Returns
// "" when no named stage is found.
func parseBuilderImageRef(dockerfile string) string {
	for _, line := range strings.Split(dockerfile, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		i := 1
		for i < len(fields) && strings.HasPrefix(fields[i], "--") {
			i++ // skip FROM flags such as --platform=...
		}
		if i >= len(fields) {
			continue
		}
		for j := i + 1; j < len(fields); j++ {
			if strings.EqualFold(fields[j], "AS") {
				return fields[i]
			}
		}
	}
	return ""
}

// workerBuilderImageRef derives the build-only builder image ref from
// the embedded worker/Dockerfile builder stage, so it cannot drift from
// the Dockerfile the way a second hardcoded copy could.
func workerBuilderImageRef() string {
	b, err := workerContextFS.ReadFile("worker/Dockerfile")
	if err != nil {
		return ""
	}
	return parseBuilderImageRef(string(b))
}

// cleanupWorkerBuilderImage removes the build-only builder image (the
// golang image pulled for the Dockerfile's builder stage) from the local
// Docker image store, so `docker images` does not keep listing it after
// init. It only ever removes that exact ref — no prunes. Best-effort:
// when the image is absent nothing happens, and a failed removal (image
// in use, already gone, ...) warns and continues; it never fails init.
func (c *depChecker) cleanupWorkerBuilderImage() {
	ref := workerBuilderImageRef()
	if ref == "" || ref == workerImage {
		return
	}
	if rc, _ := c.env.run(15*time.Second, "docker", "image", "inspect", ref); rc != 0 {
		return // not present locally — nothing to clean up
	}
	c.printf("  worker image: removing build-only builder image %s (builder stage of worker/Dockerfile; not needed by %s)", ref, workerImage)
	cmd := []string{"docker", "image", "rm", ref}
	c.printf("  $ %s", strings.Join(cmd, " "))
	rc, out := c.env.runInstall(cmd[0], cmd[1:]...)
	if rc != 0 {
		last := lastNonEmptyLine(out)
		if last == "" {
			last = fmt.Sprintf("exit %d", rc)
		}
		c.printf("  warning: could not remove builder image %s (%s) — continuing; remove it with `docker image rm %s`", ref, last, ref)
		return
	}
	c.printf("  worker image: removed build-only builder image %s", ref)
}

// probeWorkerImage reports the image's id/size when present as the user.
func probeWorkerImage(env depEnv) (string, bool) {
	info, ok := probeWorkerImageInfo(env)
	if !ok {
		return "", false
	}
	return info.detail, true
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

// ensureWorkerImage builds factory-worker-go when it is missing or stale. It runs
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
	stale, staleVersion := false, ""
	if info, present := probeWorkerImageInfo(c.env); present {
		if !workerImageVersionStale(info.version) {
			// Already current: no rebuild, but still clean up a
			// builder image left behind by an earlier build.
			c.cleanupWorkerBuilderImage()
			return depStatus{tool, depOK, info.detail}
		}
		stale, staleVersion = true, info.version
	}
	// Missing or stale: extract the embedded context and build, printing
	// the exact command first and streaming output like other init commands.
	ctxDir, cleanup, err := c.env.workerContext()
	if err != nil {
		c.printf("  warning: cannot prepare the embedded worker build context: %v", err)
		return depStatus{tool, depBuildFailed, fmt.Sprintf("cannot prepare worker build context: %v; from a repo checkout run `docker build -t %s worker/`", err, workerImage)}
	}
	defer cleanup()
	cmd := []string{"docker", "build", "-t", workerImage, ctxDir}
	if stale {
		c.printf("  worker image: %s present but stale (worker-version %q, want %q) — rebuilding from the embedded worker context", workerImage, staleVersion, workerImageVersion)
	} else {
		c.printf("  worker image: %s missing — building from the embedded worker context", workerImage)
	}
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
	if info, present := probeWorkerImageInfo(c.env); present {
		if workerImageVersionStale(info.version) {
			c.printf("  warning: worker image build reported success but its worker-version is %q, want %q", info.version, workerImageVersion)
			return depStatus{tool, depBuildFailed, fmt.Sprintf("build ran but worker image version is %q, want %q; re-run `minifactory init` or `docker build -t "+workerImage+" worker/` from a repo checkout", info.version, workerImageVersion)}
		}
		c.cleanupWorkerBuilderImage()
		if stale {
			return depStatus{tool, depInstalled, "rebuilt stale worker image: " + info.detail}
		}
		return depStatus{tool, depInstalled, info.detail}
	}
	c.printf("  warning: worker image build reported success but `docker image inspect %s` still fails", workerImage)
	return depStatus{tool, depBuildFailed, "build ran but image still not present; re-run `minifactory init` or `docker build -t " + workerImage + " worker/` from a repo checkout"}
}

// cmdRebuildWorker rebuilds the worker image when it is missing or stale,
// without touching the queue or .env — the surgical counterpart to
// `minifactory init`, which also rebuilds the image but resets factory
// state. Exits 1 when the image could not be brought current.
func cmdRebuildWorker(args []string) int {
	c := &depChecker{env: defaultDepEnv()}
	if c.env.goos == "linux" {
		c.plan = resolveLinuxPlan(c.env)
	}
	st := c.ensureWorkerImage()
	fmt.Printf("worker image: %s — %s\n", st.State, st.Detail)
	if st.outstanding() {
		return 1
	}
	return 0
}
