package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultMaxTurns is the agent turn budget per task when MAX_TURNS is unset
// or invalid.
const defaultMaxTurns = 30

// parseMaxTurns reads the MAX_TURNS .env knob: a positive integer, otherwise
// the default. Invalid values warn on stderr and fall back.
// defaultMaxRevisions is the review-revision round cap when MAX_REVISIONS is
// unset or invalid.
const defaultMaxRevisions = 2

// parseMaxRevisions reads MAX_REVISIONS: a non-negative integer (0 disables
// revisions), otherwise the default.
func parseMaxRevisions(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultMaxRevisions
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "warning: invalid MAX_REVISIONS %q, using default %d\n", s, defaultMaxRevisions)
		return defaultMaxRevisions
	}
	return n
}

func parseMaxTurns(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultMaxTurns
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		fmt.Fprintf(os.Stderr, "warning: invalid MAX_TURNS %q, using default %d\n", s, defaultMaxTurns)
		return defaultMaxTurns
	}
	return n
}

// agentToken returns the configured token for a named agent runner,
// or "" when that runner needs none (stub) or has none configured.
func agentToken(name string) string {
	switch name {
	case "claude":
		return claudeToken
	case "codex":
		return codexToken
	case "opencode":
		return opencodeToken
	}
	return ""
}

// agentEnvAllow is the whitelist of parent environment variables an agent
// subprocess may inherit. Everything else is dropped — notably GITHUB_TOKEN,
// GITHUB_REPO, the other agents' tokens, SSH agent sockets and cloud
// credentials — because the agent executes untrusted task text and must not
// be able to exfiltrate the orchestrator's secrets. The runner's own token
// arrives via extra (see agentCommand).
var agentEnvAllow = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL",
	"LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	"TERM",
}

// agentEnv builds the scrubbed environment for an agent subprocess: the
// whitelisted parent variables that are set, TERM=dumb when the parent has
// no TERM, plus the runner-specific entries (its token mapping).
func agentEnv(extra []string) []string {
	env := make([]string, 0, len(agentEnvAllow)+len(extra)+1)
	termSeen := false
	for _, k := range agentEnvAllow {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
			if k == "TERM" {
				termSeen = true
			}
		}
	}
	if !termSeen {
		env = append(env, "TERM=dumb")
	}
	return append(env, extra...)
}

// agentWarned keeps the AGENT misconfiguration warning to one per
// process — selectAgent is called from several places per run.
var agentWarned = false

func warnAgentOnce(format string, a ...any) {
	if agentWarned {
		return
	}
	agentWarned = true
	fmt.Fprintf(os.Stderr, format, a...)
}

// selectAgent picks the agent runner.
//
// An explicit AGENT setting (claude|codex|opencode|stub) wins when it names
// a usable runner — i.e. its token is configured (stub needs none). An
// unknown AGENT, or one whose token is missing, warns on stderr and falls
// back to token priority. Without AGENT, selection is token-driven with
// priority claude > codex > opencode; no token at all means the stub.
//
// CLAUDE_CODE_OAUTH_TOKEN is a Claude subscription token: generate it with
// `claude setup-token` on a machine logged into a Claude Pro/Max plan
// (no API credits used).
func selectAgent() string {
	switch agentChoice {
	case "claude", "codex", "opencode", "stub":
		if agentChoice == "stub" || agentToken(agentChoice) != "" {
			return agentChoice
		}
		warnAgentOnce("warning: AGENT=%q has no token configured, falling back to token priority\n", agentChoice)
	case "":
		// no override — token priority below
	default:
		warnAgentOnce("warning: unknown AGENT=%q (want claude|codex|opencode|stub), falling back to token priority\n", agentChoice)
	}
	if claudeToken != "" {
		return "claude"
	}
	if codexToken != "" {
		return "codex"
	}
	if opencodeToken != "" {
		return "opencode"
	}
	return "stub"
}

// agentSelection describes how selectAgent chooses, for diagnostics:
// the effective runner and why (explicit AGENT vs token priority).
func agentSelection() (name, how string) {
	name = selectAgent()
	how = "token priority"
	if agentChoice != "" {
		if agentChoice == name {
			how = "AGENT=" + agentChoice + " override"
		} else {
			how = "token priority (AGENT=" + agentChoice + " unusable)"
		}
	}
	return name, how
}

// looksLikeAuthError reports whether CLI output smells like a dead or
// missing credential rather than a task failure. Used to fail fast
// (instead of burning the next pipeline stage on the same 401).
func looksLikeAuthError(out string) bool {
	l := strings.ToLower(out)
	for _, sig := range []string{
		"401",
		"failed to authenticate",
		"invalid bearer",
		"incorrect api key",
		"invalid api key",
		"invalid_api_key",
		"missing bearer",
		"authentication failed",
		"unauthorized",
	} {
		if strings.Contains(l, sig) {
			return true
		}
	}
	return false
}

// codexLoginMatches reports whether `codex login status` output shows a
// login whose key matches token. codex masks the key as e.g.
// "sk-test-***12345", so only the visible tail is compared.
func codexLoginMatches(statusOut, token string) bool {
	if !strings.Contains(statusOut, "Logged in") {
		return false
	}
	if len(token) < 8 {
		return false
	}
	return strings.Contains(statusOut, "***"+token[len(token)-5:])
}

// ensureCodexLogin makes sure `codex` can authenticate before a run.
// codex-cli (>=0.159) ignores OPENAI_API_KEY on its own: the key must be
// registered via `codex login --with-api-key`, which stores it in
// ~/.codex/auth.json (mode 600). Re-login is skipped when the stored key
// already matches CODEX_TOKEN. The token travels via stdin, never argv.
func ensureCodexLogin(id string) bool {
	if _, err := exec.LookPath("codex"); err != nil {
		logf(id, "agent codex: `codex` not found on host PATH")
		return false
	}
	if rc, out := runAgentCmd(30*time.Second, "", nil, "", "codex", "login", "status"); rc == 0 && codexLoginMatches(out, codexToken) {
		logf(id, "agent codex: already logged in")
		return true
	}
	logf(id, "agent codex: registering CODEX_TOKEN via `codex login --with-api-key`")
	rc, out := runAgentCmd(60*time.Second, "", nil, codexToken, "codex", "login", "--with-api-key")
	if rc != 0 {
		logf(id, "agent codex: login failed (exit=%d)\n%s", rc, tailStr(out, 1500))
		return false
	}
	logf(id, "agent codex: login ok")
	return true
}

// agentPrompt is the contract handed to a real agent CLI: implement the
// task, run the tests, and write result.json. Commits/PRs stay with the
// orchestrator.
func agentPrompt(t *Task, workdir string) string {
	body := t.Body
	if body == "" {
		body = "(no details)"
	}
	testCmd := detectTestCmd(workdir)
	return fmt.Sprintf(`You are working on a software task. Your working directory is the repo root.
Task: %s
Details: %s

The task is also in task.json in the repo root.
Rules:
1. Explore the repo, then implement the change the task describes. Keep it minimal.
2. Run the repo's test suite until it passes. The canonical test command for this
   repo is:
       %s
   Run exactly that command to judge tests_passed. Do NOT run any other scripts,
   especially integration or smoke tests under scripts/ (they need a deployed
   factory, docker, or a pre-built binary and are expected to fail in this
   sandbox) — ignore them.
3. Write result.json in the repo root with exactly this shape:
   {"changed_files": ["path", ...], "tests_passed": true, "summary": "one line"}
   changed_files must list EVERY file you changed, as a repo-root-relative
   path (no ./ prefix, no absolute paths) exactly as `+"`git status --porcelain`"+` reports it.
   Run `+"`git status --porcelain`"+` and copy its paths. Do NOT list result.json,
   task.json or size.json. Set tests_passed honestly — false only if the
   canonical test command above fails.
4. Do NOT commit, push, or open pull requests — the orchestrator handles that.
5. Do NOT touch anything outside the working directory.
`, t.Title, body, testCmd)
}

// agentCommand builds the host-side CLI invocation for an agent runner.
// It is pure (no auth checks, no PATH lookup) so tests can pin the exact
// flags each runner gets. Every runner must be fully non-interactive: no
// permission prompts, no stdin reads, because nobody can answer them in a
// factory run — a prompt is a hang until the agent timeout.
func agentCommand(name, prompt string, maxTurns int) (cli, envKV string, args []string, ok bool) {
	switch name {
	case "claude":
		return "claude", "CLAUDE_CODE_OAUTH_TOKEN=" + claudeToken,
			[]string{"-p", prompt, "--output-format", "json", "--max-turns", strconv.Itoa(maxTurns), "--dangerously-skip-permissions"}, true
	case "codex":
		return "codex", "OPENAI_API_KEY=" + codexToken,
			[]string{"exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", prompt}, true
	case "opencode":
		return "opencode", "OPENCODE_API_KEY=" + opencodeToken,
			[]string{"run", "--auto", prompt}, true
	default:
		return "", "", nil, false
	}
}

// runCLI runs a vendor agent CLI on the host with a scrubbed environment.
// This is the fallback when the docker sandbox is unavailable
// (agentSandboxOK): weaker isolation than a sandboxed run, but it keeps the
// factory working where docker isn't installed. The token is mapped to the
// CLI's own env var and never enters a sandbox container. Returns the
// command's full stdout (for usage parsing) and whether it exited zero.
func runCLI(t *Task, workdir, name string) (string, bool) {
	prompt := agentPrompt(t, workdir)
	cli, envKV, args, ok := agentCommand(name, prompt, maxTurns)
	if !ok {
		return "", false
	}
	if name == "codex" {
		// codex ignores OPENAI_API_KEY unless the key is registered via
		// `codex login`; ensureCodexLogin handles that (skipped when the
		// stored key already matches). The env mapping stays as a fallback
		// for CLI versions that do honor it.
		if !ensureCodexLogin(t.ID) {
			logf(t.ID, "agent codex: not authenticated — check CODEX_TOKEN")
			return "", false
		}
	}
	if _, err := exec.LookPath(cli); err != nil {
		logf(t.ID, "agent %s: `%s` not found on host PATH", name, cli)
		return "", false
	}
	logf(t.ID, "agent %s: running `%s` in %s", name, cli, workdir)
	if name == "claude" {
		logf(t.ID, "agent %s: max-turns=%d", name, maxTurns)
	}
	rc, out := runAgentCmd(agentTimeout, workdir, []string{envKV}, "", cli, args...)
	logf(t.ID, "agent %s: exit=%d\n%s", name, rc, tailStr(out, 3000))
	return out, rc == 0
}

// sizeMaxTurns caps the sizing pass: it must judge the task, not implement it.
const sizeMaxTurns = 5

// sizeVerdict is the machine-readable outcome of the sizing pass.
type sizeVerdict struct {
	Verdict  string `json:"verdict"` // "fit" or "split"
	Reason   string `json:"reason"`
	Subtasks []struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	} `json:"subtasks,omitempty"`
}

// sizePrompt is the contract for the sizing pass: explore just enough to
// judge scope, then write size.json. It must not implement anything.
func sizePrompt(t *Task, fitBudget int) string {
	body := t.Body
	if body == "" {
		body = "(no details)"
	}
	return fmt.Sprintf(`You are sizing a software task for an automated factory. Do NOT implement anything — judge only.

Task: %s
Details: %s

The repository is cloned at your working directory. The agent that would implement this task has a budget of %d turns (implementation + tests + writing result.json must all fit).

Assess quickly — a few directory listings and file reads at most. Do not write code.
Decide: can this task be completed comfortably within ~%d turns?

Write size.json in the working directory with exactly one of these shapes:
{"verdict": "fit", "reason": "<one line>"}
{"verdict": "split", "reason": "<one line>", "subtasks": [{"title": "<short>", "body": "<self-contained scope>"}, ...]}

Split rules:
- 2 to 6 subtasks, ordered by dependency (independent work first).
- Each subtask must be independently completable within ~10-15 turns by an agent that sees only the subtask title/body plus the repo.
- Each subtask body states what to build or change, the key files involved, and acceptance criteria. No overlap between subtasks.
- If in doubt whether the task fits, choose split.
`, t.Title, body, maxTurns, fitBudget)
}

// parseSizeVerdict validates a size.json payload. Anything malformed or
// out of contract returns nil, which the pipeline treats as "fit"
// (fail open — sizing is advisory, never a gate).
func parseSizeVerdict(b []byte) *sizeVerdict {
	var sv sizeVerdict
	if json.Unmarshal(b, &sv) != nil {
		return nil
	}
	sv.Verdict = strings.ToLower(strings.TrimSpace(sv.Verdict))
	switch sv.Verdict {
	case "fit":
		return &sv
	case "split":
		if len(sv.Subtasks) < 2 || len(sv.Subtasks) > 8 {
			return nil
		}
		for _, st := range sv.Subtasks {
			if strings.TrimSpace(st.Title) == "" || strings.TrimSpace(st.Body) == "" {
				return nil
			}
		}
		return &sv
	}
	return nil
}

// runSizer runs the sizing pass in workdir and returns the verdict, the
// sizer's own model usage (claude JSON output, nil when unavailable), and
// an error. Fail-open by design: non-claude agents, a missing CLI, errors,
// and invalid size.json all mean "fit". The one exception is an
// authentication failure: retrying the main agent with the same dead token
// only burns another call, so that aborts the run.
//
// The "size" stage is timed on the run record only when the sizer actually
// runs; the early skips below leave no stage behind.
func runSizer(t *Task, workdir string, rec *RunRecord) (*sizeVerdict, *AgentUsage, error) {
	if selectAgent() != "claude" {
		logf(t.ID, "sizer: skipping (agent is %s, sizing needs claude)", selectAgent())
		return nil, nil, nil
	}
	sandboxed := agentSandboxOK()
	if !sandboxed {
		if _, err := exec.LookPath("claude"); err != nil {
			logf(t.ID, "sizer: `claude` not found on host PATH, skipping")
			return nil, nil, nil
		}
	}
	rec.beginStage("size")
	var sv *sizeVerdict
	var usage *AgentUsage
	var err error
	if sandboxed {
		sv, usage, err = runSizerSandboxed(t, workdir)
	} else {
		sv, usage, err = runSizerHost(t, workdir)
	}
	rec.endStage(err == nil)
	return sv, usage, err
}

// runSizerSandboxed runs the sizing pass inside the disposable worker
// sandbox: only the workdir is mounted and only the claude token is
// injected, like the main agent run.
func runSizerSandboxed(t *Task, workdir string) (*sizeVerdict, *AgentUsage, error) {
	fitBudget := maxTurns * 2 / 3
	prompt := sizePrompt(t, fitBudget)
	staging, err := writeAgentStaging(t.ID, "CLAUDE_CODE_OAUTH_TOKEN="+claudeToken, prompt)
	if err != nil {
		return nil, nil, fmt.Errorf("sizer staging failed: %w", err)
	}
	defer os.RemoveAll(staging)
	inner := fmt.Sprintf(`claude -p "$(cat /prompt)" --output-format json --max-turns %d --allowed-tools Read,Write,Bash`, sizeMaxTurns)
	args := append(sandboxDockerBase(workdir, staging), "bash", "-c", inner)
	logf(t.ID, "sizer: sandboxed `claude` (max-turns=%d, fit budget ~%d)", sizeMaxTurns, fitBudget)
	rc, out := runCmd(sizerTimeout, "", nil, "", "docker", args...)
	logf(t.ID, "sizer: exit=%d\n%s", rc, tailStr(out, 1500))
	if looksLikeAuthError(out) {
		return nil, nil, fmt.Errorf("agent authentication failed — check CLAUDE_CODE_OAUTH_TOKEN in .env")
	}
	return readSizeVerdict(t, workdir), parseClaudeUsage(out), nil
}

// runSizerHost runs the sizing pass with the claude CLI on the host.
func runSizerHost(t *Task, workdir string) (*sizeVerdict, *AgentUsage, error) {
	fitBudget := maxTurns * 2 / 3
	prompt := sizePrompt(t, fitBudget)
	args := []string{"-p", prompt, "--output-format", "json",
		"--max-turns", strconv.Itoa(sizeMaxTurns),
		"--allowed-tools", "Read,Write,Bash"}
	logf(t.ID, "sizer: running `claude` on host (max-turns=%d, fit budget ~%d)", sizeMaxTurns, fitBudget)
	rc, out := runAgentCmd(sizerTimeout, workdir, []string{"CLAUDE_CODE_OAUTH_TOKEN=" + claudeToken}, "", "claude", args...)
	logf(t.ID, "sizer: exit=%d\n%s", rc, tailStr(out, 1500))
	if looksLikeAuthError(out) {
		return nil, nil, fmt.Errorf("agent authentication failed — check CLAUDE_CODE_OAUTH_TOKEN in .env")
	}
	return readSizeVerdict(t, workdir), parseClaudeUsage(out), nil
}

// readSizeVerdict loads and validates size.json. A nil verdict means "fit":
// sizing is advisory, never a gate.
func readSizeVerdict(t *Task, workdir string) *sizeVerdict {
	b, err := os.ReadFile(filepath.Join(workdir, "size.json"))
	if err != nil {
		logf(t.ID, "sizer: no size.json (%v) — treating as fit", err)
		return nil
	}
	sv := parseSizeVerdict(b)
	if sv == nil {
		logf(t.ID, "sizer: invalid size.json — treating as fit")
		return nil
	}
	logf(t.ID, "sizer: verdict=%s (%s)", sv.Verdict, sv.Reason)
	if sv.Verdict != "split" {
		return nil
	}
	return sv
}
