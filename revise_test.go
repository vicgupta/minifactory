package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func stubGh(t *testing.T, routes map[string]string) {
	old := ghDo
	t.Cleanup(func() { ghDo = old })
	ghDo = func(method, path string, data any) ([]byte, error) {
		path = strings.TrimSuffix(path, "?per_page=100")
		if r, ok := routes[path]; ok {
			return []byte(r), nil
		}
		return nil, fmt.Errorf("unexpected %s", path)
	}
}

func TestHasReviseLabel(t *testing.T) {
	stubGh(t, map[string]string{
		"/repos/o/r/issues/5/labels": `[{"name":"bug"}]`,
		"/repos/o/r/issues/7/labels": `[{"name":"factory-revise"}]`,
	})
	if ok, err := hasReviseLabel("o", "r", 5, 7); !ok || err != nil {
		t.Fatalf("issue label: %v %v", ok, err)
	}
	if ok, _ := hasReviseLabel("o", "r", 7, 0); !ok {
		t.Fatal("pr label")
	}
	if ok, _ := hasReviseLabel("o", "r", 5, 0); ok {
		t.Fatal("no label should not trigger")
	}
}

func ts(sec int64) string { return time.Unix(sec, 0).UTC().Format(time.RFC3339) }

func TestBuildFeedbackBundle(t *testing.T) {
	stubGh(t, map[string]string{
		"/repos/o/r/pulls/3/reviews": fmt.Sprintf(`[
{"state":"APPROVED","body":"","submitted_at":%q,"user":{"login":"a"}},
{"state":"CHANGES_REQUESTED","body":"old one","submitted_at":%q,"user":{"login":"b"}},
{"state":"CHANGES_REQUESTED","body":"fix the thing","submitted_at":%q,"user":{"login":"c"}}]`, ts(2000), ts(500), ts(2000)),
		"/repos/o/r/pulls/3/comments": fmt.Sprintf(`[
{"body":"rename x","path":"a.go","line":4,"created_at":%q,"user":{"login":"d"}},
{"body":"old inline","path":"a.go","line":1,"created_at":%q,"user":{"login":"d"}}]`, ts(2100), ts(100)),
		"/repos/o/r/issues/3/comments": fmt.Sprintf(`[
{"body":"please add tests","created_at":%q,"user":{"login":"e"}},
{"body":"factory update","created_at":%q,"user":{"login":"x[bot]","type":"Bot"}}]`, ts(2200), ts(2300)),
	})
	out, found, err := buildFeedbackBundle("o", "r", 3, 1000)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	for _, want := range []string{"fix the thing", "rename x", "a.go:4", "please add tests"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	for _, bad := range []string{"old one", "old inline", "factory update"} {
		if strings.Contains(out, bad) {
			t.Errorf("unexpected %q", bad)
		}
	}
	if _, found, _ := buildFeedbackBundle("o", "r", 3, 5000); found {
		t.Fatal("expected no feedback")
	}
}

func TestBuildFeedbackBundleNoTrigger(t *testing.T) {
	stubGh(t, map[string]string{
		"/repos/o/r/pulls/3/reviews":   fmt.Sprintf(`[{"state":"APPROVED","body":"","submitted_at":%q,"user":{"login":"a"}}]`, ts(2000)),
		"/repos/o/r/pulls/3/comments":  `[]`,
		"/repos/o/r/issues/3/comments": `[]`,
	})
	if out, found, err := buildFeedbackBundle("o", "r", 3, 0); found || err != nil || out != "" {
		t.Fatalf("approval only: %q %v %v", out, found, err)
	}
}
