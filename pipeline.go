package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
func ensureGitignore(dir string) {
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
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		f.WriteString("\n")
	}
	f.WriteString("\n# Python bytecode (added by minifactory)\n")
	for _, r := range missing {
		f.WriteString(r + "\n")
	}
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func failTask(t *Task, msg string) int {
	logf(t.ID, "FAILED: "+msg)
	commentOnIssue(t, "minifactory: run failed \u2014 "+firstLine(msg, 200))
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
		} else if n, err := createIssue(org, name, title, body); err != nil {
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
	var t *Task
	for i := range tasks {
		if tasks[i].State == "queued" && (t == nil || tasks[i].CreatedAt < t.CreatedAt) {
			t = &tasks[i]
		}
	}
	if t == nil {
		fmt.Println("nothing queued")
		return 0
	}
	tid := t.ID
	setState(tid, "running", "", "")
	logf(tid, "picked up: %s <%s>", t.Title, t.RepoURL)
	commentOnIssue(t, "minifactory: picked this up \u2014 working in branch `factory/"+tid+"`.")
	labelIssueInProgress(t)

	workdir := filepath.Join(workDir, tid)
	os.RemoveAll(workdir)
	if rc, out := runCmd(3*time.Minute, "", nil, "git", gitCloneArgs(t.RepoURL, workdir)...); rc != 0 {
		return failTask(t, "clone failed\n"+tailStr(out, 2000))
	}
	logf(tid, "cloned ok")

	tj, _ := json.Marshal(map[string]string{"title": t.Title, "body": t.Body})
	os.WriteFile(filepath.Join(workdir, "task.json"), tj, 0644)

	// Sizing pass: if the task is too large for one agent run, split it
	// into sub-issues instead of burning the turn budget. splitTask
	// reports whether it handled the task; otherwise fall through to the
	// main agent as before.
	if sv := runSizer(t, workdir); sv != nil {
		if rc, done := splitTask(t, sv); done {
			return rc
		}
		logf(tid, "sizer: split not possible — running main agent")
	}

	agent := runAgent(t) // logs everything; hard failures surface via result.json

	// Validate the typed handoff: result.json {changed_files[], tests_passed, summary}.
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
		return failTask(t, "validation failed — no PR opened")
	}
	if s, ok := res["summary"].(string); ok {
		logf(tid, "validation passed: %s", firstLine(s, 160))
	}

	// Trust boundary for real agents: generated code executes only in a
	// disposable sandbox, never on the host.
	if agent != "stub" && !runTestsInSandbox(t) {
		return failTask(t, "sandbox test run failed — no PR opened")
	}

	// Factory metadata stays out of the commit.
	for _, meta := range []string{"task.json", "result.json"} {
		os.Remove(filepath.Join(workdir, meta))
	}

	// Pre-commit hygiene: test runs and agents leave Python bytecode
	// behind (__pycache__/, *.pyc). Remove it and keep it ignored so it
	// never enters a factory commit.
	cleanPythonCache(workdir)
	ensureGitignore(workdir)
	logf(tid, "pre-commit hygiene: cleaned bytecode caches, ensured .gitignore")

	branch := "factory/" + tid
	if rc, out := runCmd(time.Minute, workdir, nil, "git", "checkout", "-b", branch); rc != 0 {
		return failTask(t, "checkout -b failed\n"+tailStr(out, 1000))
	}
	if _, out := runCmd(time.Minute, workdir, nil, "git", "status", "--porcelain"); strings.TrimSpace(out) == "" {
		return failTask(t, "agent made no changes")
	}
	runCmd(time.Minute, workdir, nil, "git", "add", "-A")
	if rc, out := runCmd(time.Minute, workdir, nil, "git",
		"-c", "user.name=minifactory", "-c", "user.email=minifactory@localhost",
		"commit", "-m", "factory: "+firstLine(t.Title, 72)); rc != 0 {
		return failTask(t, "commit failed\n"+tailStr(out, 1000))
	}
	if rc, out := gitPush(workdir, branch, t.RepoURL); rc != 0 {
		return failTask(t, "push failed\n"+tailStr(out, 2000))
	}
	logf(tid, "pushed branch "+branch)

	// The factory's work is complete: tests passed in the sandbox and the
	// branch is pushed.
	labelIssueCompleted(t)

	prURL := createPR(t, branch)
	if prURL != "" {
		commentOnIssue(t, "minifactory: done \u2014 "+prURL+"\nTests passed in an isolated sandbox. Merge the PR to complete.")
		// PR is open and awaiting human review/merge.
		labelIssueCreatedPR(t)
	}
	setState(tid, "pr_open", branch, prURL)
	logf(tid, "state -> pr_open (human gate: merge the PR in GitHub)")
	return 0
}

// splitTask decomposes an oversized task into GitHub sub-issues and marks
// the parent split. Each child is labeled factory-ready so the next poll
// picks it up as an independent factory task. Returns (rc, true) when the
// task was handled, (0, false) when the split wasn't possible and the
// caller should fall through to the main agent.
func splitTask(t *Task, sv *sizeVerdict) (int, bool) {
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
	setState(tid, "split", "", "")
	logf(tid, "state -> split (%d sub-issues: %s)", len(children), strings.Join(nums, ", "))
	return 0, true
}

// cmdRetry re-queues a failed task. Refuses tasks that are running or
// awaiting merge, since those have a live branch/PR.
func cmdRetry(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "retry requires an id")
		return 2
	}
	id := args[0]
	found := false
	err := withStore(func(tasks []Task) ([]Task, error) {
		for i := range tasks {
			if tasks[i].ID == id {
				if tasks[i].State == "running" || tasks[i].State == "pr_open" {
					return tasks, fmt.Errorf("task %s is %s; not retrying", id, tasks[i].State)
				}
				if tasks[i].State == "split" {
					return tasks, fmt.Errorf("task %s is split into sub-issues %v; retry a sub-task instead", id, tasks[i].Children)
				}
				tasks[i].State = "queued"
				tasks[i].Branch = ""
				tasks[i].PRURL = ""
				found = true
			}
		}
		return tasks, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !found {
		fmt.Fprintln(os.Stderr, "no such task: "+id)
		return 1
	}
	fmt.Println("re-queued " + id)
	return 0
}

func cmdSync() int {
	tasks, err := listTasks()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	n := 0
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

func cmdList(args []string) int {
	tasks, err := listTasks()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt < tasks[j].CreatedAt })
	if hasFlag(args, "json") {
		b, _ := json.MarshalIndent(tasks, "", " ")
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
