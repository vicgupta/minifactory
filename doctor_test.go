package main

import (
	"strings"
	"testing"
	"time"
)

func mkTask(id, state string, createdAgo time.Duration) Task {
	return Task{
		ID:        id,
		State:     state,
		CreatedAt: float64(time.Now().Add(-createdAgo).UnixNano()) / 1e9,
	}
}

func TestAnalyzeTasksCounts(t *testing.T) {
	tasks := []Task{
		mkTask("a", "queued", time.Hour),
		mkTask("b", "queued", 30*time.Minute),
		mkTask("c", "running", 5*time.Minute),
		mkTask("d", "pr_open", 2*time.Hour),
		mkTask("e", "failed", 3*time.Hour),
		mkTask("f", "done", 4*time.Hour),
	}
	r := analyzeTasks(tasks, time.Now())
	if r.total != 6 {
		t.Errorf("total = %d, want 6", r.total)
	}
	for state, want := range map[string]int{"queued": 2, "running": 1, "pr_open": 1, "failed": 1, "done": 1} {
		if r.counts[state] != want {
			t.Errorf("counts[%s] = %d, want %d", state, r.counts[state], want)
		}
	}
	if len(r.staleRunning) != 0 {
		t.Errorf("staleRunning = %v, want empty (5m run is fresh)", r.staleRunning)
	}
	if len(r.failed) != 1 || r.failed[0] != "e" {
		t.Errorf("failed = %v, want [e]", r.failed)
	}
	if !strings.HasPrefix(r.oldestQueued, "a (") {
		t.Errorf("oldestQueued = %q, want id a", r.oldestQueued)
	}
}

func TestAnalyzeTasksStaleRunning(t *testing.T) {
	tasks := []Task{
		mkTask("fresh", "running", 10*time.Minute),
		mkTask("stale1", "running", 50*time.Minute),
		mkTask("stale2", "running", 3*time.Hour),
	}
	r := analyzeTasks(tasks, time.Now())
	if len(r.staleRunning) != 2 || r.staleRunning[0] != "stale1" || r.staleRunning[1] != "stale2" {
		t.Errorf("staleRunning = %v, want [stale1 stale2]", r.staleRunning)
	}
}

func TestAnalyzeTasksEmpty(t *testing.T) {
	r := analyzeTasks(nil, time.Now())
	if r.total != 0 || len(r.staleRunning) != 0 || r.oldestQueued != "" {
		t.Errorf("empty queue report = %+v, want zero values", r)
	}
}

func TestShowVal(t *testing.T) {
	out := "ActiveState=active\nUnitFileState=enabled\nLastTriggerUSec=Tue 2026-09-29 22:59:56 UTC\n"
	if got := showVal(out, "ActiveState"); got != "active" {
		t.Errorf("showVal ActiveState = %q, want active", got)
	}
	if got := showVal(out, "LastTriggerUSec"); got != "Tue 2026-09-29 22:59:56 UTC" {
		t.Errorf("showVal LastTriggerUSec = %q", got)
	}
	if got := showVal(out, "Nope"); got != "" {
		t.Errorf("showVal missing key = %q, want empty", got)
	}
}

func TestParseSystemdTime(t *testing.T) {
	// newer systemd renders timestamps as text
	if got := parseSystemdTime("Tue 2026-09-29 22:59:56 UTC"); got.Unix() != 1790722796 {
		t.Errorf("text form: got %v", got)
	}
	// older systemd uses raw microseconds
	if got := parseSystemdTime("1790722796000000"); got.Unix() != 1790722796 {
		t.Errorf("usec form: got %v", got)
	}
	// empty / n/a stay zero
	if !parseSystemdTime("").IsZero() || !parseSystemdTime("n/a").IsZero() {
		t.Error("empty/n/a should parse to zero time")
	}
}

func TestParseListTimersNext(t *testing.T) {
	out := "Tue 2026-09-29 23:01:57 UTC 55s Tue 2026-09-29 23:00:57 UTC 4s ago minifactory-go.timer minifactory-go.service\n"
	if got := parseListTimersNext(out); got.Unix() != 1790722917 {
		t.Errorf("got %v, want epoch 1790722917", got)
	}
	if !parseListTimersNext("").IsZero() {
		t.Error("empty output should give zero time")
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1536, "1.5 KB"},
		{5 * 1024 * 1024, "5.0 MB"},
		{3 * 1024 * 1024 * 1024, "3.0 GB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShortDur(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{90 * time.Second, "1m30s"},
		{3*time.Hour + 12*time.Minute, "3h12m"},
		{45 * time.Second, "45s"},
	} {
		if got := shortDur(tc.in); got != tc.want {
			t.Errorf("shortDur(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExitFor(t *testing.T) {
	if exitFor([]checkResult{{Status: statusOK}, {Status: statusWarn}}) != 0 {
		t.Error("warn alone should exit 0")
	}
	if exitFor([]checkResult{{Status: statusOK}, {Status: statusFail}}) != 1 {
		t.Error("any fail should exit 1")
	}
}
