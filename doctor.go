package main

// doctor — detailed health analysis of the factory.
//
// `minifactory doctor` inspects every layer the pipeline depends on and
// reports what is healthy, what is degraded, and what is broken, with
// enough detail to act on. It is read-only (apart from ensuring the
// data/work/logs directories exist) and never prints secrets: token
// presence is reported as set/unset only.
//
// Layers checked: .env config, host tools (git/docker), the selected
// agent CLI, GitHub API reachability + rate limit, the Docker daemon +
// worker image, the systemd timer, directories, queue state (including
// stale-task detection), disk space, and recent factory activity.
//
// Exit code is 1 when any check fails, 0 otherwise (warnings alone do
// not fail the run). `doctor --json` emits the same report as JSON.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type checkStatus string

const (
	statusOK   checkStatus = "ok"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "fail"
	statusInfo checkStatus = "info"
)

type checkResult struct {
	Name   string      `json:"name"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail"`
}

// staleRunningAfter is how long a task may sit in "running" before doctor
// flags it as likely orphaned (a timer cycle died mid-run). A full run is
// bounded by clone + agent (20m) + sandbox (10m) + push, so 45m of slack
// is generous.
const staleRunningAfter = 45 * time.Minute

// queueReport is the pure, testable core of the queue analysis.
type queueReport struct {
	total        int
	counts       map[string]int
	staleRunning []string
	failed       []string
	prOpen       []string
	oldestQueued string // "id (age)" of the oldest still-queued task
}

// analyzeTasks summarizes task states and flags anomalies. Pure function
// of its inputs so it can be unit-tested without touching the store.
func analyzeTasks(tasks []Task, now time.Time) *queueReport {
	r := &queueReport{counts: map[string]int{}}
	for _, t := range tasks {
		r.total++
		r.counts[t.State]++
		switch t.State {
		case "running":
			if now.Sub(time.Unix(int64(t.CreatedAt), 0)) > staleRunningAfter {
				r.staleRunning = append(r.staleRunning, t.ID)
			}
		case "failed":
			r.failed = append(r.failed, t.ID)
		case "pr_open":
			r.prOpen = append(r.prOpen, t.ID)
		}
	}
	sort.Strings(r.staleRunning)
	sort.Strings(r.failed)
	sort.Strings(r.prOpen)
	var oldest *Task
	for i := range tasks {
		if tasks[i].State == "queued" && (oldest == nil || tasks[i].CreatedAt < oldest.CreatedAt) {
			oldest = &tasks[i]
		}
	}
	if oldest != nil {
		age := now.Sub(time.Unix(int64(oldest.CreatedAt), 0)).Round(time.Second)
		r.oldestQueued = fmt.Sprintf("%s (%s)", oldest.ID, age)
	}
	return r
}

// humanBytes formats a byte count for humans.
func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}

// shortDur renders a duration like "3h12m" (drops sub-second noise).
func shortDur(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// ---- individual checks ------------------------------------------------

func checkEnvFile() checkResult {
	path := filepath.Join(baseDir, ".env")
	st, err := os.Stat(path)
	if err != nil {
		return checkResult{"config (.env)", statusFail,
			"no .env in " + baseDir + " — run install.sh to create one"}
	}
	mode := st.Mode().Perm()
	var b strings.Builder
	fmt.Fprintf(&b, ".env present, mode %o", mode)
	if mode != 0600 {
		return checkResult{"config (.env)", statusWarn,
			b.String() + "\n      secrets are world-readable; chmod 600 " + path}
	}
	return checkResult{"config (.env)", statusOK, b.String()}
}

func checkTokens() checkResult {
	var lines []string
	status := statusOK
	lines = append(lines, "GITHUB_TOKEN: "+onOff(githubToken != ""))
	lines = append(lines, "GITHUB_REPO: "+onOff(githubRepo != "")+(func() string {
		if githubRepo != "" {
			return " (" + githubRepo + ")"
		}
		return ""
	}()))
	lines = append(lines, "agent tokens: claude="+onOff(claudeToken != "")+
		" codex="+onOff(codexToken != "")+" opencode="+onOff(opencodeToken != ""))
	name, how := agentSelection()
	lines = append(lines, "agent selected: "+name+" ("+how+")")
	lines = append(lines, fmt.Sprintf("max turns (claude): %d", maxTurns))
	if githubToken == "" {
		status = statusFail
		lines = append(lines, "-> GITHUB_TOKEN is required: poll, PRs and issue labels are all disabled without it")
	} else if githubRepo == "" {
		status = statusWarn
		lines = append(lines, "-> GITHUB_REPO unset: issue intake (poll) is disabled")
	}
	if claudeToken == "" && codexToken == "" && opencodeToken == "" {
		if status == statusOK {
			status = statusInfo
		}
		lines = append(lines, "-> no agent token: tasks run with the in-sandbox stub agent")
	}
	for _, tc := range []struct{ kind, token string }{
		{"claude", claudeToken},
		{"codex", codexToken},
		{"opencode", opencodeToken},
	} {
		if tc.token == "" {
			continue
		}
		if ok, hint := tokenShapeOK(tc.kind, tc.token); !ok {
			if status == statusOK {
				status = statusWarn
			}
			lines = append(lines, "-> "+tc.kind+" token looks wrong: "+hint)
		}
	}
	return checkResult{"tokens", status, strings.Join(lines, "\n      ")}
}

func onOff(b bool) string {
	if b {
		return "set"
	}
	return "not set"
}

// tokenShapeOK is a cheap sanity check that a configured token looks like
// the real thing — it catches stubs/placeholders (e.g. a 10-char value
// where a 100+ char `claude setup-token` output belongs), which the API
// would reject with a 401. It is deliberately loose: a pass is not proof
// the token is valid, only that it isn't obviously fake.
func tokenShapeOK(kind, token string) (bool, string) {
	switch kind {
	case "claude":
		if !strings.HasPrefix(token, "sk-ant-oat01-") || len(token) < 40 {
			return false, "want the `claude setup-token` value (sk-ant-oat01-…, 100+ chars)"
		}
	case "codex":
		if !strings.HasPrefix(token, "sk-") || len(token) < 20 {
			return false, "want an OpenAI API key (sk-…, 40+ chars)"
		}
	case "opencode":
		if len(token) < 8 {
			return false, "suspiciously short"
		}
	}
	return true, ""
}

func checkTools() checkResult {
	var lines []string
	status := statusOK
	if p, err := exec.LookPath("git"); err != nil {
		status = statusFail
		lines = append(lines, "git: NOT FOUND on PATH")
	} else {
		rc, out := runCmd(10*time.Second, "", nil, "git", "--version")
		ver := "?"
		if rc == 0 {
			ver = strings.TrimSpace(firstLine(out, 60))
		}
		lines = append(lines, "git: "+ver+" ("+p+")")
	}
	if p, err := exec.LookPath("docker"); err != nil {
		status = statusFail
		lines = append(lines, "docker: NOT FOUND on PATH")
	} else {
		lines = append(lines, "docker CLI: present ("+p+")")
	}
	if status == statusFail {
		lines = append(lines, "-> install missing tools before the factory can run")
	}
	return checkResult{"host tools", status, strings.Join(lines, "\n      ")}
}

func checkAgentCLI() checkResult {
	agent := selectAgent()
	if agent == "stub" {
		return checkResult{"agent (" + agent + ")", statusInfo,
			"no agent token configured — tasks run the deterministic stub in a sandbox"}
	}
	p, err := exec.LookPath(agent)
	if err != nil {
		return checkResult{"agent (" + agent + ")", statusFail,
			"token is set but `" + agent + "` is not on the host PATH — install the CLI or clear the token"}
	}
	rc, out := runCmd(10*time.Second, "", nil, agent, "--version")
	ver := strings.TrimSpace(firstLine(out, 80))
	if rc != 0 || ver == "" {
		ver = "(version check failed)"
	}
	return checkResult{"agent (" + agent + ")", statusOK, ver + "\n      " + p}
}

// probeAgentAuth spends one real API call to verify the selected agent's
// token is actually accepted. Opt-in via `doctor --probe`: doctor is
// otherwise fast and free, while a probe costs model usage and can take
// ~2 minutes. This is the check that would have caught the dead
// CLAUDE_CODE_OAUTH_TOKEN (doctor's `--version` probe never authenticates).
func probeAgentAuth() checkResult {
	agent := selectAgent()
	name := "agent auth probe (" + agent + ")"
	if agent == "stub" {
		return checkResult{name, statusInfo, "stub agent needs no token — nothing to probe"}
	}
	if _, err := exec.LookPath(agent); err != nil {
		return checkResult{name, statusFail, "`" + agent + "` not on host PATH"}
	}
	var rc int
	var out string
	switch agent {
	case "claude":
		rc, out = runCmd(90*time.Second, "", []string{"CLAUDE_CODE_OAUTH_TOKEN=" + claudeToken},
			"claude", "-p", "Reply with exactly: ok", "--output-format", "json", "--max-turns", "1")
	case "codex":
		if !ensureCodexLogin("doctor-probe") {
			return checkResult{name, statusFail, "codex login failed — check CODEX_TOKEN (see logs/doctor-probe.log)"}
		}
		rc, out = runCmd(120*time.Second, "", []string{"OPENAI_API_KEY=" + codexToken},
			"codex", "exec", "Reply with exactly: ok")
	case "opencode":
		rc, out = runCmd(120*time.Second, "", []string{"OPENCODE_API_KEY=" + opencodeToken},
			"opencode", "run", "Reply with exactly: ok")
	}
	status := statusOK
	detail := "agent token accepted by the API"
	if looksLikeAuthError(out) {
		status = statusFail
		detail = "agent token REJECTED by the API — re-enter it in .env"
	} else if rc != 0 {
		status = statusWarn
		detail = "probe inconclusive (exit=" + strconv.Itoa(rc) + ")"
	}
	return checkResult{name, status, detail + "\n      " + tailStr(out, 800)}
}

func checkGitHub() checkResult {
	if githubToken == "" {
		return checkResult{"github api", statusInfo, "skipped — GITHUB_TOKEN not set"}
	}
	var lines []string
	b, err := ghDo("GET", "/user", nil)
	if err != nil {
		return checkResult{"github api", statusFail, "GET /user failed: " + err.Error()}
	}
	var me map[string]any
	json.Unmarshal(b, &me)
	login, _ := me["login"].(string)
	lines = append(lines, "authenticated as "+login)

	rb, err := ghDo("GET", "/rate_limit", nil)
	if err != nil {
		return checkResult{"github api", statusWarn,
			strings.Join(lines, "\n      ") + "\n      rate limit lookup failed: " + err.Error()}
	}
	var rl struct {
		Resources struct {
			Core struct {
				Limit     int   `json:"limit"`
				Remaining int   `json:"remaining"`
				Reset     int64 `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	status := statusOK
	if err := json.Unmarshal(rb, &rl); err == nil {
		resetIn := time.Until(time.Unix(rl.Resources.Core.Reset, 0)).Round(time.Minute)
		lines = append(lines, fmt.Sprintf("rate limit: %d/%d remaining, resets in %s",
			rl.Resources.Core.Remaining, rl.Resources.Core.Limit, resetIn))
		if rl.Resources.Core.Remaining == 0 {
			status = statusFail
			lines = append(lines, "-> rate limit exhausted; GitHub calls will fail until reset")
		} else if float64(rl.Resources.Core.Remaining)/float64(rl.Resources.Core.Limit) < 0.1 {
			status = statusWarn
			lines = append(lines, "-> rate limit below 10%")
		}
	}
	if githubRepo != "" {
		ib, err := ghDo("GET", "/repos/"+githubRepo, nil)
		if err != nil {
			if status == statusOK {
				status = statusWarn
			}
			lines = append(lines, "repo "+githubRepo+": NOT ACCESSIBLE ("+firstLine(err.Error(), 120)+")")
		} else {
			var info map[string]any
			json.Unmarshal(ib, &info)
			def, _ := info["default_branch"].(string)
			arch, _ := info["archived"].(bool)
			priv, _ := info["private"].(bool)
			lines = append(lines, fmt.Sprintf("repo %s: ok (default branch %s, private=%v)", githubRepo, def, priv))
			if arch {
				status = statusWarn
				lines = append(lines, "-> repository is archived; pushes and PRs will fail")
			}
		}
	}
	return checkResult{"github api", status, strings.Join(lines, "\n      ")}
}

func checkDocker() checkResult {
	if _, err := exec.LookPath("docker"); err != nil {
		return checkResult{"docker", statusFail, "docker CLI not on PATH"}
	}
	var lines []string
	rc, out := runCmd(15*time.Second, "", nil, "docker", "version", "--format", "{{.Server.Version}}")
	if rc != 0 {
		return checkResult{"docker", statusFail,
			"daemon unreachable: " + strings.TrimSpace(firstLine(out, 200))}
	}
	lines = append(lines, "daemon: reachable, server version "+strings.TrimSpace(out))
	rc, out = runCmd(15*time.Second, "", nil, "docker", "image", "inspect", workerImage,
		"--format", "{{.Created}} {{.Size}}")
	if rc != 0 {
		return checkResult{"docker", statusFail,
			strings.Join(lines, "\n      ") + "\n      worker image \"" + workerImage + "\" MISSING — build it (install.sh does this)"}
	}
	parts := strings.Fields(strings.TrimSpace(out))
	created, size := "?", "?"
	if len(parts) >= 2 {
		if t, err := time.Parse(time.RFC3339Nano, parts[0]); err == nil {
			created = t.Format("2006-01-02")
		}
		if n, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			size = humanBytes(n)
		}
	}
	lines = append(lines, "worker image \""+workerImage+"\": present (created "+created+", "+size+")")
	return checkResult{"docker", statusOK, strings.Join(lines, "\n      ")}
}

// timerInfo holds the parsed state of the factory's systemd timer.
type timerInfo struct {
	active    string
	fileState string
	last      time.Time
	next      time.Time
}

// parseShowVal extracts the value of a Name= line from `systemctl show` output.
func showVal(out, name string) string {
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, name+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// parseSystemdTime parses a systemd timestamp: either raw microseconds
// since the epoch (older systemd) or the "Tue 2026-09-29 22:59:56 UTC"
// rendering newer systemd uses in `show` output. Empty or "n/a" stays
// the zero time.
func parseSystemdTime(v string) time.Time {
	if v == "" || v == "n/a" {
		return time.Time{}
	}
	if us, err := strconv.ParseInt(v, 10, 64); err == nil && us > 0 {
		return time.Unix(us/1e6, (us%1e6)*1e3)
	}
	if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", v); err == nil {
		return t
	}
	return time.Time{}
}

// parseListTimersNext extracts the NEXT timestamp from
// `systemctl list-timers <unit> --no-pager --no-legend` output, where it
// is always the first four tokens ("Tue 2026-09-29 23:01:57 UTC").
func parseListTimersNext(out string) time.Time {
	line := strings.TrimSpace(firstLine(out, 200))
	if line == "" || strings.HasPrefix(line, "n/a") {
		return time.Time{}
	}
	toks := strings.Fields(line)
	if len(toks) < 4 {
		return time.Time{}
	}
	t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", strings.Join(toks[:4], " "))
	if err != nil {
		return time.Time{}
	}
	return t
}

func checkTimer() checkResult {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return checkResult{"timer", statusInfo, "systemctl not available — timer checks skipped (non-systemd host)"}
	}
	var lines []string
	status := statusOK
	rc, out := runCmd(10*time.Second, "", nil, "systemctl", "show", "minifactory-go.timer")
	if rc != 0 {
		return checkResult{"timer", statusWarn,
			"minifactory-go.timer not found: " + strings.TrimSpace(firstLine(out, 160)) + "\n      -> install.sh installs and enables the timer"}
	}
	ti := timerInfo{
		active:    showVal(out, "ActiveState"),
		fileState: showVal(out, "UnitFileState"),
		last:      parseSystemdTime(showVal(out, "LastTriggerUSec")),
	}
	if rc, lout := runCmd(10*time.Second, "", nil, "systemctl", "list-timers",
		"minifactory-go.timer", "--no-pager", "--no-legend"); rc == 0 {
		ti.next = parseListTimersNext(lout)
	}
	lines = append(lines, "minifactory-go.timer: active="+ti.active+" enabled="+ti.fileState)
	if ti.active != "active" {
		status = statusFail
		lines = append(lines, "-> timer is not active: the factory will not pick up work")
	}
	if ti.fileState != "enabled" {
		status = statusFail
		lines = append(lines, "-> timer is not enabled: it will not survive a reboot")
	}
	now := time.Now()
	if ti.last.IsZero() {
		lines = append(lines, "last fired: never")
	} else {
		lines = append(lines, "last fired: "+shortDur(now.Sub(ti.last))+" ago ("+ti.last.Format("15:04:05")+")")
		if now.Sub(ti.last) > 10*time.Minute && ti.active == "active" {
			status = statusWarn
			lines = append(lines, "-> last fire was over 10m ago for a 1-minute timer — systemd may be backed up")
		}
	}
	if ti.next.IsZero() {
		lines = append(lines, "next fire: not scheduled")
	} else if ti.next.After(now) {
		lines = append(lines, "next fire: in "+shortDur(ti.next.Sub(now)))
	} else {
		lines = append(lines, "next fire: overdue by "+shortDur(now.Sub(ti.next)))
	}

	// recent service activity from the journal
	if _, err := exec.LookPath("journalctl"); err == nil {
		rc, out := runCmd(10*time.Second, "", nil, "journalctl", "-u", "minifactory-go.service",
			"--since", "15 min ago", "-q", "--no-pager", "-o", "cat", "-n", "6")
		if rc == 0 {
			if tail := strings.TrimSpace(out); tail != "" {
				var jl []string
				for _, l := range strings.Split(tail, "\n") {
					jl = append(jl, "journal: "+firstLine(l, 140))
				}
				lines = append(lines, jl...)
			} else {
				lines = append(lines, "journal: no service output in the last 15m")
			}
		}
	}
	return checkResult{"timer", status, strings.Join(lines, "\n      ")}
}

func checkDirs() checkResult {
	var lines []string
	status := statusOK
	for _, d := range []struct{ label, path string }{
		{"base", baseDir},
		{"data", dataDir},
		{"work", workDir},
		{"logs", logDir},
	} {
		if err := os.MkdirAll(d.path, 0755); err != nil {
			status = statusFail
			lines = append(lines, d.label+": cannot create "+d.path+": "+err.Error())
			continue
		}
		probe := filepath.Join(d.path, ".doctor-write-test")
		if err := os.WriteFile(probe, []byte("ok"), 0644); err != nil {
			status = statusFail
			lines = append(lines, d.label+": not writable ("+d.path+")")
			continue
		}
		os.Remove(probe)
		lines = append(lines, d.label+": "+d.path+" (writable)")
	}
	return checkResult{"directories", status, strings.Join(lines, "\n      ")}
}

func checkQueue() checkResult {
	// stat before listTasks: the store round-trip rewrites queue.json,
	// which would make "modified" meaningless.
	qst, qstErr := os.Stat(storePath())
	tasks, err := listTasks()
	if err != nil {
		return checkResult{"queue", statusFail, "cannot read queue: " + err.Error()}
	}
	r := analyzeTasks(tasks, time.Now())
	var lines []string
	status := statusOK
	if r.total == 0 {
		lines = append(lines, "empty — no tasks in data/queue.json")
	} else {
		var parts []string
		for _, s := range []string{"queued", "running", "pr_open", "failed", "done"} {
			if r.counts[s] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", r.counts[s], s))
			}
		}
		lines = append(lines, fmt.Sprintf("%d task(s): %s", r.total, strings.Join(parts, ", ")))
	}
	if len(r.staleRunning) > 0 {
		status = statusWarn
		lines = append(lines, "-> STALE running tasks (over 45m, likely orphaned): "+strings.Join(r.staleRunning, ", "))
		lines = append(lines, "   inspect with `logs <id>`; re-queue with `retry <id>` if the run is dead")
	}
	if len(r.failed) > 0 {
		if status == statusOK {
			status = statusWarn
		}
		lines = append(lines, "-> failed tasks: "+strings.Join(r.failed, ", "))
	}
	if len(r.prOpen) > 0 {
		lines = append(lines, "awaiting merge: "+strings.Join(r.prOpen, ", "))
	}
	if r.oldestQueued != "" {
		lines = append(lines, "oldest queued: "+r.oldestQueued)
	}
	if qstErr == nil {
		lines = append(lines, fmt.Sprintf("queue.json: %s, modified %s ago", humanBytes(qst.Size()), shortDur(time.Since(qst.ModTime()))))
	}
	return checkResult{"queue", status, strings.Join(lines, "\n      ")}
}

func checkDisk() checkResult {
	var st syscall.Statfs_t
	if err := syscall.Statfs(baseDir, &st); err != nil {
		return checkResult{"disk", statusWarn, "cannot stat filesystem for " + baseDir + ": " + err.Error()}
	}
	free := st.Bavail * uint64(st.Bsize)
	total := st.Blocks * uint64(st.Bsize)
	status := statusOK
	detail := fmt.Sprintf("%s free of %s on the factory filesystem", humanBytes(int64(free)), humanBytes(int64(total)))
	switch {
	case free < 500*1024*1024:
		status = statusFail
		detail += "\n      -> critically low: sandbox runs and git clones need headroom"
	case free < 2*1024*1024*1024:
		status = statusWarn
		detail += "\n      -> under 2 GB free; sandbox image pulls may fail"
	}
	return checkResult{"disk", status, detail}
}

func checkRecentActivity() checkResult {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return checkResult{"recent activity", statusInfo, "logs dir not readable: " + err.Error()}
	}
	var newest time.Time
	var newestName string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		if inf, err := e.Info(); err == nil && inf.ModTime().After(newest) {
			newest = inf.ModTime()
			newestName = e.Name()
		}
	}
	if newest.IsZero() {
		return checkResult{"recent activity", statusInfo, "no task logs yet — the factory has not run a task"}
	}
	return checkResult{"recent activity", statusOK,
		fmt.Sprintf("last task log activity: %s ago (%s)", shortDur(time.Since(newest)), newestName)}
}

// ---- command ----------------------------------------------------------

func cmdDoctor(args []string) int {
	checks := []checkResult{
		checkEnvFile(),
		checkTokens(),
		checkTools(),
		checkAgentCLI(),
		checkGitHub(),
		checkDocker(),
		checkTimer(),
		checkDirs(),
		checkQueue(),
		checkDisk(),
		checkRecentActivity(),
	}
	if hasFlag(args, "probe") {
		checks = append(checks, probeAgentAuth())
	}
	if hasFlag(args, "json") {
		summary := map[checkStatus]int{}
		for _, c := range checks {
			summary[c.Status]++
		}
		out := map[string]any{
			"version":      version,
			"generated_at": time.Now().UTC().Format(time.RFC3339),
			"checks":       checks,
			"summary": map[string]any{
				"ok":   summary[statusOK],
				"warn": summary[statusWarn],
				"fail": summary[statusFail],
				"info": summary[statusInfo],
			},
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return exitFor(checks)
	}
	fmt.Printf("minifactory %s — doctor %s\n", version, time.Now().Format("2006-01-02 15:04:05"))
	for _, c := range checks {
		fmt.Printf("\n[%s] %s\n", c.Status, c.Name)
		for _, line := range strings.Split(c.Detail, "\n") {
			fmt.Println("      " + line)
		}
	}
	nOk, nWarn, nFail := 0, 0, 0
	for _, c := range checks {
		switch c.Status {
		case statusOK:
			nOk++
		case statusWarn:
			nWarn++
		case statusFail:
			nFail++
		}
	}
	fmt.Printf("\ndoctor: %d ok, %d warn, %d fail\n", nOk, nWarn, nFail)
	return exitFor(checks)
}

func exitFor(checks []checkResult) int {
	for _, c := range checks {
		if c.Status == statusFail {
			return 1
		}
	}
	return 0
}
