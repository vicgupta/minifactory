package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
// claude/codex/opencode: the vendor CLI runs inside the disposable worker
// sandbox — only the task workdir is mounted, only that agent's own token
// is injected, and the container is removed afterwards. When docker or the
// worker image is unavailable the CLI falls back to the host with a
// scrubbed environment and a loud warning: weaker isolation, but the
// factory keeps working (e.g. macOS without docker).
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
	if agentSandboxOK() {
		runAgentSandboxed(t, workdir, agent)
		return agent
	}
	logf(t.ID, "agent %s: WARNING — sandbox unavailable (docker or worker image missing), running on host with scrubbed env", agent)
	runCLI(t, workdir, agent)
	return agent
}

// agentTokenEnv maps an agent runner to the environment variable its CLI
// reads the token from.
func agentTokenEnv(name string) string {
	switch name {
	case "claude":
		return "CLAUDE_CODE_OAUTH_TOKEN"
	case "codex":
		return "OPENAI_API_KEY"
	case "opencode":
		return "OPENCODE_API_KEY"
	}
	return ""
}

// sandboxOKCached caches the per-process sandbox probe: run-once handles
// one task per process, so the docker/image check runs at most once.
var sandboxOKCached *bool

// agentSandboxOK reports whether a real agent can run inside the disposable
// worker sandbox: the docker CLI is on PATH and the worker image (which
// carries the vendor CLIs) exists locally.
func agentSandboxOK() bool {
	if sandboxOKCached != nil {
		return *sandboxOKCached
	}
	ok := false
	if _, err := exec.LookPath("docker"); err == nil {
		rc, _ := runCmd(15*time.Second, "", nil, "docker", "image", "inspect", workerImage)
		ok = rc == 0
	}
	sandboxOKCached = &ok
	return ok
}

// writeAgentStaging creates a per-run staging dir (0700, outside the repo
// checkout) holding the agent's token env-file and the task prompt file.
// Only the selected agent's own token is staged — never the whole .env —
// and the caller removes the dir after the run, so the token and the task
// text never enter the repo, the git history, the docker argv, or ps output.
func writeAgentStaging(tid, tokenKV, prompt string) (string, error) {
	dir, err := os.MkdirTemp("", "minifactory-agent-")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "token.env"), []byte(tokenKV+"\n"), 0600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte(prompt), 0600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// sandboxDockerBase builds the docker run prefix shared by sandboxed agent
// runs: disposable (--rm), resource-capped, the workdir mounted at /work,
// the staging dir's token env-file injected, and the prompt file mounted
// read-only at /prompt. Proxy variables pass through when set on the host
// (agents need model API access); everything else stays out of the
// container.
func sandboxDockerBase(workdir, stagingDir string) []string {
	args := []string{"run", "--rm",
		"--cpus", "2", "--memory", "4g", "--pids-limit", "256",
		"--env-file", filepath.Join(stagingDir, "token.env"),
		"-v", workdir + ":/work",
		"-v", filepath.Join(stagingDir, "prompt.txt") + ":/prompt:ro",
		"-w", "/work",
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if _, ok := os.LookupEnv(k); ok {
			args = append(args, "-e", k)
		}
	}
	return append(args, workerImage)
}

// sandboxInnerCommand builds the in-container shell command for an agent.
// The prompt is read from /prompt (mounted read-only), so no task text
// ever appears in the docker argv. codex registers its key via
// `codex login` first — a fresh container has no stored auth — with the
// token arriving via the env-file, never argv.
func sandboxInnerCommand(name string, maxTurns int) string {
	p := `"$(cat /prompt)"`
	switch name {
	case "claude":
		return fmt.Sprintf("claude -p %s --output-format json --max-turns %d --dangerously-skip-permissions", p, maxTurns)
	case "codex":
		return fmt.Sprintf("printf '%%s' \"$OPENAI_API_KEY\" | codex login --with-api-key >/dev/null 2>&1 && exec codex exec --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox %s", p)
	case "opencode":
		return fmt.Sprintf("opencode run --auto %s", p)
	}
	return ""
}

// runAgentSandboxed runs a real agent CLI inside the disposable worker
// sandbox and returns nothing — the outcome surfaces via result.json, as
// with host runs. On a staging failure it falls back to the host path
// rather than failing the task outright.
func runAgentSandboxed(t *Task, workdir, name string) {
	prompt := agentPrompt(t)
	staging, err := writeAgentStaging(t.ID, agentTokenEnv(name)+"="+agentToken(name), prompt)
	if err != nil {
		logf(t.ID, "agent %s: staging failed (%v) — falling back to host", name, err)
		runCLI(t, workdir, name)
		return
	}
	defer os.RemoveAll(staging)
	args := append(sandboxDockerBase(workdir, staging), "bash", "-c", sandboxInnerCommand(name, maxTurns))
	logf(t.ID, "agent %s: sandboxed run in %s (workdir-only mount, token-only env)", name, workerImage)
	rc, out := runCmd(agentTimeout, "", nil, "docker", args...)
	logf(t.ID, "agent %s: exit=%d\n%s", name, rc, tailStr(out, 3000))
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
// disposable sandbox. For real agents this is the trust boundary on the
// generated code: the agent itself ran sandboxed (or on the host in the
// docker-unavailable fallback), so its tests_passed claim is verified here,
// never trusted. The stub already ran its tests in-sandbox, so this is
// skipped for it.
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
