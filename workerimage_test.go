package main

import (
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
