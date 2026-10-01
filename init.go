package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// envTemplate is the fresh .env written by `minifactory init`. Values are
// empty; the user fills them in and verifies with `minifactory doctor`.
// Never print this with values substituted — and init never prints values.
func envTemplate() string {
	return fmt.Sprintf(`# minifactory configuration — created by `+"`minifactory init`"+` on %s
# Fill in the values below, then run `+"`minifactory doctor`"+` to verify.
# This file holds secrets: keep it mode 600.

# Required. Classic PAT with `+"`repo`"+` scope: clone/push, PRs, issue labels.
GITHUB_TOKEN=

# Optional. owner/repo for `+"`poll`"+` issue intake; unset disables poll.
GITHUB_REPO=

# At least one agent token enables that runner (priority: claude > codex > opencode).
# Without any, tasks run with the in-sandbox stub agent.
# Claude subscription OAuth token from `+"`claude setup-token`"+`.
CLAUDE_CODE_OAUTH_TOKEN=
# Optional, for the codex agent.
CODEX_TOKEN=
# Optional, for the opencode agent.
OPENCODE_TOKEN=

# Optional. Force which agent runs tasks: claude|codex|opencode|stub.
# Unset (or a choice whose token is missing) falls back to token priority:
# claude > codex > opencode > stub. Use stub for dry runs.
AGENT=

# Optional. Turn budget per task for the claude runner (default 30).
# Raise for large tasks; each turn costs model usage.
MAX_TURNS=
`, time.Now().Format("2006-01-02 15:04:05"))
}

// backupPath returns a free <path>.bak.<timestamp> name, appending -2, -3,
// ... if several backups land in the same second. Used for .env and
// data/queue.json alike.
func backupPath(path string) string {
	base := path + ".bak." + time.Now().Format("20060102-150405")
	p := base
	for i := 2; ; i++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
		p = fmt.Sprintf("%s-%d", base, i)
	}
}

// backupFile copies src to a free backupPath(src) name with the given mode.
func backupFile(src string, mode os.FileMode) (string, error) {
	bak := backupPath(src)
	b, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(bak, b, mode); err != nil {
		return "", err
	}
	return bak, nil
}

// initFactory sets up a fresh factory runtime directory:
//   - creates the base, data/, logs/, work/ directories
//   - resets data/queue.json to [], backing up an existing one to
//     data/queue.json.bak.<timestamp> first
//   - backs up an existing .env to .env.bak.<timestamp> (mode 600), then
//     writes a fresh .env template (mode 600)
//   - rereads the configuration and reports which keys are set, without
//     ever printing values
//   - ensures the factory issue labels (factory-ready, factory-inProgress,
//     factory-completed) exist on the configured GitHub repo, using the
//     pre-existing configuration captured before the rewrite
//
// It returns 0 on success, 1 on filesystem errors. A template with empty
// values is not an error: the user fills it in afterwards.
func initFactory(dir string) int {
	fmt.Printf("initializing minifactory in %s\n", dir)

	for _, d := range []string{dir,
		filepath.Join(dir, "data"),
		filepath.Join(dir, "logs"),
		filepath.Join(dir, "work")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "init: cannot create %s: %v\n", d, err)
			return 1
		}
	}
	fmt.Println("  dirs ok: data/ logs/ work/")

	qpath := filepath.Join(dir, "data", "queue.json")
	if _, err := os.Stat(qpath); err == nil {
		bak, err := backupFile(qpath, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "init: cannot back up %s: %v\n", qpath, err)
			return 1
		}
		fmt.Printf("  backed up existing queue -> %s\n", filepath.Join("data", filepath.Base(bak)))
	}
	if err := os.WriteFile(qpath, []byte("[]\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "init: cannot write %s: %v\n", qpath, err)
		return 1
	}
	fmt.Println("  queue reset: data/queue.json")

	envPath := filepath.Join(dir, ".env")
	// Capture the pre-existing configuration before it is backed up and
	// replaced: the label setup below runs against the repo this factory
	// dir was managing.
	preCfg, _ := loadEnvFrom(dir)
	if _, err := os.Stat(envPath); err == nil {
		bak, err := backupFile(envPath, 0600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "init: cannot back up %s: %v\n", envPath, err)
			return 1
		}
		fmt.Printf("  backed up existing .env -> %s\n", filepath.Base(bak))
	}
	if err := os.WriteFile(envPath, []byte(envTemplate()), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "init: cannot write %s: %v\n", envPath, err)
		return 1
	}
	// Enforce 600 even if the file already existed with looser perms.
	os.Chmod(envPath, 0600)
	fmt.Println("  wrote fresh .env template (mode 600)")

	// Reread the configuration and report status — values never printed.
	cfg, _ := loadEnvFrom(dir)
	fmt.Println("configuration:")
	keys := []string{"GITHUB_TOKEN", "GITHUB_REPO", "CLAUDE_CODE_OAUTH_TOKEN", "CODEX_TOKEN", "OPENCODE_TOKEN"}
	for _, k := range keys {
		fmt.Printf("  %s: %s\n", k, onOff(cfg[k] != ""))
	}
	fmt.Printf("  MAX_TURNS: %d\n", parseMaxTurns(cfg["MAX_TURNS"]))
	agent := strings.ToLower(strings.TrimSpace(cfg["AGENT"]))
	if agent == "" {
		fmt.Println("  AGENT: unset (token priority)")
	} else {
		fmt.Printf("  AGENT: %s\n", agent)
	}
	if cfg["GITHUB_TOKEN"] == "" {
		fmt.Println("  -> GITHUB_TOKEN is required: set it in .env, then run `minifactory doctor`")
	} else if cfg["GITHUB_REPO"] == "" {
		fmt.Println("  -> GITHUB_REPO unset: issue intake (poll) stays disabled")
	}
	if cfg["CLAUDE_CODE_OAUTH_TOKEN"] == "" && cfg["CODEX_TOKEN"] == "" && cfg["OPENCODE_TOKEN"] == "" {
		fmt.Println("  -> no agent token: tasks will run with the in-sandbox stub agent")
	}

	// Ensure the factory issue-label lifecycle set exists on the configured
	// repo, creating whatever is missing. Uses the pre-existing
	// configuration: after the rewrite above, .env is always a fresh
	// template. Best-effort — a failure warns but doesn't fail init.
	ensureLabels(preCfg["GITHUB_TOKEN"], preCfg["GITHUB_REPO"])
	return 0
}

// ensureLabels makes sure the factory's issue labels exist on repo
// ("owner/repo" short form or full URL). Skips quietly when no token or no
// repo is configured.
func ensureLabels(token, repo string) {
	if token == "" || repo == "" {
		fmt.Println("  labels: skipped (GITHUB_TOKEN/GITHUB_REPO not configured)")
		return
	}
	org, name, ok := parseGithubRepo(repo)
	if !ok {
		fmt.Fprintf(os.Stderr, "init: warning: cannot parse GITHUB_REPO %q, labels not ensured\n", repo)
		return
	}
	// ensureFactoryLabels reads the globals; run it with the pre-existing
	// config's token in place.
	old := githubToken
	githubToken = token
	err := ensureFactoryLabels(org, name)
	githubToken = old
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: warning: label setup on %s failed: %v\n", repo, err)
		return
	}
	fmt.Printf("  labels ok on %s: %s\n", repo, strings.Join(factoryLabels, ", "))
}

// initWouldClobber reports live state that initFactory would destroy in dir:
// a configured .env (any credential value set) and/or a non-empty queue.
// Returned strings describe each item at risk; empty means dir is fresh.
func initWouldClobber(dir string) []string {
	var atRisk []string
	cfg, _ := loadEnvFrom(dir)
	for _, k := range []string{"GITHUB_TOKEN", "GITHUB_REPO", "CLAUDE_CODE_OAUTH_TOKEN", "CODEX_TOKEN", "OPENCODE_TOKEN"} {
		if cfg[k] != "" {
			atRisk = append(atRisk, ".env is configured ("+k+" is set)")
			break
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "data", "queue.json")); err == nil {
		var tasks []json.RawMessage
		if json.Unmarshal(b, &tasks) == nil && len(tasks) > 0 {
			atRisk = append(atRisk, fmt.Sprintf("data/queue.json holds %d task(s)", len(tasks)))
		}
	}
	return atRisk
}

// cmdInit implements `minifactory init [--dir path] [--force]`. Without
// --dir it initializes the factory's own directory (MINIFACTORY_DIR or the
// binary's directory). Refusing to clobber live state without --force:
// init rewrites .env and resets the queue, so a configured .env or a
// non-empty queue aborts with exit 2 unless --force is given.
func cmdInit(args []string) int {
	dir := flagVal(args, "dir")
	if dir == "" {
		dir = baseDir
	}
	if !hasFlag(args, "force") {
		if atRisk := initWouldClobber(dir); len(atRisk) > 0 {
			fmt.Fprintf(os.Stderr, "init: refusing to reset live state in %s:\n", dir)
			for _, r := range atRisk {
				fmt.Fprintf(os.Stderr, "  - %s\n", r)
			}
			fmt.Fprintf(os.Stderr, "re-run with --force to proceed (existing .env and queue are backed up first)\n")
			return 2
		}
	}
	return initFactory(dir)
}
