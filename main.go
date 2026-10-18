// minifactory — the whole software factory in one stdlib-only Go binary.
//
// Pipeline: queue (JSON) -> Docker sandbox per task (full egress)
//
//	-> branch + PR. The human gate is merging the PR in GitHub.
//
// No web server, no UI, no webhook listener.
//
// Agent selection is token-driven — the four tokens in .env are the ONLY
// user configuration. GITHUB_TOKEN (required) for clone/push/PRs;
// CLAUDE_CODE_OAUTH_TOKEN (subscription token from `claude setup-token`),
// CODEX_TOKEN, OPENCODE_TOKEN (each optional) enable the
// claude / codex / opencode runners. Priority: claude > codex > opencode.
// No agent token: stub.
//
// Subcommands:
//
//	issue --repo URL --title T --body B [--issue N]
//	                                      enqueue a task (prints task id);
//	                                      creates a GitHub issue on the repo
//	                                      and labels it factory-ready,
//	                                      unless --issue N links an existing
//	                                      one
//	run-once                              run the next queued task end-to-end
//	sync                                  update pr_open tasks from GitHub PR state
//	list [--json] [--state S[,S...]]       show tasks
//	logs <id>                             show a task's log
//	poll                                  enqueue new `ready`-labeled GitHub issues
//	retry <id> [--force]                  reclaim a stuck/failed task
//	report [--since Nd] [--task id] [--json]
//	                                      run records: aggregate stats,
//	                                      per-task attempts, costs, timings
//	doctor [--json]                       detailed health analysis
//	init [--dir path] [--skip-deps] [--skip-path]
//	                                      initialize a factory dir: runtime dirs,
//	                                      fresh .env (old one backed up), config
//	                                      check, git/docker check+install,
//	                                      PATH/profile enablement
//	version                               print the factory version
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	baseDir        string
	dataDir        string
	workDir        string
	logDir         string
	workerImage    = "factory-worker-go" // python3.12 + git + node + pytest + /opt/stub_agent
	sandboxTimeout = 10 * time.Minute
	agentTimeout   = 20 * time.Minute
	sizerTimeout   = 8 * time.Minute

	env           map[string]string
	envFound      bool   // .env was found and readable at startup
	envFile       string // resolved .env path used at startup
	verboseLog    bool   // --log: narrate every step on stdout
	githubToken   string
	githubRepo    string // optional: owner/repo for `poll` issue intake
	claudeToken   string
	codexToken    string
	opencodeToken string
	agentChoice   string            // optional AGENT override: claude|codex|opencode|stub
	baseBranch    string            // optional BASE_BRANCH; "" = repo default
	maxTurns      = defaultMaxTurns // agent turn budget per task (MAX_TURNS)
	maxCostUSD    float64           // per-task spend cap (MAX_COST_USD_PER_TASK); 0 = off
)

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// resolveBaseDirWith decides the factory base directory — the directory
// holding .env, data/queue.json, logs/ and work/ — without ever using the
// executable's directory (which may be a protected system dir such as
// /usr/local/bin). Precedence:
//
//  1. explicit override: MINIFACTORY_DIR, then MINIFACTORY_HOME
//     (MINIFACTORY_HOME names the factory home that replaces ~/.minifactory)
//  2. the current working directory, when it already contains a .env
//     (backwards compatible with a VPS install/work dir)
//  3. ~/.minifactory when a home directory is known
//  4. the current working directory when HOME is unset (never crashes)
func resolveBaseDirWith(cwd, home, dirOverride, homeOverride string, exists func(string) bool) string {
	if dirOverride != "" {
		return dirOverride
	}
	if homeOverride != "" {
		return homeOverride
	}
	if cwd != "" && exists(filepath.Join(cwd, ".env")) {
		return cwd
	}
	if home != "" {
		return filepath.Join(home, ".minifactory")
	}
	if cwd != "" {
		return cwd
	}
	return "."
}

// resolveEnvFileWith returns the .env path for the resolved base dir.
func resolveEnvFileWith(cwd, home, dirOverride, homeOverride string, exists func(string) bool) string {
	return filepath.Join(resolveBaseDirWith(cwd, home, dirOverride, homeOverride, exists), ".env")
}

// resolveBaseDir is resolveBaseDirWith against the real filesystem.
func resolveBaseDir(cwd, home string) string {
	return resolveBaseDirWith(cwd, home, os.Getenv("MINIFACTORY_DIR"), os.Getenv("MINIFACTORY_HOME"), fileExists)
}

// resolvedEnvPath returns the .env path in use: envFile when main has set
// it, otherwise baseDir/.env (the fallback keeps tests that set baseDir
// directly working).
func resolvedEnvPath() string {
	if envFile != "" {
		return envFile
	}
	return filepath.Join(baseDir, ".env")
}

// loadEnvPath parses KEY=VALUE lines from path, skipping # comments and
// blanks. The second return value reports whether the file was found and
// readable — callers that need configuration must fail fast when it
// isn't, instead of silently running with an empty env.
func loadEnvPath(path string) (map[string]string, bool) {
	m := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return m, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		m[k] = v
	}
	return m, true
}

// loadEnvFrom parses KEY=VALUE lines from dir/.env.
func loadEnvFrom(dir string) (map[string]string, bool) {
	return loadEnvPath(filepath.Join(dir, ".env"))
}

// loadEnv parses KEY=VALUE lines from the resolved .env file.
func loadEnv() (map[string]string, bool) {
	return loadEnvPath(resolvedEnvPath())
}

// runCmd runs a command with a timeout and the parent's environment plus
// extras. stdin is fed to the child (empty string → immediate EOF, never the
// parent's stdin): an agent CLI that reads stdin would otherwise block
// forever when the factory is started from a terminal. Secrets must never
// appear in args. Returns the exit code and combined stdout+stderr.
func runCmd(timeout time.Duration, dir string, envAdd []string, stdin string, name string, args ...string) (int, string) {
	return runCmdFull(timeout, dir, append(os.Environ(), envAdd...), stdin, name, args...)
}

// runAgentCmd is runCmd for agent subprocesses: the child gets a scrubbed
// environment (agentEnv) instead of the parent's full env, because the agent
// executes untrusted task text and must not see the orchestrator's secrets
// (GITHUB_TOKEN, other agents' tokens, ...).
func runAgentCmd(timeout time.Duration, dir string, envAdd []string, stdin string, name string, args ...string) (int, string) {
	return runCmdFull(timeout, dir, agentEnv(envAdd), stdin, name, args...)
}

// runCmdFull runs a command with a timeout and an explicit environment.
// A nil env inherits the parent's (exec default); a non-nil env replaces
// it entirely, so callers must include everything the child needs.
func runCmdFull(timeout time.Duration, dir string, env []string, stdin string, name string, args ...string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		cmd.Env = env
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	} else {
		cmd.Stdin = strings.NewReader("")
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if ctx.Err() == context.DeadlineExceeded {
		return -1, out + "\n[TIMEOUT]"
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), out
		}
		return -1, out + "\n[ERROR] " + err.Error()
	}
	return 0, out
}

// tailStr returns the last n bytes of s.
func tailStr(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func firstLine(s string, n int) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// logf appends a timestamped line to logs/<taskID>.log and prints it.
func logf(taskID, format string, a ...any) {
	os.MkdirAll(logDir, 0755)
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
	if f, err := os.OpenFile(filepath.Join(logDir, taskID+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		f.WriteString(line)
		f.Close()
	}
	fmt.Print(line)
}

// flagVal returns the value of --name value or --name=value from args.
func flagVal(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "--"+name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(args[i], "--"+name+"="); ok {
			return v
		}
	}
	return ""
}

// hasFlag reports whether --name appears in args.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--"+name {
			return true
		}
	}
	return false
}

func usage() {
	fmt.Fprintln(os.Stderr, `minifactory — queue -> sandbox -> PR

  issue --repo URL --title T --body B [--issue N]
                         enqueue a task (prints task id); creates a GitHub
                         issue on the repo and labels it factory-ready,
                         unless --issue N links an existing one
  run-once                              run the next queued task end-to-end
  sync                                  update pr_open tasks from GitHub PR state
  list [--json] [--state S[,S...]]       show tasks
  logs <id>                             show a task's log
  poll                                  enqueue new 'ready'-labeled GitHub issues
  retry <id> [--force]                  reclaim a stuck/failed task
  report [--since Nd] [--task id] [--json]
                                        run records: aggregate stats, per-task
                                        attempts, costs and stage timings
  doctor [--json] [--probe]           detailed health analysis of the factory
                                      (--probe spends one API call verifying
                                      the selected agent's token is accepted)
  init [--dir path] [--force] [--skip-deps] [--skip-path]
                                        initialize a factory dir (dirs, fresh .env
                                        with backup of the old one, config check,
                                        git/docker check and install on Linux,
                                        PATH/profile enablement for the binary's
                                        directory unless --skip-path;
                                        refuses live state without --force)
  install-cron [--dir path] [--schedule "*/5 * * * *"] [--remove] [--force]
                                        manage a per-user crontab entry running
                                        one cycle on the schedule (default every
                                        minute) — no root needed; guarded by flock
  update [--check]                      self-update to the latest GitHub release
                                        (downloads the binary for this platform,
                                        verifies its checksum, smoke-tests it,
                                        then swaps it in — no Go toolchain needed)
  delete-all [--dir path] [--user name] [--keep-user] [--force]
                                        tear down a factory installation:
                                        systemd units, crontab entry, the
                                        nologin service user, and the dir's
                                        managed state (prints a plan first;
                                        --force executes)
  rebuild-worker                        rebuild the worker image when it is
                                        missing or stale, without touching
                                        the queue or .env (init does this
                                        too, but also resets factory state)
  version                             print the factory version

  global flags:
  --log                               narrate every step on stdout
                                      (human-readable; don't script against it)`)
}

// checkEnvFor reports whether cmd may proceed given the .env read result.
// poll, run-once and sync act on configuration and fail fast when .env is
// missing or unreadable; everything else is local/diagnostic and stays usable.
func checkEnvFor(cmd string) error {
	switch cmd {
	case "poll", "run-once", "sync":
		if !envFound {
			return fmt.Errorf("no .env found at %s; run `minifactory init` to create one",
				resolvedEnvPath())
		}
	}
	return nil
}

// stripLogFlag removes --log / -log from args (wherever it appears) and
// reports whether it was present, so both `minifactory --log run-once`
// and `minifactory run-once --log` work without tripping the per-command
// flag parsing.
func stripLogFlag(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == "--log" || a == "-log" {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

// tracef narrates what the factory is doing, step by step, on stdout —
// but only when --log was given. Unlike logf it isn't tied to a task, so
// it covers poll/sync/retry/issue as well as the gaps between task log
// lines. Human-readable output; don't script against it.
func tracef(format string, a ...any) {
	if !verboseLog {
		return
	}
	fmt.Printf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}

func main() {
	os.Args, verboseLog = stripLogFlag(os.Args)
	// The factory base dir (and with it .env, data/queue.json, logs/ and
	// work/) is never anchored to the executable's directory: a binary in
	// /usr/local/bin or ~/.local/bin must not try to write state there.
	// Precedence: MINIFACTORY_DIR / MINIFACTORY_HOME override, then the
	// cwd when it already has a .env, then ~/.minifactory, then (when
	// HOME is unset) the cwd. See resolveBaseDirWith.
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir() // "" when HOME is unset; handled above
	baseDir = resolveBaseDir(cwd, home)
	envFile = filepath.Join(baseDir, ".env")
	dataDir = filepath.Join(baseDir, "data")
	workDir = filepath.Join(baseDir, "work")
	logDir = filepath.Join(baseDir, "logs")

	env, envFound = loadEnv()
	if envFound {
		tracef("config: .env loaded from %s", resolvedEnvPath())
	} else {
		tracef("config: no .env at %s", resolvedEnvPath())
	}
	githubToken = env["GITHUB_TOKEN"]
	githubRepo = env["GITHUB_REPO"]
	claudeToken = env["CLAUDE_CODE_OAUTH_TOKEN"]
	codexToken = env["CODEX_TOKEN"]
	opencodeToken = env["OPENCODE_TOKEN"]
	agentChoice = strings.ToLower(strings.TrimSpace(env["AGENT"]))
	maxTurns = parseMaxTurns(env["MAX_TURNS"])
	baseBranch = parseBaseBranch(env["BASE_BRANCH"])
	maxCostUSD = parseCostUSD(env["MAX_COST_USD_PER_TASK"])

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// Operations that act on configuration fail fast when .env is missing
	// or unreadable, instead of silently running with an empty env (which
	// once hid a real outage behind "poll: skipping"). init, doctor, list,
	// logs, version and retry are local/diagnostic and stay usable.
	if err := checkEnvFor(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tracef("minifactory %s: starting", os.Args[1])
	var rc int
	switch os.Args[1] {
	case "issue":
		rc = cmdIssue(os.Args[2:])
	case "run-once":
		rc = cmdRunOnce()
	case "sync":
		rc = cmdSync()
	case "list":
		rc = cmdList(os.Args[2:])
	case "logs":
		rc = cmdLogs(os.Args[2:])
	case "poll":
		rc = cmdPoll()
	case "retry":
		rc = cmdRetry(os.Args[2:])
	case "report":
		rc = cmdReport(os.Args[2:])
	case "doctor":
		rc = cmdDoctor(os.Args[2:])
	case "init":
		rc = cmdInit(os.Args[2:])
	case "install-cron":
		rc = cmdInstallCron(os.Args[2:])
	case "update":
		rc = cmdUpdate(os.Args[2:])
	case "delete-all":
		rc = cmdDeleteAll(os.Args[2:])
	case "rebuild-worker":
		rc = cmdRebuildWorker(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("minifactory " + version)
	default:
		usage()
		rc = 2
	}
	os.Exit(rc)
}
