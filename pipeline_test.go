package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// testDirs redirects the factory's data/work/log dirs into a temp dir so
// tests never touch the real queue. Returns a restore func.
func testDirs(t *testing.T) func() {
	t.Helper()
	root := t.TempDir()
	oldData, oldWork, oldLog := dataDir, workDir, logDir
	dataDir = filepath.Join(root, "data")
	workDir = filepath.Join(root, "work")
	logDir = filepath.Join(root, "logs")
	return func() {
		dataDir, workDir, logDir = oldData, oldWork, oldLog
	}
}

func seedTask(t *testing.T, task Task) {
	t.Helper()
	if err := withStore(func(tasks []Task) ([]Task, error) {
		return append(tasks, task), nil
	}); err != nil {
		t.Fatal(err)
	}
}

func taskState(t *testing.T, id string) string {
	t.Helper()
	tasks, err := listTasks()
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == id {
			return task.State
		}
	}
	t.Fatalf("task %s not found", id)
	return ""
}

func TestRunLockLifecycle(t *testing.T) {
	restore := testDirs(t)
	defer restore()

	if runIsAlive("nope") {
		t.Fatal("no lock file: expected not alive")
	}
	claimRun("abc123")
	if !runIsAlive("abc123") {
		t.Fatal("just claimed by this process: expected alive")
	}
	releaseRun("abc123")
	if runIsAlive("abc123") {
		t.Fatal("lock released: expected not alive")
	}
}

func TestRunIsAliveDeadPID(t *testing.T) {
	restore := testDirs(t)
	defer restore()

	os.MkdirAll(workDir, 0755)
	// A PID that cannot exist.
	os.WriteFile(runLockPath("dead"), []byte("999999999"), 0644)
	if runIsAlive("dead") {
		t.Fatal("dead PID: expected not alive")
	}
}

func TestRunIsAliveGarbage(t *testing.T) {
	restore := testDirs(t)
	defer restore()

	os.MkdirAll(workDir, 0755)
	os.WriteFile(runLockPath("junk"), []byte("not-a-pid"), 0644)
	if runIsAlive("junk") {
		t.Fatal("garbage lock content: expected not alive")
	}
}

func TestRetryRequeuesFailed(t *testing.T) {
	restore := testDirs(t)
	defer restore()

	seedTask(t, Task{ID: "f1", State: "failed", Title: "x", Branch: "factory/f1", PRURL: ""})
	if rc := cmdRetry([]string{"f1"}); rc != 0 {
		t.Fatalf("retry failed task: rc=%d", rc)
	}
	if got := taskState(t, "f1"); got != "queued" {
		t.Fatalf("expected queued, got %s", got)
	}
}

func TestRetryRefusesLiveRun(t *testing.T) {
	restore := testDirs(t)
	defer restore()

	seedTask(t, Task{ID: "r1", State: "running", Title: "x"})
	claimRun("r1") // this test process holds the lock → looks live
	if rc := cmdRetry([]string{"r1"}); rc == 0 {
		t.Fatal("expected refusal for a live run")
	}
	if got := taskState(t, "r1"); got != "running" {
		t.Fatalf("live run must stay running, got %s", got)
	}
	releaseRun("r1")
}

func TestRetryForceRequeuesLiveRun(t *testing.T) {
	restore := testDirs(t)
	defer restore()

	seedTask(t, Task{ID: "r2", State: "running", Title: "x"})
	claimRun("r2")
	if rc := cmdRetry([]string{"r2", "--force"}); rc != 0 {
		t.Fatalf("retry --force: rc=%d", rc)
	}
	if got := taskState(t, "r2"); got != "queued" {
		t.Fatalf("expected queued, got %s", got)
	}
}

func TestRetryRequeuesDeadRun(t *testing.T) {
	restore := testDirs(t)
	defer restore()

	seedTask(t, Task{ID: "r3", State: "running", Title: "x"})
	// Simulate a crash: stale lock with a dead PID.
	os.MkdirAll(workDir, 0755)
	os.WriteFile(runLockPath("r3"), []byte(strconv.Itoa(999999999)), 0644)
	if rc := cmdRetry([]string{"r3"}); rc != 0 {
		t.Fatalf("retry dead run: rc=%d", rc)
	}
	if got := taskState(t, "r3"); got != "queued" {
		t.Fatalf("expected queued, got %s", got)
	}
	if _, err := os.Stat(runLockPath("r3")); !os.IsNotExist(err) {
		t.Fatal("stale lock should be cleaned up")
	}
}

func TestRetryRefusesSplit(t *testing.T) {
	restore := testDirs(t)
	defer restore()

	seedTask(t, Task{ID: "s1", State: "split", Title: "x", Children: []int{11, 12}})
	if rc := cmdRetry([]string{"s1"}); rc == 0 {
		t.Fatal("expected refusal for split parent")
	}
	if got := taskState(t, "s1"); got != "split" {
		t.Fatalf("split parent must stay split, got %s", got)
	}
}

func TestRetryRefusesOpenPR(t *testing.T) {
	restore := testDirs(t)
	defer restore()

	seedTask(t, Task{ID: "p1", State: "pr_open", Title: "x", Branch: "factory/p1",
		PRURL: "https://github.com/o/r/pull/9"})
	if rc := cmdRetry([]string{"p1"}); rc == 0 {
		t.Fatal("expected refusal when a PR is already open")
	}
}

func TestRetryPRCreateWithoutURL(t *testing.T) {
	restore := testDirs(t)
	defer restore()

	// No github token in tests → createPR fails fast, task stays pr_open.
	seedTask(t, Task{ID: "p2", State: "pr_open", Title: "x", Branch: "factory/p2", PRURL: ""})
	if rc := cmdRetry([]string{"p2"}); rc == 0 {
		t.Fatal("expected failure when PR creation fails again")
	}
	if got := taskState(t, "p2"); got != "pr_open" {
		t.Fatalf("task must stay pr_open, got %s", got)
	}
}

func TestRetryNoSuchTask(t *testing.T) {
	restore := testDirs(t)
	defer restore()

	if rc := cmdRetry([]string{"missing"}); rc == 0 {
		t.Fatal("expected failure for unknown id")
	}
}
