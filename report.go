package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// minifactory report: aggregate run records into "how is the factory
// doing?". Usage:
//
//	minifactory report                 # last 7 days, human-readable
//	minifactory report --since 30d     # window
//	minifactory report --task <id>     # every attempt for one task
//	minifactory report --json          # machine-readable aggregate
//
// Cost figures are the runners' reported model costs. On a Claude
// subscription token (CLAUDE_CODE_OAUTH_TOKEN) that number is an
// API-equivalent cost, not a real bill.

func parseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	n, ok := strings.CutSuffix(s, "d")
	if !ok {
		return 0, fmt.Errorf("want <N>d, got %q", s)
	}
	days, err := strconv.Atoi(n)
	if err != nil || days < 0 {
		return 0, fmt.Errorf("want <N>d, got %q", s)
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

func cmdReport(args []string) int {
	if id := flagVal(args, "task"); id != "" {
		return reportTask(id, hasFlag(args, "json"))
	}
	window := 7 * 24 * time.Hour
	if s := flagVal(args, "since"); s != "" {
		d, err := parseSince(s)
		if err != nil {
			fmt.Fprintln(os.Stderr, "report: --since:", err)
			return 2
		}
		window = d
	}
	return reportAggregate(time.Now().Add(-window), hasFlag(args, "json"))
}

// expensiveRun is one entry of the most-expensive list.
type expensiveRun struct {
	TaskID    string  `json:"task_id"`
	Title     string  `json:"title"`
	CostUSD   float64 `json:"cost_usd"`
	DurationS float64 `json:"duration_s"`
	Outcome   string  `json:"outcome"`
	PRURL     string  `json:"pr_url,omitempty"`
}

// reportAgg is the machine-readable aggregate for a window of runs.
type reportAgg struct {
	Since           string             `json:"since"`
	Until           string             `json:"until"`
	Runs            int                `json:"runs"`
	Outcomes        map[string]int     `json:"outcomes"`
	SuccessRate     float64            `json:"success_rate"`
	MedianPRTimeS   float64            `json:"median_time_to_pr_s"`
	P90PRTimeS      float64            `json:"p90_time_to_pr_s"`
	SlowestStage    string             `json:"slowest_stage"`
	SlowestStageAvg float64            `json:"slowest_stage_avg_s"`
	TotalCostUSD    float64            `json:"total_cost_usd"`
	CostByAgent     map[string]float64 `json:"cost_by_agent"`
	TotalTurns      int                `json:"total_turns"`
	OverBudgetRuns  int                `json:"over_budget_runs"`
	MostExpensive   []expensiveRun     `json:"most_expensive"`
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

func aggregateRuns(recs []*RunRecord, since time.Time) *reportAgg {
	agg := &reportAgg{
		Since:       since.Format("2006-01-02"),
		Until:       time.Now().Format("2006-01-02"),
		Outcomes:    map[string]int{},
		CostByAgent: map[string]float64{},
	}
	var prTimes []float64
	stageTotal := map[string]float64{}
	stageCount := map[string]int{}
	for _, r := range recs {
		agg.Runs++
		agg.Outcomes[r.Outcome]++
		if r.Outcome == "pr_open" {
			prTimes = append(prTimes, r.DurationS)
		}
		for _, st := range r.Stages {
			stageTotal[st.Name] += st.DurationS
			stageCount[st.Name]++
		}
		if r.OverBudget {
			agg.OverBudgetRuns++
		}
		if r.Usage != nil && r.Usage.Total != nil {
			u := r.Usage.Total
			if u.CostUSD != nil {
				agg.TotalCostUSD += *u.CostUSD
				agg.CostByAgent[r.Agent] += *u.CostUSD
			}
			if u.Turns != nil {
				agg.TotalTurns += *u.Turns
			}
		}
	}
	if agg.Runs > 0 {
		agg.SuccessRate = float64(agg.Outcomes["pr_open"]) / float64(agg.Runs)
	}
	sort.Float64s(prTimes)
	agg.MedianPRTimeS = percentile(prTimes, 0.5)
	agg.P90PRTimeS = percentile(prTimes, 0.9)
	for name, total := range stageTotal {
		avg := total / float64(stageCount[name])
		if avg > agg.SlowestStageAvg {
			agg.SlowestStage = name
			agg.SlowestStageAvg = avg
		}
	}
	type costed struct {
		r *RunRecord
		c float64
	}
	var byCost []costed
	for _, r := range recs {
		if r.Usage != nil && r.Usage.Total != nil && r.Usage.Total.CostUSD != nil {
			byCost = append(byCost, costed{r, *r.Usage.Total.CostUSD})
		}
	}
	sort.Slice(byCost, func(i, j int) bool { return byCost[i].c > byCost[j].c })
	for i := 0; i < len(byCost) && i < 5; i++ {
		r := byCost[i].r
		agg.MostExpensive = append(agg.MostExpensive, expensiveRun{
			TaskID: r.TaskID, Title: firstLine(r.Title, 60),
			CostUSD: byCost[i].c, DurationS: r.DurationS,
			Outcome: r.Outcome, PRURL: r.PRURL,
		})
	}
	return agg
}

func reportAggregate(since time.Time, asJSON bool) int {
	recs, err := loadRunRecords(since)
	if err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		return 1
	}
	agg := aggregateRuns(recs, since)
	if asJSON {
		b, _ := json.MarshalIndent(agg, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Printf("minifactory report — %s → %s\n", agg.Since, agg.Until)
	fmt.Printf("  runs: %d", agg.Runs)
	if agg.Runs > 0 {
		outs := []string{}
		for _, o := range []string{"pr_open", "failed", "split"} {
			if n := agg.Outcomes[o]; n > 0 {
				outs = append(outs, fmt.Sprintf("%d %s", n, o))
			}
		}
		fmt.Printf(" (%s) · success rate %.0f%%", strings.Join(outs, ", "), agg.SuccessRate*100)
	}
	fmt.Println()
	if len(agg.MostExpensive) > 0 || agg.TotalCostUSD > 0 {
		fmt.Printf("  time to PR: median %s, p90 %s\n", formatDuration(agg.MedianPRTimeS), formatDuration(agg.P90PRTimeS))
	}
	if agg.SlowestStage != "" {
		fmt.Printf("  slowest stage on average: %s (%s)\n", agg.SlowestStage, formatDuration(agg.SlowestStageAvg))
	}
	fmt.Printf("  cost: $%.2f total", agg.TotalCostUSD)
	if len(agg.CostByAgent) > 0 {
		parts := []string{}
		agents := make([]string, 0, len(agg.CostByAgent))
		for a := range agg.CostByAgent {
			agents = append(agents, a)
		}
		sort.Strings(agents)
		for _, a := range agents {
			parts = append(parts, fmt.Sprintf("%s $%.2f", a, agg.CostByAgent[a]))
		}
		fmt.Printf(" (%s)", strings.Join(parts, ", "))
	}
	fmt.Printf(" · %d turns\n", agg.TotalTurns)
	if agg.OverBudgetRuns > 0 {
		fmt.Printf("  over budget: %d run(s) exceeded MAX_COST_USD_PER_TASK\n", agg.OverBudgetRuns)
	}
	if len(agg.MostExpensive) > 0 {
		fmt.Println("  most expensive:")
		for _, e := range agg.MostExpensive {
			fmt.Printf("    %s  $%.2f  %s  %s  %.60s\n", e.TaskID, e.CostUSD, formatDuration(e.DurationS), e.Outcome, e.Title)
			if e.PRURL != "" {
				fmt.Printf("      %s\n", e.PRURL)
			}
		}
	}
	fmt.Println("  (costs are the runners' reported model costs; on a Claude subscription token this is an API-equivalent cost, not a real bill)")
	return 0
}

// reportTask prints every attempt for one task, stage by stage.
func reportTask(id string, asJSON bool) int {
	recs, err := loadTaskRecords(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "report: no run records for "+id)
		return 1
	}
	if asJSON {
		b, _ := json.MarshalIndent(recs, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Printf("task %s — %.80s (%d attempt(s))\n", id, firstLine(recs[0].Title, 80), len(recs))
	for _, r := range recs {
		ts := time.Unix(int64(r.StartedAt), 0).Format("2006-01-02 15:04:05")
		fmt.Printf("attempt %d (%s): outcome %s · %s\n", r.Attempt, ts, r.Outcome, r.summaryLine())
		if r.FailReason != "" {
			fmt.Printf("  failed: %s\n", r.FailReason)
		}
		if r.PRURL != "" {
			fmt.Printf("  pr: %s\n", r.PRURL)
		}
		if r.OverBudget {
			fmt.Printf("  over budget: exceeded MAX_COST_USD_PER_TASK\n")
		}
		for _, st := range r.Stages {
			mark := "ok"
			if !st.OK {
				mark = "FAILED"
			}
			fmt.Printf("  %-14s %-8s %s\n", st.Name, formatDuration(st.DurationS), mark)
		}
	}
	return 0
}
