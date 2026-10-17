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
	for _, v := range []string{"", "0", "999"} {
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
		"@anthropic-ai/claude-code",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("embedded Dockerfile missing %q", want)
		}
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
