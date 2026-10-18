package main

import (
	"strings"
	"testing"
)

func TestNormalizeRepoURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"short form", "vicgupta/nova-test", "https://github.com/vicgupta/nova-test"},
		{"short form with .git", "vicgupta/nova-test.git", "https://github.com/vicgupta/nova-test"},
		{"short form with spaces", "  vicgupta/nova-test  ", "https://github.com/vicgupta/nova-test"},
		{"full https url", "https://github.com/vicgupta/mf-test", "https://github.com/vicgupta/mf-test"},
		{"full https url with .git", "https://github.com/vicgupta/mf-test.git", "https://github.com/vicgupta/mf-test.git"},
		{"ssh url", "git@github.com:vicgupta/mf-test.git", "git@github.com:vicgupta/mf-test.git"},
		{"file url", "file:///tmp/repo", "file:///tmp/repo"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRepoURL(tc.in); got != tc.want {
				t.Errorf("normalizeRepoURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// parseGithubRepo also accepts the short "owner/repo" form via
// normalizeRepoURL.
func TestParseGithubRepoShortForm(t *testing.T) {
	org, repo, ok := parseGithubRepo("vicgupta/nova-test")
	if !ok || org != "vicgupta" || repo != "nova-test" {
		t.Errorf("parseGithubRepo(short form) = %q, %q, %v; want vicgupta, nova-test, true", org, repo, ok)
	}
}

// The factory label lifecycle names are contractual: poll, issue, and the
// pickup/completion/merge transitions all key off these exact strings.
func TestFactoryLabelNames(t *testing.T) {
	want := map[string]string{
		"labelReady":      "factory-ready",
		"labelInProgress": "factory-inProgress",
		"labelCompleted":  "factory-completed",
		"labelCreatedPR":  "factory-createdPR",
		"labelDone":       "completed",
		"labelRejected":   "factory-rejected",
	}
	got := map[string]string{
		"labelReady":      labelReady,
		"labelInProgress": labelInProgress,
		"labelCompleted":  labelCompleted,
		"labelCreatedPR":  labelCreatedPR,
		"labelDone":       labelDone,
		"labelRejected":   labelRejected,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
		if color, _ := labelMeta(got[k]); color == "" {
			t.Errorf("labelMeta(%q) returned empty color", got[k])
		}
	}
	// The lifecycle order init ensures must match the transition order.
	for i, name := range []string{labelReady, labelInProgress, labelCompleted, labelCreatedPR, labelDone, labelRejected} {
		if factoryLabels[i] != name {
			t.Errorf("factoryLabels[%d] = %q, want %q", i, factoryLabels[i], name)
		}
	}
}

// The normalized URL must trip the token-auth branch in gitAuthArgs,
// otherwise clones of private repos would go out unauthenticated.
func TestGitCloneArgsAuthenticatesNormalizedURL(t *testing.T) {
	old := githubToken
	githubToken = "test-token"
	defer func() { githubToken = old }()

	args := gitCloneArgs("vicgupta/nova-test", "/tmp/workdir")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "https://github.com/vicgupta/nova-test") {
		t.Errorf("clone args missing normalized URL: %q", joined)
	}
	if !strings.Contains(joined, "http.extraHeader=AUTHORIZATION: basic ") {
		t.Errorf("clone args missing auth header (token not attached): %q", joined)
	}
}

func TestBuildPRBody(t *testing.T) {
	task := &Task{Body: "do it", IssueNumber: 42}
	const footer = "Built by minifactory. Tests passed in an isolated sandbox."

	plain := buildPRBody(task, nil, nil)
	if want := "do it\n\n---\n" + footer + "\nCloses #42."; plain != want {
		t.Fatalf("no-handoff body = %q, want %q", plain, want)
	}

	h := &handoff{Summary: "line1\nline2", Agent: "claude", Files: []string{"a.go", "b_test.go"}}
	body := buildPRBody(task, nil, h)
	i, j, k, l := strings.Index(body, "do it"), strings.Index(body, "### Factory handoff"), strings.Index(body, footer), strings.Index(body, "Closes #42.")
	if !(i == 0 && i < j && j < k && k < l) {
		t.Fatalf("bad section order: %q", body)
	}
	for _, w := range []string{"**Summary:** line1\nline2", "**Agent:** claude", "- `a.go`\n", "- `b_test.go`\n"} {
		if !strings.Contains(body, w) {
			t.Errorf("body missing %q", w)
		}
	}

	big := &handoff{Summary: strings.Repeat("x", 5000), Agent: "claude"}
	for n := 0; n < 55; n++ {
		big.Files = append(big.Files, "f")
	}
	b := buildPRBody(task, nil, big)
	if strings.Contains(b, strings.Repeat("x", 2001)) {
		t.Error("summary not capped")
	}
	if got := strings.Count(b, "- `f`"); got != 50 || !strings.Contains(b, "…and 5 more") {
		t.Errorf("files cap wrong: %d bullets", got)
	}
}
