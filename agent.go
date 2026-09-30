package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// defaultMaxTurns is the agent turn budget per task when MAX_TURNS is unset
// or invalid.
const defaultMaxTurns = 30

// parseMaxTurns reads the MAX_TURNS .env knob: a positive integer, otherwise
// the default. Invalid values warn on stderr and fall back.
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

// selectAgent picks the agent runner from the configured tokens.
//
// Agent selection is token-driven; MAX_TURNS (default 30) caps the claude
// runner's turns per task. Priority when several tokens are set:
// claude > codex > opencode. No token: stub.
//
// CLAUDE_CODE_OAUTH_TOKEN is a Claude subscription token: generate it with
// `claude setup-token` on a machine logged into a Claude Pro/Max plan
// (no API credits used).
func selectAgent() string {
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

// agentPrompt is the contract handed to a real agent CLI: implement the
// task, run the tests, and write result.json. Commits/PRs stay with the
// orchestrator.
func agentPrompt(t *Task) string {
	body := t.Body
	if body == "" {
		body = "(no details)"
	}
	return fmt.Sprintf(`You are working on a software task. Your working directory is the repo root.
Task: %s
Details: %s

The task is also in task.json in the repo root.
Rules:
1. Explore the repo, then implement the change the task describes. Keep it minimal.
2. Run the repo's test suite (e.g. `+"`python3 -m pytest -q`"+` or `+"`npm test`"+`) until it passes.
3. Write result.json in the repo root with exactly this shape:
   {"changed_files": ["path", ...], "tests_passed": true, "summary": "one line"}
   Set tests_passed honestly — false if anything fails.
4. Do NOT commit, push, or open pull requests — the orchestrator handles that.
5. Do NOT touch anything outside the working directory.
`, t.Title, body)
}

// runCLI runs a vendor agent CLI ON THE HOST as trusted tooling (it needs
// its token + model API access). The token is mapped to the CLI's own env
// var and never enters a sandbox container. The CLI edits files only inside
// the task workdir.
func runCLI(t *Task, workdir, name string) bool {
	var cli, envKV string
	var args []string
	prompt := agentPrompt(t)
	switch name {
	case "claude":
		cli, envKV = "claude", "CLAUDE_CODE_OAUTH_TOKEN="+claudeToken
		args = []string{"-p", prompt, "--output-format", "json", "--max-turns", strconv.Itoa(maxTurns), "--allowed-tools", "Write,Edit,Bash"}
	case "codex":
		cli, envKV = "codex", "OPENAI_API_KEY="+codexToken
		args = []string{"exec", prompt}
	case "opencode":
		cli, envKV = "opencode", "OPENCODE_API_KEY="+opencodeToken
		args = []string{"run", prompt}
	default:
		return false
	}
	if _, err := exec.LookPath(cli); err != nil {
		logf(t.ID, "agent %s: `%s` not found on host PATH", name, cli)
		return false
	}
	logf(t.ID, "agent %s: running `%s` in %s", name, cli, workdir)
	if name == "claude" {
		logf(t.ID, "agent %s: max-turns=%d", name, maxTurns)
	}
	rc, out := runCmd(agentTimeout, workdir, []string{envKV}, cli, args...)
	logf(t.ID, "agent %s: exit=%d\n%s", name, rc, tailStr(out, 3000))
	return true
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

// runSizer runs the sizing pass in workdir and returns the verdict, or nil
// to proceed with the main agent. Fail-open by design: non-claude agents,
// a missing CLI, errors, and invalid size.json all mean "fit".
func runSizer(t *Task, workdir string) *sizeVerdict {
	if selectAgent() != "claude" {
		logf(t.ID, "sizer: skipping (agent is %s, sizing needs claude)", selectAgent())
		return nil
	}
	if _, err := exec.LookPath("claude"); err != nil {
		logf(t.ID, "sizer: `claude` not found on host PATH, skipping")
		return nil
	}
	fitBudget := maxTurns * 2 / 3
	prompt := sizePrompt(t, fitBudget)
	args := []string{"-p", prompt, "--output-format", "json",
		"--max-turns", strconv.Itoa(sizeMaxTurns),
		"--allowed-tools", "Read,Write,Bash"}
	logf(t.ID, "sizer: running `claude` (max-turns=%d, fit budget ~%d)", sizeMaxTurns, fitBudget)
	rc, out := runCmd(sizerTimeout, workdir, []string{"CLAUDE_CODE_OAUTH_TOKEN=" + claudeToken}, "claude", args...)
	logf(t.ID, "sizer: exit=%d\n%s", rc, tailStr(out, 1500))
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
