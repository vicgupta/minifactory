package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cleanPythonCache removes Python bytecode artifacts (__pycache__/,
// *.pyc, *.pyo) from dir, skipping .git. Test runs and agents recreate
// these constantly; they must not leak into factory commits.
func cleanPythonCache(dir string) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "__pycache__" {
				if d.Name() == "__pycache__" {
					os.RemoveAll(p)
				}
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".pyc") || strings.HasSuffix(name, ".pyo") {
			os.Remove(p)
		}
		return nil
	})
}

// ensureGitignore adds bytecode ignore rules to dir/.gitignore when they
// are missing, so future test runs don't reintroduce the artifacts.
// ensureGitignore makes sure dir/.gitignore covers Python bytecode
// artifacts, creating or extending the file as needed. It reports whether
// it modified the file, so callers can exclude .gitignore from
// changed-file accounting (the agent didn't touch it).
func ensureGitignore(dir string) bool {
	rules := []string{"__pycache__/", "*.py[cod]", "*$py.class"}
	path := filepath.Join(dir, ".gitignore")
	existing, _ := os.ReadFile(path)
	var missing []string
	for _, r := range rules {
		found := false
		for _, line := range strings.Split(string(existing), "\n") {
			if strings.TrimSpace(line) == r {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		return false
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return false
	}
	defer f.Close()
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		f.WriteString("\n")
	}
	f.WriteString("\n# Python bytecode (added by minifactory)\n")
	for _, r := range missing {
		f.WriteString(r + "\n")
	}
	return true
}

// factoryMetaFiles are orchestrator-owned files in the workdir that never
// count as agent changes.
var factoryMetaFiles = map[string]bool{
	"task.json":   true,
	"result.json": true,
	"size.json":   true,
}

// cleanAgentPath normalizes a claimed file path for comparison: trims
// whitespace, drops a leading ./, and cleans the result.
func cleanAgentPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	return filepath.Clean(p)
}

// gitChangedFiles lists the files git sees changed in workdir: tracked
// modifications against HEAD plus untracked files (minus the factory
// metadata files, which the orchestrator owns). NUL-separated output keeps
// unusual file names intact.
func gitChangedFiles(workdir string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, args := range [][]string{
		{"diff", "--name-only", "-z", "HEAD"},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	} {
		rc, sout := runCmd(time.Minute, workdir, nil, "", "git", args...)
		if rc != 0 {
			return nil, fmt.Errorf("git %s failed", strings.Join(args, " "))
		}
		for _, f := range strings.Split(sout, "\x00") {
			f = strings.TrimSpace(f)
			if f == "" || factoryMetaFiles[filepath.Clean(f)] {
				continue
			}
			out[filepath.Clean(f)] = true
		}
	}
	return out, nil
}

// changedFilesDiff compares the agent's claimed changed_files against what
// git sees changed, returning the files the claim includes that git does not
// (onlyClaimed) and the git-changed files the claim omits (onlyActual). Both
// empty means the sets match. git is authoritative — the commit uses
// `git add -A` — so this is advisory: it surfaces a sloppy or dishonest
// handoff without failing the run.
func changedFilesDiff(workdir string, claimed any, gitignoreTouched bool) (onlyClaimed, onlyActual []string, err error) {
	actual, err := gitChangedFiles(workdir)
	if err != nil {
		return nil, nil, err
	}
	if gitignoreTouched {
		delete(actual, ".gitignore")
	}
	claimedSet := map[string]bool{}
	if list, ok := claimed.([]any); ok {
		for _, f := range list {
			if s, ok := f.(string); ok {
				claimedSet[cleanAgentPath(s)] = true
			}
		}
	}
	for f := range claimedSet {
		if !actual[f] {
			onlyClaimed = append(onlyClaimed, f)
		}
	}
	for f := range actual {
		if !claimedSet[f] {
			onlyActual = append(onlyActual, f)
		}
	}
	sort.Strings(onlyClaimed)
	sort.Strings(onlyActual)
	return onlyClaimed, onlyActual, nil
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func failTask(t *Task, rec *RunRecord, msg string) int {
	rec.Outcome = "failed"
	rec.FailReason = firstLine(msg, 200)
	rec.finish()
	logf(t.ID, "FAILED: "+msg)
	commentOnIssue(t, "minifactory: run failed — "+firstLine(msg, 200)+"\n"+rec.summaryLine())
	setState(t.ID, "failed", "", "")
	return 1
}

func cmdIssue(args []string) int {
	repo := flagVal(args, "repo")
	title := flagVal(args, "title")
	body := flagVal(args, "body")
	explicitIssue := false
	issueNum := 0
	if s := flagVal(args, "issue"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			fmt.Fprintln(os.Stderr, "issue: --issue must be a positive issue number")
			return 2
		}
		issueNum = n
		explicitIssue = true
	}
	if repo == "" || title == "" {
		fmt.Fprintln(os.Stderr, "issue requires --repo and --title")
		return 2
	}
	repoURL := normalizeRepoURL(repo)
	tracef("issue: repo=%s title=%q", repoURL, title)
	// Every `issue` invocation gets a tracking issue on the GitHub repo, which the
	// factory then labels factory-ready and works through the label
	// lifecycle. An explicitly linked issue (--issue) is reused instead.
	// Issue creation is best-effort: a failure warns but doesn't fail the
	// issue command — the task itself is queued either way.
	if !explicitIssue {
		if githubToken == "" {
			fmt.Fprintln(os.Stderr, "issue: warning: no GITHUB_TOKEN, no GitHub issue created")
		} else if org, name, ok := parseGithubRepo(repoURL); !ok {
			fmt.Fprintf(os.Stderr, "issue: warning: %q is not a GitHub repo, no issue created\n", repo)
		} else if n, err := func() (int, error) {
			tracef("issue: creating GitHub issue")
			return createIssue(org, name, title, body)
		}(); err != nil {
			fmt.Fprintf(os.Stderr, "issue: warning: issue creation failed: %v\n", err)
		} else {
			issueNum = n
			fmt.Fprintf(os.Stderr, "issue: created issue #%d\n", n)
		}
	}
	taskTitle := title
	if issueNum != 0 {
		taskTitle = fmt.Sprintf("[#%d] %s", issueNum, title)
	}
	id := newID()
	err := withStore(func(tasks []Task) ([]Task, error) {
		return append(tasks, Task{
			ID:          id,
			RepoURL:     repoURL,
			Title:       taskTitle,
			Body:        body,
			State:       "queued",
			CreatedAt:   float64(time.Now().UnixNano()) / 1e9,
			IssueNumber: issueNum,
		}), nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Link the issue into the label lifecycle: creating it for factory work
	// marks it factory-ready. stdout carries only the id (parseable), so
	// lifecycle notes go to stderr.
	if issueNum != 0 {
		if githubToken == "" {
			fmt.Fprintf(os.Stderr, "issue: warning: no GITHUB_TOKEN, issue #%d not labeled %s\n", issueNum, labelReady)
		} else if err := addIssueLabel(repoURL, issueNum, labelReady); err != nil {
			fmt.Fprintf(os.Stderr, "issue: warning: issue #%d not labeled %s: %v\n", issueNum, labelReady, err)
		} else {
			fmt.Fprintf(os.Stderr, "issue: issue #%d labeled %s\n", issueNum, labelReady)
		}
	}
	fmt.Println(id) // parseable: stdout carries only the id
	return 0
}

func cmdRunOnce() int {
	tasks, err := listTasks()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tracef("run-once: %d task(s) in queue", len(tasks))
	t := pickNextTask(tasks)
	if t == nil {
		fmt.Println("nothing queued")
		return 0
	}
	tid := t.ID
	tracef("run-once: picked %s (%s)", tid, t.Title)
	setState(tid, "running", "", "")
	// Liveness lock: lets `retry` tell a live run from a crashed one. The
	// deferred release runs on every exit path; a crash (or SIGKILL/reboot)
	// leaves a stale lock whose PID is dead, which retry treats as reclaimable.
	claimRun(tid)
	defer releaseRun(tid)
	// The run record is finalized and written on every exit path — success,
	// failure, and split alike.
	rec := newRunRecord(t)
	defer func() {
		rec.finish()
		if err := rec.write(); err != nil {
			logf(tid, "WARNING: run record write failed: %v", err)
		}
	}()
	logf(tid, "picked up: %s <%s>", t.Title, t.RepoURL)
	commentOnIssue(t, "minifactory: picked this up \u2014 working in branch `factory/"+tid+"`.")
	labelIssueInProgress(t)

	workdir := filepath.Join(workDir, tid)
	os.RemoveAll(workdir)

	if err := runCloneStage(t, rec, workdir); err != nil {
		return failTask(t, rec, err.Error())
	}

	tj, _ := json.Marshal(map[string]string{"title": t.Title, "body": t.Body})
	os.WriteFile(filepath.Join(workdir, "task.json"), tj, 0644)

	sv, handled, err := runSizeStage(t, rec, workdir)
	if err != nil {
		return failTask(t, rec, "sizer: "+err.Error())
	}
	if handled {
		return 0
	}
	if sv != nil {
		logf(tid, "sizer: split not possible — running main agent")
	}

	ar := runAgentStage(t, rec, workdir)

	res, err := runValidateStage(t, rec, workdir)
	if err != nil {
		return failTask(t, rec, err.Error())
	}

	if ar.name != "stub" {
		if !runSandboxTestStage(t, rec) {
			return failTask(t, rec, "sandbox test run failed — no PR opened")
		}
	}

	// Factory metadata stays out of the commit.
	for _, meta := range []string{"task.json", "result.json", "size.json"} {
		os.Remove(filepath.Join(workdir, meta))
	}

	branch := "factory/" + tid
	if err := runPushStage(t, rec, workdir, branch); err != nil {
		return failTask(t, rec, err.Error())
	}

	labelIssueCompleted(t)

	tracef("run-once: creating PR from %s", branch)
	rec.beginStage("pr")
	prURL := createPR(t, branch, rec)
	rec.endStage(true)
	rec.Outcome = "pr_open"
	rec.PRURL = prURL
	if cf, ok := res["changed_files"].([]any); ok {
		rec.ChangedFiles = len(cf)
	}
	if s, ok := res["summary"].(string); ok {
		rec.Summary = firstLine(s, 200)
	}
	if prURL != "" {
		commentOnIssue(t, "minifactory: done \u2014 "+prURL+"\nTests passed in an isolated sandbox. Merge the PR to complete.\n"+rec.summaryLine())
		labelIssueCreatedPR(t)
		// The issue's tracking job is done: close it. The PR now carries
		// the work to the human gate (merge/reject in GitHub).
		setTaskIssueState(t, "closed")
	}
	setState(tid, "pr_open", branch, prURL)
	logf(tid, "state -> pr_open (human gate: merge the PR in GitHub)")
	return 0
}

// pickNextTask returns the oldest queued task, or nil when none is queued.
func pickNextTask(tasks []Task) *Task {
	var t *Task
	for i := range tasks {
		if tasks[i].State == "queued" && (t == nil || tasks[i].CreatedAt < t.CreatedAt) {
			t = &tasks[i]
		}
	}
	return t
}

// runCloneStage clones the repo into workdir and times the clone stage.
func runCloneStage(t *Task, rec *RunRecord, workdir string) error {
	tid := t.ID
	tracef("run-once: cloning %s", t.RepoURL)
	rec.beginStage("clone")
	if rc, out := runCmd(3*time.Minute, "", nil, "", "git", gitCloneArgs(t.RepoURL, workdir)...); rc != 0 {
		return fmt.Errorf("clone failed\n%s", tailStr(out, 2000))
	}
	rec.endStage(true)
	logf(tid, "cloned ok")
	return nil
}

// runSizeStage runs the sizing pass. It returns the verdict, whether the
// split was handled (task is complete), and any error.
func runSizeStage(t *Task, rec *RunRecord, workdir string) (*sizeVerdict, bool, error) {
	tid := t.ID
	tracef("run-once: sizing pass (fit vs split)")
	sv, sizerUsage, serr := runSizer(t, workdir, rec)
	if serr != nil {
		return nil, false, serr
	}
	rec.setUsage(sizerUsage, nil)
	checkOverBudget(rec, sizerUsage, "sizer")
	if sv != nil {
		if _, done := splitTask(t, rec, sv); done {
			return nil, true, nil
		}
		logf(tid, "sizer: split not possible — running main agent")
	}
	return sv, false, nil
}

// runAgentStage runs the selected agent and records its usage and sandboxing.
func runAgentStage(t *Task, rec *RunRecord, workdir string) agentRun {
	tracef("run-once: starting agent")
	ar := runAgent(t, rec)
	tracef("run-once: agent finished")
	rec.setUsage(nil, ar.usage)
	rec.Sandboxed = ar.sandboxed
	checkOverBudget(rec, ar.usage, "agent")
	return ar
}

// runValidateStage validates result.json and cross-checks changed_files
// against git status. It returns the parsed result and any error.
func runValidateStage(t *Task, rec *RunRecord, workdir string) (map[string]any, error) {
	tid := t.ID
	rec.beginStage("validate")
	cleanPythonCache(workdir)
	gitignoreTouched := ensureGitignore(workdir)
	logf(tid, "pre-commit hygiene: cleaned bytecode caches, ensured .gitignore")

	tracef("run-once: validating result.json")
	var res map[string]any
	valid := false
	if b, err := os.ReadFile(filepath.Join(workdir, "result.json")); err == nil {
		if json.Unmarshal(b, &res) == nil {
			_, cfOK := res["changed_files"].([]any)
			tp, tpOK := res["tests_passed"].(bool)
			_, sOK := res["summary"].(string)
			valid = cfOK && tpOK && sOK && tp
		}
	} else {
		logf(tid, "result.json missing/invalid: %v", err)
	}
	if !valid {
		return nil, fmt.Errorf("validation failed — no PR opened")
	}
	if s, ok := res["summary"].(string); ok {
		logf(tid, "validation passed: %s", firstLine(s, 160))
	}

	// Cross-check the agent's claimed changed_files against git status.
	// git is authoritative (the commit uses `git add -A`), so a mismatch is
	// a logged warning, not a failure: the sandbox test re-run and the human
	// PR review are the real trust boundaries.
	onlyClaimed, onlyActual, err := changedFilesDiff(workdir, res["changed_files"], gitignoreTouched)
	switch {
	case err != nil:
		logf(tid, "WARNING: cannot list changed files: %v", err)
	case len(onlyClaimed) == 0 && len(onlyActual) == 0:
		logf(tid, "changed_files verified against git status")
	default:
		logf(tid, "WARNING: changed_files differs from git status — claimed-only=%v git-only=%v (committing git's version)", onlyClaimed, onlyActual)
	}
	rec.endStage(true)
	return res, nil
}

// runSandboxTestStage re-runs the repo test suite in a sandbox for
// non-stub agents (the trust boundary on the agent's tests_passed claim).
func runSandboxTestStage(t *Task, rec *RunRecord) bool {
	tracef("run-once: running tests in sandbox")
	rec.beginStage("sandbox_tests")
	testsOK := runTestsInSandbox(t)
	rec.endStage(testsOK)
	return testsOK
}

// runPushStage commits the agent's changes and pushes them as a branch.
func runPushStage(t *Task, rec *RunRecord, workdir, branch string) error {
	tid := t.ID
	tracef("run-once: committing and pushing branch %s", branch)
	rec.beginStage("push")
	if rc, out := runCmd(time.Minute, workdir, nil, "", "git", "checkout", "-b", branch); rc != 0 {
		return fmt.Errorf("checkout -b failed\n%s", tailStr(out, 1000))
	}
	if _, out := runCmd(time.Minute, workdir, nil, "", "git", "status", "--porcelain"); strings.TrimSpace(out) == "" {
		return fmt.Errorf("agent made no changes")
	}
	runCmd(time.Minute, workdir, nil, "", "git", "add", "-A")
	if rc, out := runCmd(time.Minute, workdir, nil, "", "git",
		"-c", "user.name=minifactory", "-c", "user.email=minifactory@localhost",
		"commit", "-m", "factory: "+firstLine(t.Title, 72)); rc != 0 {
		return fmt.Errorf("commit failed\n%s", tailStr(out, 1000))
	}
	if rc, out := gitPush(workdir, branch, t.RepoURL); rc != 0 {
		return fmt.Errorf("push failed\n%s", tailStr(out, 2000))
	}
	rec.endStage(true)
	logf(tid, "pushed branch "+branch)
	return nil
}

// splitTask decomposes an oversized task into GitHub sub-issues and marks
// the parent split. Each child is labeled factory-ready so the next poll
// picks it up as an independent factory task. Returns (rc, true) when the
// task was handled, (0, false) when the split wasn't possible and the
// caller should fall through to the main agent.
func splitTask(t *Task, rec *RunRecord, sv *sizeVerdict) (int, bool) {
	tid := t.ID
	org, repo, ok := parseGithubRepo(t.RepoURL)
	if t.IssueNumber == 0 || githubToken == "" || !ok {
		logf(tid, "sizer: split advised (%s) but no linked issue/token/GitHub repo — cannot create sub-issues", sv.Reason)
		return 0, false
	}
	var children []int
	for i := range sv.Subtasks {
		st := &sv.Subtasks[i]
		body := fmt.Sprintf("Sub-task %d/%d of #%d (%s).\n\nParent scope: %s\n\n%s",
			i+1, len(sv.Subtasks), t.IssueNumber, firstLine(t.Title, 80), sv.Reason, st.Body)
		n, err := createIssue(org, repo, st.Title, body)
		if err != nil {
			logf(tid, "sizer: WARNING: sub-issue %d/%d creation failed: %v", i+1, len(sv.Subtasks), err)
			continue
		}
		if err := addIssueLabel(t.RepoURL, n, labelReady); err != nil {
			logf(tid, "sizer: WARNING: sub-issue #%d labeling failed: %v", n, err)
		}
		children = append(children, n)
		logf(tid, "sizer: created sub-issue #%d: %s", n, st.Title)
	}
	if len(children) == 0 {
		logf(tid, "sizer: no sub-issues created — cannot split")
		return 0, false
	}
	_ = withStore(func(tasks []Task) ([]Task, error) {
		for i := range tasks {
			if tasks[i].ID == tid {
				tasks[i].Children = children
			}
		}
		return tasks, nil
	})
	nums := make([]string, len(children))
	for i, n := range children {
		nums[i] = "#" + strconv.Itoa(n)
	}
	commentOnIssue(t, fmt.Sprintf("minifactory: too large for a single run (%s) — split into %d sub-tasks: %s.\nEach will be picked up as a separate factory task.", sv.Reason, len(children), strings.Join(nums, ", ")))
	rec.Outcome = "split"
	setState(tid, "split", "", "")
	logf(tid, "state -> split (%d sub-issues: %s)", len(children), strings.Join(nums, ", "))
	return 0, true
}

// runLockPath is the liveness lock for a task being executed by run-once.
// It lives next to (not inside) the task workdir so the workdir wipe at
// pickup doesn't remove it.
func runLockPath(tid string) string { return filepath.Join(workDir, tid+".lock") }

// claimRun records this process as the live executor of tid.
func claimRun(tid string) {
	os.MkdirAll(workDir, 0755)
	os.WriteFile(runLockPath(tid), []byte(strconv.Itoa(os.Getpid())), 0644)
}

// releaseRun drops the liveness lock for tid.
func releaseRun(tid string) { os.Remove(runLockPath(tid)) }

// runIsAlive reports whether the process that claimed tid still looks like a
// live minifactory run. A missing/unparseable lock, a dead PID, or a PID that
// no longer belongs to a minifactory process all read as "not alive" —
// exactly what `retry` needs to reclaim a crashed task.
func runIsAlive(tid string) bool {
	b, err := os.ReadFile(runLockPath(tid))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false // no such process (or no permission to signal it)
	}
	// Guard against PID reuse: the live holder must be a minifactory process.
	if cb, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		return strings.Contains(string(cb), "minifactory")
	}
	return true // no /proc (non-Linux): trust kill -0
}

// cmdRetry reclaims a stuck or failed task:
//
//	failed              → re-queue from scratch (issue label reset to factory-ready)
//	running             → re-queue if the run is dead; a live run is refused
//	                      unless --force is given
//	pr_open, no pr_url  → the branch is pushed but PR creation failed: retry
//	                      only the PR creation, no agent re-run
//	pr_open with URL    → refuse (merge or close the PR first)
//	split               → refuse (retry a sub-task instead)
//	queued / done       → re-queue (explicit user choice)
func cmdRetry(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "retry requires an id")
		return 2
	}
	id := args[0]
	force := hasFlag(args, "force")

	tasks, err := listTasks()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tracef("retry: %d task(s) in queue, looking for %s", len(tasks), id)
	var t *Task
	for i := range tasks {
		if tasks[i].ID == id {
			t = &tasks[i]
			break
		}
	}
	if t == nil {
		fmt.Fprintln(os.Stderr, "no such task: "+id)
		return 1
	}

	tracef("retry: task %s state=%s branch=%s pr_url=%s", id, t.State, t.Branch, t.PRURL)
	switch t.State {
	case "split":
		fmt.Fprintf(os.Stderr, "task %s is split into sub-issues %v; retry a sub-task instead\n", id, t.Children)
		return 1
	case "pr_open":
		if t.PRURL != "" {
			fmt.Fprintf(os.Stderr, "task %s has an open PR (%s); merge or close it first\n", id, t.PRURL)
			return 1
		}
		if t.Branch == "" {
			break // nothing salvageable — fall through to a full re-queue
		}
		tracef("retry: retrying PR creation only for %s (branch %s)", t.ID, t.Branch)
		return retryPRCreate(t)
	case "running":
		if runIsAlive(id) && !force {
			fmt.Fprintf(os.Stderr, "task %s looks actively running; pass --force to re-queue anyway\n", id)
			return 1
		}
		releaseRun(id) // drop the stale lock, if any
	}

	tracef("retry: re-queueing %s", id)
	requeueTask(t)
	fmt.Println("re-queued " + id)
	return 0
}

// requeueTask resets a task to queued, clears its branch/PR bookkeeping,
// and returns its issue to the start of the label lifecycle.
func requeueTask(t *Task) {
	withStore(func(tasks []Task) ([]Task, error) {
		for i := range tasks {
			if tasks[i].ID == t.ID {
				tasks[i].State = "queued"
				tasks[i].Branch = ""
				tasks[i].PRURL = ""
			}
		}
		return tasks, nil
	})
	labelIssueReady(t)
	// Reopen the issue if it was closed when a previous PR opened.
	setTaskIssueState(t, "open")
	commentOnIssue(t, "minifactory: re-queued for another attempt.")
}

// retryPRCreate re-attempts PR creation for a task whose branch was pushed
// but whose PR create failed (pr_open with no URL). No agent re-run.
func retryPRCreate(t *Task) int {
	prURL := createPR(t, t.Branch, nil)
	if prURL == "" {
		fmt.Fprintf(os.Stderr, "retry: PR creation failed again for %s; task left pr_open (see logs/%s.log)\n", t.ID, t.ID)
		return 1
	}
	setState(t.ID, "pr_open", t.Branch, prURL)
	labelIssueCreatedPR(t)
	commentOnIssue(t, "minifactory: done — "+prURL+"\nTests passed in an isolated sandbox. Merge the PR to complete.")
	setTaskIssueState(t, "closed")
	fmt.Println("PR opened: " + prURL)
	return 0
}

func cmdSync() int {
	tasks, err := listTasks()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	n := 0
	opens := 0
	for i := range tasks {
		if tasks[i].State == "pr_open" || tasks[i].State == "split" {
			opens++
		}
	}
	tracef("sync: %d task(s) in queue, %d awaiting PR/merge check", len(tasks), opens)
	for i := range tasks {
		t := &tasks[i]
		// Split parents: when every sub-issue is closed, the parent's work
		// is complete. The parent never had a PR, so it moves straight
		// from factory-inProgress to completed.
		if t.State == "split" {
			n++
			if t.IssueNumber == 0 || len(t.Children) == 0 || githubToken == "" {
				continue
			}
			org, repo, ok := parseGithubRepo(t.RepoURL)
			if !ok {
				logf(t.ID, "sync: cannot parse repo from %s", t.RepoURL)
				continue
			}
			allClosed := true
			for _, c := range t.Children {
				b, err := ghDo("GET", "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(c), nil)
				if err != nil {
					logf(t.ID, "sync: sub-issue #%d check failed: %v", c, err)
					allClosed = false
					break
				}
				var is map[string]any
				json.Unmarshal(b, &is)
				if s, _ := is["state"].(string); s != "closed" {
					allClosed = false
					break
				}
			}
			if allClosed {
				setState(t.ID, "done", "", "")
				color, desc := labelMeta(labelDone)
				swapIssueLabel(t, labelInProgress, labelDone, color, desc)
				commentOnIssue(t, "minifactory: all sub-tasks are complete.")
				logf(t.ID, "sync: all sub-issues closed -> done")
			} else {
				logf(t.ID, "sync: sub-issues still open")
			}
			continue
		}
		if t.State != "pr_open" {
			continue
		}
		n++
		tracef("sync: checking PR for %s (%s)", t.ID, t.PRURL)
		if t.PRURL == "" {
			logf(t.ID, "sync: no pr_url recorded; skipping (merge the branch manually)")
			continue
		}
		org, repo, ok := parseGithubRepo(t.RepoURL)
		if !ok {
			logf(t.ID, "sync: cannot parse repo from %s", t.RepoURL)
			continue
		}
		num := strings.TrimSuffix(t.PRURL, "/")
		num = num[strings.LastIndex(num, "/")+1:]
		b, err := ghDo("GET", "/repos/"+org+"/"+repo+"/pulls/"+num, nil)
		if err != nil {
			logf(t.ID, "sync: GitHub API failed: %v", err)
			continue
		}
		var pr map[string]any
		json.Unmarshal(b, &pr)
		if m, _ := pr["merged"].(bool); m {
			setState(t.ID, "done", "", "")
			labelIssueMerged(t)
			logf(t.ID, "sync: PR merged -> done")
		} else if s, _ := pr["state"].(string); s == "closed" {
			setState(t.ID, "failed", "", "")
			labelIssueRejected(t)
			logf(t.ID, "sync: PR closed unmerged -> failed")
		} else {
			logf(t.ID, "sync: PR still open")
		}
	}
	if n == 0 {
		fmt.Println("no open PRs to sync")
	}
	return 0
}

// taskListEntry is a Task plus its latest run-record stats, for list --json.
type taskListEntry struct {
	Task
	LastDurationS *float64 `json:"last_duration_s,omitempty"`
	LastCostUSD   *float64 `json:"last_cost_usd,omitempty"`
}

var validStates = []string{"queued", "running", "failed", "pr_open", "split", "done"}

// parseStateFilter parses a comma-separated --state value into a set.
func parseStateFilter(v string) (map[string]bool, error) {
	set := map[string]bool{}
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		if !slices.Contains(validStates, s) {
			return nil, fmt.Errorf("unknown state %q (valid: %s)", s, strings.Join(validStates, ", "))
		}
		set[s] = true
	}
	return set, nil
}

func cmdList(args []string) int {
	var states map[string]bool
	if hasStateFlag(args) {
		var err error
		if states, err = parseStateFilter(flagVal(args, "state")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	tasks, err := listTasks()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if states != nil {
		kept := tasks[:0]
		for _, t := range tasks {
			if states[t.State] {
				kept = append(kept, t)
			}
		}
		tasks = kept
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt < tasks[j].CreatedAt })
	if hasFlag(args, "json") {
		entries := make([]taskListEntry, 0, len(tasks))
		for _, t := range tasks {
			e := taskListEntry{Task: t}
			if recs, err := loadTaskRecords(t.ID); err == nil && len(recs) > 0 {
				last := recs[len(recs)-1]
				if last.FinishedAt != 0 {
					e.LastDurationS = floatPtr(last.DurationS)
				}
				if last.Usage != nil && last.Usage.Total != nil && last.Usage.Total.CostUSD != nil {
					e.LastCostUSD = last.Usage.Total.CostUSD
				}
			}
			entries = append(entries, e)
		}
		b, _ := json.MarshalIndent(entries, "", " ")
		fmt.Println(string(b))
		return 0
	}
	for _, t := range tasks {
		fmt.Printf("%s  %-8s  %.60s  %s\n", t.ID, t.State, t.Title, t.PRURL)
	}
	return 0
}

func cmdLogs(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "logs requires an id")
		return 2
	}
	b, err := os.ReadFile(filepath.Join(logDir, args[0]+".log"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "no logs for "+args[0])
		return 1
	}
	fmt.Print(string(b))
	return 0
}

// cmdPoll enqueues open issues labeled `ready` from GITHUB_REPO.
// Exits quietly when the token/repo isn't configured.
func cmdPoll() int {
	missing := []string{}
	if githubToken == "" {
		missing = append(missing, "GITHUB_TOKEN")
	}
	if githubRepo == "" {
		missing = append(missing, "GITHUB_REPO")
	}
	if len(missing) > 0 {
		fmt.Printf("poll: skipping — not set in .env: %s\n", strings.Join(missing, ", "))
		return 0
	}
	tracef("poll: checking %s for open %q issues", githubRepo, labelReady)
	b, err := ghDo("GET", "/repos/"+githubRepo+"/issues?state=open&labels="+labelReady+"&per_page=50", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "poll:", err)
		return 1
	}
	var issues []map[string]any
	if err := json.Unmarshal(b, &issues); err != nil {
		fmt.Fprintln(os.Stderr, "poll:", err)
		return 1
	}
	tracef("poll: GitHub returned %d open issue(s)", len(issues))
	added := 0
	withStore(func(tasks []Task) ([]Task, error) {
		haveNum := map[int]bool{}
		haveTitle := map[string]bool{}
		for _, t := range tasks {
			if t.IssueNumber != 0 {
				haveNum[t.IssueNumber] = true
			}
			haveTitle[t.Title] = true
		}
		for _, is := range issues {
			if _, isPR := is["pull_request"]; isPR {
				continue
			}
			num, _ := is["number"].(float64)
			title, _ := is["title"].(string)
			key := fmt.Sprintf("[#%d] %s", int(num), title)
			if haveNum[int(num)] || haveTitle[key] {
				continue
			}
			body, _ := is["body"].(string)
			tracef("poll: queueing [#%d] %s", int(num), title)
			tasks = append(tasks, Task{
				ID:          newID(),
				RepoURL:     "https://github.com/" + githubRepo + ".git",
				Title:       key,
				Body:        body,
				State:       "queued",
				CreatedAt:   float64(time.Now().UnixNano()) / 1e9,
				IssueNumber: int(num),
			})
			haveNum[int(num)] = true
			haveTitle[key] = true
			added++
		}
		return tasks, nil
	})
	fmt.Printf("poll: enqueued %d new task(s)\n", added)
	return 0
}

// hasStateFlag reports whether --state or --state=... appears in args.
func hasStateFlag(args []string) bool {
	for _, a := range args {
		if a == "--state" || strings.HasPrefix(a, "--state=") {
			return true
		}
	}
	return false
}

// cmdAuto runs a full factory cycle: poll for new issues, then drain the
// queue — run-once for every queued task, syncing after each run to pick
// up merged/closed PRs. A final sync catches PRs already open when the
// cycle began. This is the cron/timer entry point: it supersedes the bare
// `run-once` the cron block used to run, which skipped poll and sync.
func cmdAuto() int {
	cmdPoll()

	for {
		tasks, err := listTasks()
		if err != nil {
			fmt.Fprintln(os.Stderr, "auto:", err)
			return 1
		}
		if pickNextTask(tasks) == nil {
			break
		}
		cmdRunOnce()
		cmdSync()
	}
	cmdSync()
	return 0
}
