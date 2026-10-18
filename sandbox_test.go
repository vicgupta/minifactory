package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkwork builds a temp workdir populated with the given relative files
// (contents are irrelevant) and returns its path.
func mkwork(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestHasTestSuite(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  bool
	}{
		{"readme only repo has no suite", []string{"README.md"}, false},
		{"test_ prefix file", []string{"test_foo.py"}, true},
		{"_test suffix file", []string{"foo_test.py"}, true},
		{"nested test file", []string{"src/bar/test_baz.py"}, true},
		{"test file inside .git is ignored", []string{".git/test_foo.py"}, false},
		{"test file inside __pycache__ is ignored", []string{"__pycache__/test_foo.py"}, false},
		{"pytest.ini implies suite", []string{"pytest.ini"}, true},
		{"pyproject with pytest config", []string{"pyproject.toml"}, true},
		{"tests directory implies suite", []string{"tests/"}, true},
		{"package.json with test script", []string{"package.json"}, true},
		{"factory.json test command is authoritative", []string{"factory.json"}, true},
		{"go.mod implies suite", []string{"go.mod"}, true},
		{"go test file", []string{"foo_test.go"}, true},
		{"go repo with no tests still has go.mod", []string{"go.mod", "main.go"}, true},
	}
	// Write meaningful contents for the config-file cases.
	write := map[string]string{
		"pyproject.toml": "[tool.pytest.ini_options]\n",
		"package.json":   `{"scripts": {"test": "jest"}}`,
		"factory.json":   `{"test": "make check"}`,
		"tests/":         "", // directory marker, no file content needed
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				p := filepath.Join(dir, f)
				if f[len(f)-1] == '/' {
					if err := os.MkdirAll(p, 0o755); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				content := "# x\n"
				if c, ok := write[f]; ok {
					content = c
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := hasTestSuite(dir); got != tc.want {
				t.Errorf("hasTestSuite() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDetectTestCmd(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  string
	}{
		{"go.mod means go test", []string{"go.mod", "main.go"}, "go test ./..."},
		{"package.json means npm test", []string{"package.json"}, "npm test --silent"},
		{"no markers defaults to pytest", []string{"README.md"}, "python3 -m pytest -q"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("# x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := detectTestCmd(dir); got != tc.want {
				t.Errorf("detectTestCmd() = %q, want %q", got, tc.want)
			}
		})
	}
	// factory.json is authoritative over layout.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "factory.json"), []byte(`{"test": "make check"}`), 0o644)
	if got := detectTestCmd(dir); got != "make check" {
		t.Errorf("detectTestCmd() with factory.json = %q, want %q", got, "make check")
	}
}

func TestHasTestSuiteNegativeConfigs(t *testing.T) {
	t.Run("pyproject without pytest config is not a suite", func(t *testing.T) {
		dir := mkwork(t)
		if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"),
			[]byte("[build-system]\nrequires = [\"setuptools\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if hasTestSuite(dir) {
			t.Error("hasTestSuite() = true, want false")
		}
	})
	t.Run("package.json without test script is not a suite", func(t *testing.T) {
		dir := mkwork(t)
		if err := os.WriteFile(filepath.Join(dir, "package.json"),
			[]byte(`{"scripts": {"build": "tsc"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if hasTestSuite(dir) {
			t.Error("hasTestSuite() = true, want false")
		}
	})
	t.Run("setup.cfg with pytest section is a suite", func(t *testing.T) {
		dir := mkwork(t)
		if err := os.WriteFile(filepath.Join(dir, "setup.cfg"),
			[]byte("[tool:pytest]\ntestpaths = tests\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !hasTestSuite(dir) {
			t.Error("hasTestSuite() = false, want true")
		}
	})
}

func TestAgentTokenEnv(t *testing.T) {
	cases := map[string]string{
		"claude":   "CLAUDE_CODE_OAUTH_TOKEN",
		"codex":    "OPENAI_API_KEY",
		"opencode": "OPENCODE_API_KEY",
		"stub":     "",
		"bogus":    "",
	}
	for name, want := range cases {
		if got := agentTokenEnv(name); got != want {
			t.Errorf("agentTokenEnv(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestWriteAgentStaging(t *testing.T) {
	dir, err := writeAgentStaging("t1", "OPENCODE_API_KEY=sekret-token", "do the thing\nmultiline")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("staging dir perm = %v, err = %v; want 0700", fi.Mode(), err)
	}
	envB, err := os.ReadFile(filepath.Join(dir, "token.env"))
	if err != nil || string(envB) != "OPENCODE_API_KEY=sekret-token\n" {
		t.Errorf("token.env = %q, err = %v", envB, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "token.env")); fi.Mode().Perm() != 0o600 {
		t.Errorf("token.env perm = %o, want 0600", fi.Mode().Perm())
	}
	promptB, err := os.ReadFile(filepath.Join(dir, "prompt.txt"))
	if err != nil || string(promptB) != "do the thing\nmultiline" {
		t.Errorf("prompt.txt = %q, err = %v", promptB, err)
	}
	// The staging dir lives outside any repo checkout.
	if dir == "" {
		t.Error("empty staging dir")
	}
}

func TestSandboxDockerBase(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy:8080")
	workdir := mkwork(t, "a.txt")
	staging, err := writeAgentStaging("t1", "OPENCODE_API_KEY=sekret-token", "do the thing")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(staging)
	args := sandboxDockerBase(workdir, staging)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run", "--rm",
		"--cpus", "2", "--memory", "4g", "--pids-limit", "256",
		"--env-file", filepath.Join(staging, "token.env"),
		workdir + ":/work",
		filepath.Join(staging, "prompt.txt") + ":/prompt:ro",
		"-w", "/work", workerImage,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker args missing %q in %q", want, joined)
		}
	}
	// Proxy passes through when set on the host.
	if !strings.Contains(joined, "-e HTTP_PROXY") {
		t.Errorf("expected proxy passthrough in %q", joined)
	}
	// Secrets and task text must never appear in the argv.
	for _, leak := range []string{"sekret-token", "do the thing", "OPENCODE_API_KEY="} {
		if strings.Contains(joined, leak) {
			t.Errorf("docker args leak %q: %q", leak, joined)
		}
	}
}

func TestSandboxInnerCommand(t *testing.T) {
	prompt := "PROMPT-SENTINEL-do the thing"
	inner := sandboxInnerCommand("claude", 30)
	for _, want := range []string{`claude -p`, `"$(cat /prompt)"`, "--max-turns 30", "--dangerously-skip-permissions", "--output-format json"} {
		if !strings.Contains(inner, want) {
			t.Errorf("claude inner missing %q: %q", want, inner)
		}
	}
	inner = sandboxInnerCommand("codex", 30)
	for _, want := range []string{"codex login --with-api-key", "codex exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", `"$(cat /prompt)"`} {
		if !strings.Contains(inner, want) {
			t.Errorf("codex inner missing %q: %q", want, inner)
		}
	}
	if strings.Contains(inner, "OPENAI_API_KEY=") {
		t.Errorf("codex inner must not carry the token value: %q", inner)
	}
	inner = sandboxInnerCommand("opencode", 30)
	for _, want := range []string{"opencode run --auto", `"$(cat /prompt)"`} {
		if !strings.Contains(inner, want) {
			t.Errorf("opencode inner missing %q: %q", want, inner)
		}
	}
	// The prompt text itself travels via /prompt, never in the command.
	for _, name := range []string{"claude", "codex", "opencode"} {
		if strings.Contains(sandboxInnerCommand(name, 30), prompt) {
			t.Errorf("%s inner leaks prompt text", name)
		}
	}
	if got := sandboxInnerCommand("bogus", 30); got != "" {
		t.Errorf("unknown agent inner = %q, want empty", got)
	}
}
