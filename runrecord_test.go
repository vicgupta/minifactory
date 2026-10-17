package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// capturedClaudeResult is a realistic `claude -p --output-format json`
// payload (trimmed): the shape parseClaudeUsage must handle.
const capturedClaudeResult = `{
  "type": "result",
  "subtype": "success",
  "is_error": false,
  "duration_ms": 372123,
  "num_turns": 18,
  "total_cost_usd": 0.4123,
  "usage": {
    "input_tokens": 12480,
    "output_tokens": 3215,
    "cache_read_input_tokens": 90210,
    "cache_creation_input_tokens": 1200
  },
  "result": "done"
}`

func TestParseClaudeUsage(t *testing.T) {
	u := parseClaudeUsage(capturedClaudeResult)
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.Turns == nil || *u.Turns != 18 {
		t.Errorf("turns = %v, want 18", u.Turns)
	}
	if u.CostUSD == nil || *u.CostUSD != 0.4123 {
		t.Errorf("cost = %v, want 0.4123", u.CostUSD)
	}
	if u.InputTokens == nil || *u.InputTokens != 12480 {
		t.Errorf("input_tokens = %v, want 12480", u.InputTokens)
	}
	if u.OutputTokens == nil || *u.OutputTokens != 3215 {
		t.Errorf("output_tokens = %v, want 3215", u.OutputTokens)
	}
	if u.CacheReadTokens == nil || *u.CacheReadTokens != 90210 {
		t.Errorf("cache_read_tokens = %v, want 90210", u.CacheReadTokens)
	}
}

func TestParseClaudeUsageMalformed(t *testing.T) {
	for _, bad := range []string{"", "not json at all", "{oops", "[[[", "plain text\nmore text"} {
		if u := parseClaudeUsage(bad); u != nil {
			t.Errorf("parseClaudeUsage(%q) = %+v, want nil", bad, u)
		}
	}
}

func TestParseClaudeUsageNoUsageFields(t *testing.T) {
	// Valid JSON but no usage payload: empty record, not nil, never fatal.
	u := parseClaudeUsage(`{"type":"result","subtype":"error","is_error":true}`)
	if u == nil {
		t.Fatal("expected non-nil empty usage")
	}
	if u.Turns != nil || u.CostUSD != nil {
		t.Errorf("expected all-nil fields, got %+v", u)
	}
}

func TestParseClaudeUsageEmbeddedNoise(t *testing.T) {
	// docker/stderr noise around the JSON: the extractor finds the object.
	u := parseClaudeUsage("some warning line\n" + capturedClaudeResult + "\ntrailing noise")
	if u == nil || u.Turns == nil || *u.Turns != 18 {
		t.Fatalf("expected parsed usage, got %+v", u)
	}
}

func TestZeroUsage(t *testing.T) {
	u := zeroUsage()
	if u.Turns == nil || *u.Turns != 0 || u.CostUSD == nil || *u.CostUSD != 0 {
		t.Errorf("stub usage should be explicit zeros, got %+v", u)
	}
	// Zeros must serialize explicitly, not vanish.
	b, _ := json.Marshal(u)
	for _, want := range []string{`"turns":0`, `"cost_usd":0`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("zero usage JSON %s missing %s", b, want)
		}
	}
}

func TestCombineUsage(t *testing.T) {
	a := &AgentUsage{Turns: intPtr(3), CostUSD: floatPtr(0.05), InputTokens: int64Ptr(100)}
	b := &AgentUsage{Turns: intPtr(18), CostUSD: floatPtr(0.41), OutputTokens: int64Ptr(200)}
	c := combineUsage(a, b)
	if *c.Turns != 21 || *c.InputTokens != 100 || *c.OutputTokens != 200 {
		t.Errorf("bad combine: %+v", c)
	}
	if diff := *c.CostUSD - 0.46; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("bad combine cost: %v", *c.CostUSD)
	}
	if c.CacheReadTokens != nil {
		t.Errorf("both nil should stay nil, got %v", c.CacheReadTokens)
	}
	if combineUsage(nil, b) != b || combineUsage(a, nil) != a {
		t.Error("nil side should return the other side")
	}
}

func seedRunRecord(t *testing.T, tid string, attempt int) *RunRecord {
	t.Helper()
	r := &RunRecord{
		TaskID: tid, Attempt: attempt, RepoURL: "https://github.com/o/r.git",
		Title: "t", Agent: "stub", Selection: "token priority",
		StartedAt: nowFloat() - 100, FinishedAt: nowFloat(), DurationS: 100,
		Outcome: "pr_open", Stages: []StageRec{{Name: "clone", DurationS: 5, OK: true}},
	}
	r.FinishedAt = r.StartedAt + r.DurationS
	if err := r.write(); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNextAttempt(t *testing.T) {
	restore := testDirs(t)
	defer restore()
	if n := nextAttempt("abc"); n != 1 {
		t.Fatalf("want 1, got %d", n)
	}
	seedRunRecord(t, "abc", 1)
	if n := nextAttempt("abc"); n != 2 {
		t.Fatalf("want 2, got %d", n)
	}
	seedRunRecord(t, "abc", 2)
	if n := nextAttempt("abc"); n != 3 {
		t.Fatalf("want 3, got %d", n)
	}
	// Another task is unaffected.
	if n := nextAttempt("xyz"); n != 1 {
		t.Fatalf("want 1 for new task, got %d", n)
	}
}

func TestRunRecordWriteLoadRoundTrip(t *testing.T) {
	restore := testDirs(t)
	defer restore()
	r := seedRunRecord(t, "abc", 1)
	r.Usage = &UsageRec{Agent: zeroUsage(), Total: zeroUsage()}
	if err := r.write(); err != nil {
		t.Fatal(err)
	}
	recs, err := loadTaskRecords("abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	got := recs[0]
	if got.Outcome != "pr_open" || got.Agent != "stub" || len(got.Stages) != 1 {
		t.Errorf("bad round trip: %+v", got)
	}
	if got.Usage == nil || got.Usage.Total.CostUSD == nil || *got.Usage.Total.CostUSD != 0 {
		t.Errorf("usage lost in round trip: %+v", got.Usage)
	}
	// Atomic write: no temp files left behind.
	files, _ := os.ReadDir(filepath.Join(runsDir(), "abc"))
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "tmp-") {
			t.Errorf("temp file left behind: %s", f.Name())
		}
	}
}

func TestFailTaskRecord(t *testing.T) {
	restore := testDirs(t)
	defer restore()
	task := Task{ID: "abc", Title: "t", RepoURL: "https://github.com/o/r.git", State: "running"}
	seedTask(t, task)
	rec := newRunRecord(&task)
	rec.beginStage("clone")
	rec.endStage(true)
	rec.beginStage("agent")
	if rc := failTask(&task, rec, "validation failed — no PR opened"); rc != 1 {
		t.Fatalf("failTask rc = %d, want 1", rc)
	}
	// failTask finishes the record; cmdRunOnce's deferred write persists it.
	if err := rec.write(); err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != "failed" {
		t.Errorf("outcome = %q, want failed", rec.Outcome)
	}
	if rec.FailReason != "validation failed — no PR opened" {
		t.Errorf("fail_reason = %q", rec.FailReason)
	}
	if len(rec.Stages) != 2 || !rec.Stages[0].OK || rec.Stages[1].OK {
		t.Errorf("stages should be [ok, failed], got %+v", rec.Stages)
	}
	if taskState(t, "abc") != "failed" {
		t.Error("task state should be failed")
	}
	// The record on disk matches.
	recs, _ := loadTaskRecords("abc")
	if len(recs) != 1 || recs[0].Outcome != "failed" {
		t.Fatalf("record not persisted as failed: %+v", recs)
	}
}

func TestSummaryLine(t *testing.T) {
	r := &RunRecord{Agent: "claude", Sandboxed: true, DurationS: 372,
		Usage: &UsageRec{Total: &AgentUsage{Turns: intPtr(18), CostUSD: floatPtr(0.41)}}}
	if got := r.summaryLine(); got != "took 6m12s · 18 turns · $0.41 (claude, sandboxed)" {
		t.Errorf("got %q", got)
	}
	// No usage: degrades without inventing numbers.
	r2 := &RunRecord{Agent: "stub", Sandboxed: true, DurationS: 45}
	if got := r2.summaryLine(); got != "took 45s (stub, sandboxed)" {
		t.Errorf("got %q", got)
	}
	// Host fallback reads "host".
	r3 := &RunRecord{Agent: "codex", Sandboxed: false, DurationS: 61}
	if got := r3.summaryLine(); got != "took 1m01s (codex, host)" {
		t.Errorf("got %q", got)
	}
}

func TestCheckOverBudget(t *testing.T) {
	restore := testDirs(t)
	defer restore()
	old := maxCostUSD
	defer func() { maxCostUSD = old }()

	task := Task{ID: "abc"}
	rec := newRunRecord(&task)
	maxCostUSD = 0.5
	checkOverBudget(rec, &AgentUsage{CostUSD: floatPtr(0.41)}, "agent")
	if rec.OverBudget {
		t.Error("0.41 under a 0.50 cap should not flag")
	}
	checkOverBudget(rec, &AgentUsage{CostUSD: floatPtr(0.51)}, "agent")
	if !rec.OverBudget {
		t.Error("0.51 over a 0.50 cap should flag")
	}
	// Guard off: never flags.
	rec2 := newRunRecord(&task)
	maxCostUSD = 0
	checkOverBudget(rec2, &AgentUsage{CostUSD: floatPtr(99)}, "agent")
	if rec2.OverBudget {
		t.Error("guard off should never flag")
	}
	// Unknown cost: never flags.
	rec3 := newRunRecord(&task)
	maxCostUSD = 0.5
	checkOverBudget(rec3, &AgentUsage{Turns: intPtr(5)}, "agent")
	if rec3.OverBudget {
		t.Error("nil cost should never flag")
	}
}

func TestParseCostUSD(t *testing.T) {
	if parseCostUSD("") != 0 || parseCostUSD("0") != 0 || parseCostUSD("-1") != 0 {
		t.Error("empty/zero/negative should disable the guard")
	}
	if v := parseCostUSD("2.50"); v != 2.5 {
		t.Errorf("got %v, want 2.5", v)
	}
	if parseCostUSD("junk") != 0 {
		t.Error("invalid should disable the guard")
	}
}

func fixtureRecord(tid string, agent, outcome string, dur float64, cost *float64, turns *int, overBudget bool, startedAgo time.Duration) *RunRecord {
	r := &RunRecord{
		TaskID: tid, Attempt: 1, Title: "task " + tid, Agent: agent,
		Selection: "token priority", Sandboxed: true,
		StartedAt: float64(time.Now().Add(-startedAgo).UnixNano()) / 1e9,
		DurationS: dur, Outcome: outcome, OverBudget: overBudget,
		Stages: []StageRec{
			{Name: "clone", DurationS: 10, OK: true},
			{Name: "agent", DurationS: dur - 10, OK: outcome != "failed"},
		},
	}
	r.FinishedAt = r.StartedAt + dur
	if cost != nil || turns != nil {
		r.Usage = &UsageRec{Agent: &AgentUsage{CostUSD: cost, Turns: turns}, Total: &AgentUsage{CostUSD: cost, Turns: turns}}
	}
	return r
}

func writeFixture(t *testing.T, r *RunRecord) {
	t.Helper()
	if err := r.write(); err != nil {
		t.Fatal(err)
	}
}

func TestAggregateRuns(t *testing.T) {
	restore := testDirs(t)
	defer restore()
	c1, c2, c3 := 0.40, 1.20, 0.10
	t1, t2, t3 := 18, 40, 5
	writeFixture(t, fixtureRecord("a1", "claude", "pr_open", 300, &c1, &t1, false, 24*time.Hour))
	writeFixture(t, fixtureRecord("a2", "claude", "pr_open", 600, &c2, &t2, true, 48*time.Hour))
	writeFixture(t, fixtureRecord("a3", "codex", "failed", 120, &c3, &t3, false, 72*time.Hour))
	writeFixture(t, fixtureRecord("a4", "stub", "split", 60, nil, nil, false, 96*time.Hour))

	recs, err := loadRunRecords(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	agg := aggregateRuns(recs, time.Now().Add(-7*24*time.Hour))
	if agg.Runs != 4 {
		t.Fatalf("runs = %d, want 4", agg.Runs)
	}
	if agg.Outcomes["pr_open"] != 2 || agg.Outcomes["failed"] != 1 || agg.Outcomes["split"] != 1 {
		t.Errorf("bad outcomes: %+v", agg.Outcomes)
	}
	if agg.SuccessRate != 0.5 {
		t.Errorf("success rate = %v, want 0.5", agg.SuccessRate)
	}
	// Time to PR over the two pr_open runs (300s, 600s).
	if agg.MedianPRTimeS != 300 || agg.P90PRTimeS != 300 {
		t.Errorf("median/p90 = %v/%v, want 300/300", agg.MedianPRTimeS, agg.P90PRTimeS)
	}
	// Agent stage dominates: (290+590+110+50)/4 = 260 avg.
	if agg.SlowestStage != "agent" {
		t.Errorf("slowest stage = %q, want agent", agg.SlowestStage)
	}
	if diff := agg.TotalCostUSD - 1.7; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("total cost = %v, want 1.7", agg.TotalCostUSD)
	}
	if diff := agg.CostByAgent["claude"] - 1.6; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("cost by agent = %+v", agg.CostByAgent)
	}
	if diff := agg.CostByAgent["codex"] - 0.1; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("cost by agent = %+v", agg.CostByAgent)
	}
	if _, ok := agg.CostByAgent["stub"]; ok {
		t.Errorf("stub with nil cost should not appear: %+v", agg.CostByAgent)
	}
	if agg.TotalTurns != 63 {
		t.Errorf("turns = %d, want 63", agg.TotalTurns)
	}
	if agg.OverBudgetRuns != 1 {
		t.Errorf("over-budget = %d, want 1", agg.OverBudgetRuns)
	}
	if len(agg.MostExpensive) != 3 || agg.MostExpensive[0].TaskID != "a2" {
		t.Errorf("most expensive wrong: %+v", agg.MostExpensive)
	}
}

func TestLoadRunRecordsSinceFiltering(t *testing.T) {
	restore := testDirs(t)
	defer restore()
	c := 0.1
	writeFixture(t, fixtureRecord("new", "claude", "pr_open", 60, &c, nil, false, 24*time.Hour))
	writeFixture(t, fixtureRecord("old", "claude", "pr_open", 60, &c, nil, false, 30*24*time.Hour))

	recs, err := loadRunRecords(time.Now().Add(-7 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].TaskID != "new" {
		t.Fatalf("since filter failed: %+v", recs)
	}
	all, err := loadRunRecords(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered load = %d, want 2", len(all))
	}
}

func TestParseSince(t *testing.T) {
	d, err := parseSince("30d")
	if err != nil || d != 30*24*time.Hour {
		t.Errorf("got %v, %v", d, err)
	}
	if _, err := parseSince("7"); err == nil {
		t.Error("missing unit should fail")
	}
	if _, err := parseSince("-3d"); err == nil {
		t.Error("negative should fail")
	}
	if _, err := parseSince("xd"); err == nil {
		t.Error("non-numeric should fail")
	}
}

func TestReportTaskAttempts(t *testing.T) {
	restore := testDirs(t)
	defer restore()
	seedRunRecord(t, "abc", 1)
	seedRunRecord(t, "abc", 2)
	recs, err := loadTaskRecords("abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Attempt != 1 || recs[1].Attempt != 2 {
		t.Fatalf("attempts out of order: %+v", recs)
	}
}
