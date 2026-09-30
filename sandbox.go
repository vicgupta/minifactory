package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// dockerBase builds the common prefix of a sandbox invocation: disposable,
// resource-capped, FULL EGRESS (agents need package registries), workdir
// mounted at /work. No secrets are ever passed into a container.
func dockerBase(workdir string) []string {
	return []string{"run", "--rm",
		"--cpus", "2", "--memory", "4g", "--pids-limit", "256",
		"-v", workdir + ":/work",
		"-w", "/work", workerImage}
}

// runAgent runs the selected agent for a task and returns the agent name.
//
// stub: deterministic stub binary (/opt/stub_agent) inside a sandbox.
// claude/codex/opencode: the vendor CLI runs ON THE HOST as trusted
// tooling. Generated code still only ever executes inside disposable
// sandboxes — see runTestsInSandbox.
func runAgent(t *Task) string {
	workdir := filepath.Join(workDir, t.ID)
	agent := selectAgent()
	logf(t.ID, "agent selected: "+agent)
	if agent == "stub" {
		args := append(dockerBase(workdir), "/opt/stub_agent")
		logf(t.ID, "sandbox: docker %s", strings.Join(args, " "))
		rc, out := runCmd(sandboxTimeout, "", nil, "docker", args...)
		logf(t.ID, "sandbox: exit=%d\n%s", rc, tailStr(out, 3000))
		return agent
	}
	runCLI(t, workdir, agent)
	return agent
}

// detectTestCmd picks the repo's test command. A repo may declare its own
// via factory.json {"test": "..."}; otherwise guess from the layout.
func detectTestCmd(workdir string) string {
	if b, err := os.ReadFile(filepath.Join(workdir, "factory.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			if s, ok := m["test"].(string); ok && s != "" {
				return s
			}
		}
	}
	if _, err := os.Stat(filepath.Join(workdir, "package.json")); err == nil {
		return "npm test --silent"
	}
	return "python3 -m pytest -q"
}

// hasTestSuite reports whether the repo looks like it has a runnable test
// suite. A repo with no tests at all (e.g. docs-only) makes pytest exit 5
// ("no tests collected"), which is not a failure — so the sandbox run is
// skipped for such repos instead of failing the task.
func hasTestSuite(workdir string) bool {
	// An explicit factory.json test command is authoritative: if the repo
	// declares how to test itself, there is a suite to run.
	if b, err := os.ReadFile(filepath.Join(workdir, "factory.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			if s, ok := m["test"].(string); ok && strings.TrimSpace(s) != "" {
				return true
			}
		}
	}
	// Node: package.json with a test script.
	if b, err := os.ReadFile(filepath.Join(workdir, "package.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			if scripts, ok := m["scripts"].(map[string]any); ok {
				if _, ok := scripts["test"]; ok {
					return true
				}
			}
		}
	}
	// Python: pytest config files, a tox config, or a tests directory imply
	// a suite even when no test files are visible yet. pyproject.toml only
	// counts when it actually configures pytest (it is often just build
	// metadata); setup.cfg likewise.
	if _, err := os.Stat(filepath.Join(workdir, "pytest.ini")); err == nil {
		return true
	}
	if b, err := os.ReadFile(filepath.Join(workdir, "pyproject.toml")); err == nil {
		if strings.Contains(string(b), "[tool.pytest") {
			return true
		}
	}
	if b, err := os.ReadFile(filepath.Join(workdir, "setup.cfg")); err == nil {
		if strings.Contains(string(b), "[tool:pytest") {
			return true
		}
	}
	for _, name := range []string{"tox.ini", "tests", "test"} {
		if _, err := os.Stat(filepath.Join(workdir, name)); err == nil {
			return true
		}
	}
	// Python: any test_*.py or *_test.py file.
	found := false
	filepath.WalkDir(workdir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "__pycache__", "node_modules", ".venv", "venv", ".tox":
				return filepath.SkipDir
			}
			return nil
		}
		n := d.Name()
		if strings.HasPrefix(n, "test_") && strings.HasSuffix(n, ".py") ||
			strings.HasSuffix(n, "_test.py") {
			found = true
		}
		return nil
	})
	return found
}

// runTestsInSandbox independently re-runs the repo test suite inside a
// disposable sandbox. For real agents this is the trust boundary: the agent
// ran on the host, so its generated code gets executed here, never on the
// host. The stub already ran its tests in-sandbox, so this is skipped for it.
func runTestsInSandbox(t *Task) bool {
	workdir := filepath.Join(workDir, t.ID)
	if !hasTestSuite(workdir) {
		logf(t.ID, "sandbox tests: no test suite detected — skipping (nothing to verify)")
		return true
	}
	testCmd := detectTestCmd(workdir)
	args := append(dockerBase(workdir), "bash", "-c", testCmd)
	logf(t.ID, "sandbox tests: docker %s", strings.Join(args, " "))
	rc, out := runCmd(sandboxTimeout, "", nil, "docker", args...)
	logf(t.ID, "sandbox tests: exit=%d\n%s", rc, tailStr(out, 3000))
	return rc == 0
}
