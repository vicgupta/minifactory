package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const labelRevise = "factory-revise"

// hasReviseLabel reports whether the PR or its linked issue carries the
// factory-revise label. Zero numbers are skipped.
func hasReviseLabel(org, repo string, prNum, issueNum int) (bool, error) {
	var firstErr error
	for _, n := range []int{prNum, issueNum} {
		if n == 0 {
			continue
		}
		raw, err := ghDo("GET", "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(n)+"/labels", nil)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var ls []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &ls); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("parse labels: %w", err)
			}
			continue
		}
		for _, l := range ls {
			if l.Name == labelRevise {
				return true, nil
			}
		}
	}
	return false, firstErr
}

type feedbackItem struct {
	Body      string `json:"body"`
	State     string `json:"state"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	CreatedAt string `json:"created_at"`
	Submitted string `json:"submitted_at"`
	User      struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

func (f feedbackItem) ts() float64 {
	s := f.Submitted
	if s == "" {
		s = f.CreatedAt
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return float64(t.Unix())
}

func isBotComment(f feedbackItem) bool {
	l := strings.ToLower(f.User.Login)
	return f.User.Type == "Bot" || strings.HasSuffix(l, "[bot]") || strings.Contains(l, "minifactory")
}

func fetchFeedback(path string) ([]feedbackItem, error) {
	raw, err := ghDo("GET", path+"?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	var items []feedbackItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return items, nil
}

// buildFeedbackBundle collects reviews, inline comments and PR comments
// submitted after since (unix seconds). Bot comments and empty approvals are
// ignored. The bool is true when any actionable new feedback exists.
func buildFeedbackBundle(org, repo string, prNum int, since float64) (string, bool, error) {
	base := "/repos/" + org + "/" + repo
	n := strconv.Itoa(prNum)
	reviews, err := fetchFeedback(base + "/pulls/" + n + "/reviews")
	if err != nil {
		return "", false, err
	}
	inline, err := fetchFeedback(base + "/pulls/" + n + "/comments")
	if err != nil {
		return "", false, err
	}
	plain, err := fetchFeedback(base + "/issues/" + n + "/comments")
	if err != nil {
		return "", false, err
	}
	var sb strings.Builder
	found := false
	add := func(header string, f feedbackItem) {
		fmt.Fprintf(&sb, "%s by @%s:\n%s\n\n", header, f.User.Login, strings.TrimSpace(f.Body))
		found = true
	}
	skip := func(f feedbackItem) bool {
		return isBotComment(f) || f.ts() <= since || strings.TrimSpace(f.Body) == ""
	}
	for _, f := range reviews {
		if skip(f) || f.State == "PENDING" {
			continue
		}
		add("Review ("+f.State+")", f)
	}
	for _, f := range inline {
		if !skip(f) {
			add(fmt.Sprintf("Inline comment on %s:%d", f.Path, f.Line), f)
		}
	}
	for _, f := range plain {
		if !skip(f) {
			add("PR comment", f)
		}
	}
	return strings.TrimSpace(sb.String()), found, nil
}
