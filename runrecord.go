package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Run records: a structured per-attempt account of every task run. The
// queue only keeps the latest state; the record keeps how the run went —
// per-stage durations, agent usage and cost — so `minifactory report` can
// answer "how is the factory doing?" without re-reading free-text logs.
// Records live at data/runs/<task-id>/<attempt>.json, written atomically.

// AgentUsage is the model usage of one runner invocation. Every field is a
// pointer so "unknown" stays null in the JSON instead of being confused
// with zero: codex/opencode expose no reliable usage output, so theirs is
// nil; the stub reports explicit zeros.
type AgentUsage struct {
	Turns           *int     `json:"turns"`
	CostUSD         *float64 `json:"cost_usd"`
	InputTokens     *int64   `json:"input_tokens"`
	OutputTokens    *int64   `json:"output_tokens"`
	CacheReadTokens *int64   `json:"cache_read_tokens"`
}

// UsageRec splits usage into the sizing pass and the main agent run, with
// a total across both. Either side may be nil when that runner produced no
// usage data.
type UsageRec struct {
	Sizer *AgentUsage `json:"sizer,omitempty"`
	Agent *AgentUsage `json:"agent,omitempty"`
	Total *AgentUsage `json:"total,omitempty"`
}

// StageRec is one timed pipeline stage. Skipped stages are left out of the
// record entirely.
type StageRec struct {
	Name      string  `json:"name"`
	StartedAt float64 `json:"started_at"`
	DurationS float64 `json:"duration_s"`
	OK        bool    `json:"ok"`
}

// RunRecord is the per-attempt record. Outcome is one of "pr_open",
// "failed" or "split".
type RunRecord struct {
	TaskID       string     `json:"task_id"`
	Attempt      int        `json:"attempt"`
	IssueNumber  int        `json:"issue_number,omitempty"`
	RepoURL      string     `json:"repo_url"`
	Title        string     `json:"title"`
	Agent        string     `json:"agent"`
	Selection    string     `json:"selection"`
	Sandboxed    bool       `json:"sandboxed"`
	StartedAt    float64    `json:"started_at"`
	FinishedAt   float64    `json:"finished_at,omitempty"`
	DurationS    float64    `json:"duration_s,omitempty"`
	Stages       []StageRec `json:"stages"`
	Outcome      string     `json:"outcome,omitempty"`
	FailReason   string     `json:"fail_reason,omitempty"`
	PRURL        string     `json:"pr_url,omitempty"`
	Usage        *UsageRec  `json:"usage,omitempty"`
	ChangedFiles int        `json:"changed_files,omitempty"`
	Summary      string     `json:"summary,omitempty"`
	OverBudget   bool       `json:"over_budget,omitempty"`
}

func runsDir() string { return filepath.Join(dataDir, "runs") }

func nowFloat() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// nextAttempt returns the attempt number for a new run of tid: one more
// than the records already on disk, so `retry` keeps every attempt.
func nextAttempt(tid string) int {
	entries, err := os.ReadDir(filepath.Join(runsDir(), tid))
	if err != nil {
		return 1
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n + 1
}

// newRunRecord starts the record for one run-once attempt.
func newRunRecord(t *Task) *RunRecord {
	agent, selection := agentSelection()
	return &RunRecord{
		TaskID:      t.ID,
		Attempt:     nextAttempt(t.ID),
		IssueNumber: t.IssueNumber,
		RepoURL:     t.RepoURL,
		Title:       t.Title,
		Agent:       agent,
		Selection:   selection,
		StartedAt:   nowFloat(),
		Stages:      []StageRec{},
	}
}

// beginStage opens a timed stage; endStage closes the most recently opened
// one. A stage left open when the run fails is closed as failed by
// finish/failTask, so callers only close the happy path explicitly.
func (r *RunRecord) beginStage(name string) {
	r.Stages = append(r.Stages, StageRec{Name: name, StartedAt: nowFloat()})
}

func (r *RunRecord) endStage(ok bool) {
	for i := len(r.Stages) - 1; i >= 0; i-- {
		if r.Stages[i].DurationS == 0 && r.Stages[i].StartedAt != 0 {
			r.Stages[i].DurationS = nowFloat() - r.Stages[i].StartedAt
			r.Stages[i].OK = ok
			return
		}
	}
}

// finish stamps the end of the run; idempotent, so failTask and the
// deferred write can both call it.
func (r *RunRecord) finish() {
	if r.FinishedAt != 0 {
		return
	}
	r.FinishedAt = nowFloat()
	r.DurationS = r.FinishedAt - r.StartedAt
	r.endStage(false) // a stage still open at finish did not complete
}

// write persists the record atomically: temp file in the same directory,
// then rename.
func (r *RunRecord) write() error {
	dir := filepath.Join(runsDir(), r.TaskID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "tmp-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, fmt.Sprintf("%d.json", r.Attempt)))
}

// formatDuration renders seconds as "6m12s", "45s" or "1h02m".
func formatDuration(s float64) string {
	if s < 0 {
		s = 0
	}
	total := int(s + 0.5)
	h, m, sec := total/3600, (total%3600)/60, total%60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, sec)
	}
	return fmt.Sprintf("%ds", sec)
}

// summaryLine is the one-line run summary appended to the done/failed
// issue comments, e.g. "took 6m12s · 18 turns · $0.41 (claude, sandboxed)".
// Missing usage degrades gracefully instead of printing zeros as facts.
func (r *RunRecord) summaryLine() string {
	dur := r.DurationS
	if dur == 0 && r.StartedAt != 0 {
		dur = nowFloat() - r.StartedAt
	}
	parts := []string{"took " + formatDuration(dur)}
	if r.Usage != nil && r.Usage.Total != nil {
		if r.Usage.Total.Turns != nil {
			parts = append(parts, fmt.Sprintf("%d turns", *r.Usage.Total.Turns))
		}
		if r.Usage.Total.CostUSD != nil {
			parts = append(parts, fmt.Sprintf("$%.2f", *r.Usage.Total.CostUSD))
		}
	}
	mode := "sandboxed"
	if !r.Sandboxed {
		mode = "host"
	}
	agent := r.Agent
	if agent == "" {
		agent = "?"
	}
	return strings.Join(parts, " · ") + fmt.Sprintf(" (%s, %s)", agent, mode)
}

func intPtr(i int) *int           { return &i }
func int64Ptr(i int64) *int64     { return &i }
func floatPtr(f float64) *float64 { return &f }

// zeroUsage is the usage record for the stub agent: explicit zeros, so
// reports can distinguish "ran for free" from "unknown".
func zeroUsage() *AgentUsage {
	return &AgentUsage{
		Turns: intPtr(0), CostUSD: floatPtr(0),
		InputTokens: int64Ptr(0), OutputTokens: int64Ptr(0), CacheReadTokens: int64Ptr(0),
	}
}

// combineUsage adds two usage records field-wise; nil fields stay nil
// unless the other side knows them.
func combineUsage(a, b *AgentUsage) *AgentUsage {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	addInt := func(x, y *int) *int {
		if x == nil {
			return y
		}
		if y == nil {
			return x
		}
		return intPtr(*x + *y)
	}
	addInt64 := func(x, y *int64) *int64 {
		if x == nil {
			return y
		}
		if y == nil {
			return x
		}
		return int64Ptr(*x + *y)
	}
	addFloat := func(x, y *float64) *float64 {
		if x == nil {
			return y
		}
		if y == nil {
			return x
		}
		return floatPtr(*x + *y)
	}
	return &AgentUsage{
		Turns:           addInt(a.Turns, b.Turns),
		CostUSD:         addFloat(a.CostUSD, b.CostUSD),
		InputTokens:     addInt64(a.InputTokens, b.InputTokens),
		OutputTokens:    addInt64(a.OutputTokens, b.OutputTokens),
		CacheReadTokens: addInt64(a.CacheReadTokens, b.CacheReadTokens),
	}
}

// setUsage records the sizer and agent usage and derives the total.
func (r *RunRecord) setUsage(sizer, agent *AgentUsage) {
	if sizer == nil && agent == nil {
		return
	}
	r.Usage = &UsageRec{Sizer: sizer, Agent: agent, Total: combineUsage(sizer, agent)}
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// parseClaudeUsage extracts usage from a `claude -p --output-format json`
// result payload. Malformed or missing JSON returns nil — never a failed
// task. A valid payload without usage fields returns an empty record.
func parseClaudeUsage(out string) *AgentUsage {
	s := strings.TrimSpace(out)
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) != nil {
		start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
		if start < 0 || end <= start || json.Unmarshal([]byte(s[start:end+1]), &m) != nil {
			return nil
		}
	}
	u := &AgentUsage{}
	if v, ok := toInt(m["num_turns"]); ok {
		u.Turns = intPtr(v)
	}
	if v, ok := toFloat(m["total_cost_usd"]); ok {
		u.CostUSD = floatPtr(v)
	}
	if um, ok := m["usage"].(map[string]any); ok {
		if v, ok := toInt64(um["input_tokens"]); ok {
			u.InputTokens = int64Ptr(v)
		}
		if v, ok := toInt64(um["output_tokens"]); ok {
			u.OutputTokens = int64Ptr(v)
		}
		if v, ok := toInt64(um["cache_read_input_tokens"]); ok {
			u.CacheReadTokens = int64Ptr(v)
		}
	}
	return u
}

// loadRunRecords reads every run record under data/runs started at or
// after since (zero since means all).
func loadRunRecords(since time.Time) ([]*RunRecord, error) {
	var out []*RunRecord
	taskDirs, err := os.ReadDir(runsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, td := range taskDirs {
		if !td.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(runsDir(), td.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(runsDir(), td.Name(), f.Name()))
			if err != nil {
				continue
			}
			var r RunRecord
			if json.Unmarshal(b, &r) != nil {
				continue
			}
			if !since.IsZero() && r.StartedAt < float64(since.UnixNano())/1e9 {
				continue
			}
			out = append(out, &r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt < out[j].StartedAt })
	return out, nil
}

// loadTaskRecords reads every attempt record for one task, oldest first.
func loadTaskRecords(tid string) ([]*RunRecord, error) {
	var out []*RunRecord
	files, err := os.ReadDir(filepath.Join(runsDir(), tid))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(runsDir(), tid, f.Name()))
		if err != nil {
			continue
		}
		var r RunRecord
		if json.Unmarshal(b, &r) != nil {
			continue
		}
		out = append(out, &r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Attempt < out[j].Attempt })
	return out, nil
}

// parseCostUSD reads the MAX_COST_USD_PER_TASK .env knob: a positive
// dollar amount, otherwise 0 (guard off). Invalid values warn and fall back.
func parseCostUSD(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		fmt.Fprintf(os.Stderr, "warning: invalid MAX_COST_USD_PER_TASK %q, budget guard off\n", s)
		return 0
	}
	return f
}

// checkOverBudget flags the run record when a runner's reported model cost
// exceeds the MAX_COST_USD_PER_TASK cap. Flag-only: runs are never killed
// mid-flight for going over budget.
func checkOverBudget(rec *RunRecord, u *AgentUsage, stage string) {
	if maxCostUSD <= 0 || u == nil || u.CostUSD == nil {
		return
	}
	if *u.CostUSD > maxCostUSD {
		rec.OverBudget = true
		logf(rec.TaskID, "%s: reported cost $%.2f over budget cap $%.2f — flagged, continuing", stage, *u.CostUSD, maxCostUSD)
	}
}
