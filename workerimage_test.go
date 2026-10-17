package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedWorkerContextExtraction(t *testing.T) {
	dir, cleanup, err := extractWorkerContext()
	if err != nil {
		t.Fatalf("extractWorkerContext: %v", err)
	}
	defer cleanup()
	dfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatalf("Dockerfile missing from extracted context: %v", err)
	}
	if !strings.Contains(string(dfile), "factory-worker-go") && !strings.Contains(string(dfile), "/opt/stub_agent") {
		t.Errorf("extracted Dockerfile unexpected:\n%s", dfile)
	}
	stub, err := os.ReadFile(filepath.Join(dir, "stub.go"))
	if err != nil {
		t.Fatalf("stub.go missing from extracted context: %v", err)
	}
	if !strings.Contains(string(stub), "package main") || !strings.Contains(string(stub), "factory_addon") {
		t.Errorf("extracted stub.go unexpected (first bytes): %.80q", stub)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanup left temp context dir %s", dir)
	}
}

func TestWorkerImageStaleLogic(t *testing.T) {
	if workerImageVersionStale(workerImageVersion) {
		t.Error("current version should not be stale")
	}
	if workerImageVersion != "2" {
		t.Errorf("workerImageVersion = %q, want %q (label/constant must be bumped together)", workerImageVersion, "2")
	}
	for _, v := range []string{"", "0", "1", "999"} {
		if !workerImageVersionStale(v) {
			t.Errorf("version %q should be stale", v)
		}
	}
	if got := parseWorkerImageLabelsJSON(`{"minifactory.worker-version":"` + workerImageVersion + `"}`); got != workerImageVersion {
		t.Errorf("labels JSON = %q, want %q", got, workerImageVersion)
	}
	if got := parseWorkerImageLabelsJSON(`null`); got != "" {
		t.Errorf("null labels = %q, want empty (pre-label image)", got)
	}
	if got := parseWorkerImageLabelsJSON(`not json`); got != "" {
		t.Errorf("bad labels = %q, want empty", got)
	}
	for out, want := range map[string]string{
		"sha256:abc|123|" + workerImageVersion: workerImageVersion,
		"sha256:abc|123|":                      "",
		"sha256:abc|123|<no value>":            "",
		"sha256:abc|123|0":                     "0",
	} {
		info, ok := parseWorkerImageInspect(out)
		if !ok || info.version != want {
			t.Errorf("parseWorkerImageInspect(%q) = %+v,%v want version %q", out, info, ok, want)
		}
	}
	if _, ok := parseWorkerImageInspect(""); ok {
		t.Error("empty inspect should be not-present")
	}
}

func TestEmbeddedDockerfileNode24AndLabel(t *testing.T) {
	b, err := workerContextFS.ReadFile("worker/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	d := string(b)
	for _, want := range []string{
		"https://deb.nodesource.com/setup_24.x",
		`LABEL ` + workerImageLabel + `="` + workerImageVersion + `"`,
		`LABEL minifactory.worker-version="2"`,
		"@anthropic-ai/claude-code",
		// Exactly the two packages npm flagged as having postinstall
		// scripts, scoped to the one global install (npm's own suggestion).
		"npm install -g --allow-scripts=@anthropic-ai/claude-code,opencode-ai @anthropic-ai/claude-code @openai/codex opencode-ai",
		// Build-time smoke check in the final stage: broken CLIs fail loudly.
		"RUN claude --version && codex --version && opencode --version",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("embedded Dockerfile missing %q", want)
		}
	}
	for _, banned := range []string{
		"dangerously-allow-all-scripts",
		"--allow-scripts=all",
		"@openai/codex,opencode-ai", // codex has no install script; do not allow it
	} {
		if strings.Contains(d, banned) {
			t.Errorf("embedded Dockerfile contains overly broad allow-scripts %q", banned)
		}
	}
	// Smoke check must come after the global install, in the final stage.
	installIdx := strings.Index(d, "npm install -g")
	smokeIdx := strings.Index(d, "RUN claude --version")
	if installIdx < 0 || smokeIdx < 0 || smokeIdx < installIdx {
		t.Errorf("smoke check must follow the npm install (install=%d smoke=%d)", installIdx, smokeIdx)
	}
	if idx := strings.LastIndex(d, "FROM "); idx < 0 || smokeIdx < idx {
		t.Errorf("smoke check must be in the final stage")
	}
	if strings.Contains(d, "setup_20") || strings.Contains(d, "setup_22") {
		t.Error("Dockerfile still references old NodeSource line")
	}
	// Extracted context must be byte-identical to the embedded Dockerfile.
	dir, cleanup, err := extractWorkerContext()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ext, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ext) != d {
		t.Error("extracted Dockerfile differs from embedded")
	}
}

func TestWorkerImageNoLabelRebuilds(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.daemon = true
	f.imageVersion = "" // pre-label image (Node 20 era)
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[2].State != depBuilt || !strings.Contains(got[2].Detail, "rebuilt stale") {
		t.Fatalf("worker image = %+v, want rebuilt stale", got[2])
	}
	if !strings.Contains(fmt.Sprint(f.installs), "docker build") {
		t.Errorf("stale (no label) image was not rebuilt: %v", f.installs)
	}
}

func TestWorkerImageOldLabelRebuilds(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.daemon = true
	f.imageVersion = "0"
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[2].State != depBuilt || !strings.Contains(got[2].Detail, "rebuilt stale") {
		t.Fatalf("worker image = %+v, want rebuilt stale", got[2])
	}
	if !strings.Contains(fmt.Sprint(f.installs), "docker build") {
		t.Errorf("old-label image was not rebuilt: %v", f.installs)
	}
}

// Label "1" is the Node-24-but-blocked-postinstall image already on
// Vic's VPS: it must be treated stale and rebuilt automatically.
func TestWorkerImageLabel1Rebuilds(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.daemon = true
	f.imageVersion = "1"
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[2].State != depBuilt || !strings.Contains(got[2].Detail, "rebuilt stale") {
		t.Fatalf("worker image = %+v, want rebuilt stale for label 1", got[2])
	}
	if !strings.Contains(fmt.Sprint(f.installs), "docker build") {
		t.Errorf("label-1 image was not rebuilt: %v", f.installs)
	}
}

func TestBuilderImageRefDerivedFromDockerfile(t *testing.T) {
	if got := workerBuilderImageRef(); got != "golang:1.26-bookworm" {
		t.Errorf("workerBuilderImageRef() = %q, want golang:1.26-bookworm (from embedded Dockerfile)", got)
	}
	for in, want := range map[string]string{
		"FROM golang:1.26-bookworm AS build\nFROM python:3.12-slim\n": "golang:1.26-bookworm",
		"FROM --platform=linux/amd64 golang:1.26 AS build\n":          "golang:1.26",
		"from golang:1.26 as build\n":                                 "golang:1.26",
		"FROM python:3.12-slim\n":                                     "",
		"# FROM golang:1.26 AS build\nFROM python:3.12-slim\n":        "",
	} {
		if got := parseBuilderImageRef(in); got != want {
			t.Errorf("parseBuilderImageRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// (1) Successful build triggers builder-image removal.
func TestWorkerImageBuildRemovesBuilderImage(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.daemon = true
	f.imagePresent = false
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[2].State != depBuilt {
		t.Fatalf("worker image = %+v, want built", got[2])
	}
	ref := workerBuilderImageRef()
	joined := installsJoined(f)
	if !strings.Contains(joined, "docker image rm "+ref) {
		t.Errorf("installs = %v, want docker image rm %s after build", f.installs, ref)
	}
	if f.builderPresent {
		t.Error("builder image still present after cleanup")
	}
	out := f.outBuf.String()
	if !strings.Contains(out, "$ docker image rm "+ref) || !strings.Contains(out, "removed build-only builder image "+ref) {
		t.Errorf("output missing printed builder removal:\n%s", out)
	}
	if strings.Contains(joined, "system prune") || strings.Contains(joined, "image prune") {
		t.Errorf("cleanup used a broad prune: %v", f.installs)
	}
}

// (2) Failed build does NOT claim cleanup / does not remove anything.
func TestWorkerImageFailedBuildDoesNotRemoveBuilderImage(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.daemon = true
	f.imagePresent = false
	f.builderPresent = true // lingering from an earlier build
	f.failBuild = true
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[2].State != depBuildFailed {
		t.Fatalf("worker image = %+v, want build failed", got[2])
	}
	if strings.Contains(installsJoined(f), "docker image rm") {
		t.Errorf("failed build removed an image: %v", f.installs)
	}
	if !f.builderPresent {
		t.Error("failed build should leave the builder image untouched")
	}
	if strings.Contains(f.outBuf.String(), "removed build-only builder image") {
		t.Errorf("failed build claimed cleanup:\n%s", f.outBuf.String())
	}
}

// (3) Cleanup failure warns but init (ensureDeps) still succeeds.
func TestWorkerImageBuilderCleanupFailureWarnsButSucceeds(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.daemon = true
	f.imagePresent = false
	f.failBuilderRm = true
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[2].State != depBuilt || got[2].outstanding() {
		t.Fatalf("worker image = %+v, want built (cleanup failure must not fail init)", got[2])
	}
	if !strings.Contains(installsJoined(f), "docker image rm "+workerBuilderImageRef()) {
		t.Errorf("installs = %v, want attempted builder removal", f.installs)
	}
	out := f.outBuf.String()
	if !strings.Contains(out, "warning: could not remove builder image "+workerBuilderImageRef()) {
		t.Errorf("output missing cleanup-failure warning:\n%s", out)
	}
}

// (4) Current worker image + lingering builder image on the no-build
// path still gets cleaned, without rebuilding the worker image.
func TestWorkerImageCurrentWithLingeringBuilderCleansUp(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.daemon = true
	f.imagePresent = true
	f.imageVersion = workerImageVersion
	f.builderPresent = true
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[2].State != depPresent {
		t.Fatalf("worker image = %+v, want present (no rebuild)", got[2])
	}
	joined := installsJoined(f)
	if strings.Contains(joined, "docker build") {
		t.Errorf("current image was rebuilt: %v", f.installs)
	}
	if !strings.Contains(joined, "docker image rm "+workerBuilderImageRef()) {
		t.Errorf("installs = %v, want lingering builder removal on no-build path", f.installs)
	}
	if f.builderPresent {
		t.Error("lingering builder image still present after no-build cleanup")
	}
}

// No-build cleanup failure also warns but keeps the present status.
func TestWorkerImageCurrentBuilderCleanupFailureStillPresent(t *testing.T) {
	f := newFakeHost("git", "docker")
	f.daemon = true
	f.builderPresent = true
	f.failBuilderRm = true
	got := ensureDeps(f.env("linux", 1000, debianRelease))
	if got[2].State != depPresent || got[2].outstanding() {
		t.Fatalf("worker image = %+v, want present despite cleanup failure", got[2])
	}
	if !strings.Contains(f.outBuf.String(), "warning: could not remove builder image") {
		t.Errorf("output missing cleanup-failure warning:\n%s", f.outBuf.String())
	}
}
