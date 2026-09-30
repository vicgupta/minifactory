package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// ghDo calls the GitHub REST API with the Bearer token. Returns the raw body.
func ghDo(method, path string, data any) ([]byte, error) {
	if githubToken == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN not set")
	}
	var body io.Reader
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://api.github.com"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "minifactory")
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github api %s %s -> %d: %s", method, path, resp.StatusCode, tailStr(string(rb), 500))
	}
	return rb, nil
}

// parseGithubRepo extracts org/repo from an https or ssh github remote URL.
func parseGithubRepo(repoURL string) (org, repo string, ok bool) {
	u := strings.TrimSpace(normalizeRepoURL(repoURL))
	switch {
	case strings.HasPrefix(u, "git@github.com:"):
		u = strings.TrimPrefix(u, "git@github.com:")
	case strings.Contains(u, "github.com/"):
		u = u[strings.Index(u, "github.com/")+len("github.com/"):]
	default:
		return "", "", false
	}
	u = strings.Trim(strings.TrimSuffix(u, ".git"), "/")
	parts := strings.Split(u, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// gitAuthArgs returns the git -c extraHeader args that authenticate GitHub
// HTTPS operations with the token (host-side only, never logged).
func gitAuthArgs(repoURL string) []string {
	if githubToken != "" && strings.HasPrefix(repoURL, "https://github.com") {
		cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + githubToken))
		return []string{"-c", "http.extraHeader=AUTHORIZATION: basic " + cred}
	}
	return nil
}

// gitPush pushes branch to origin. The token travels via gitAuthArgs
// (host-side only) and is never logged.
func gitPush(workdir, branch, repoURL string) (int, string) {
	args := append([]string{}, gitAuthArgs(normalizeRepoURL(repoURL))...)
	args = append(args, "push", "origin", branch)
	return runCmd(2*time.Minute, workdir, nil, "git", args...)
}

// normalizeRepoURL accepts the forms a user might pass to `issue --repo`
// and returns a full clone URL. "owner/repo" becomes
// "https://github.com/owner/repo"; full URLs (https://, git@, file://)
// pass through unchanged.
func normalizeRepoURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if strings.Contains(s, "://") || strings.HasPrefix(s, "git@") {
		return s
	}
	return "https://github.com/" + strings.TrimSuffix(s, ".git")
}

// gitCloneArgs returns the authenticated clone command parts for repoURL.
// The -c flag must come BEFORE the subcommand: `git clone -c ...` would
// persist the header into the new repo's config, causing duplicate
// Authorization headers on later push.
func gitCloneArgs(repoURL, workdir string) []string {
	repoURL = normalizeRepoURL(repoURL)
	args := append([]string{}, gitAuthArgs(repoURL)...)
	args = append(args, "clone", "--depth", "1", repoURL, workdir)
	return args
}

// postIssueComment posts a comment on a GitHub issue.
func postIssueComment(org, repo string, num int, body string) error {
	_, err := ghDo("POST", "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(num)+"/comments",
		map[string]any{"body": body})
	return err
}

// ensureLabel creates a repo label if it doesn't exist yet.
// A 422 from GitHub means it's already there — not an error.
func ensureLabel(org, repo, name, color, description string) error {
	_, err := ghDo("POST", "/repos/"+org+"/"+repo+"/labels",
		map[string]any{"name": name, "color": color, "description": description})
	if err != nil && strings.Contains(err.Error(), "-> 422") {
		return nil
	}
	return err
}

// Factory issue-label lifecycle:
//
//	factory-ready -> factory-inProgress -> factory-completed
//	-> factory-createdPR -> completed
//
// A PR closed without merging moves the issue to factory-rejected instead.
// `poll` only picks up factory-ready issues. `completed`/`factory-rejected`
// are applied by `sync` once the PR is merged/closed.
const (
	labelReady      = "factory-ready"
	labelInProgress = "factory-inProgress"
	labelCompleted  = "factory-completed"
	labelCreatedPR  = "factory-createdPR"
	labelDone       = "completed"
	labelRejected   = "factory-rejected"
)

// labelMeta returns the color and description for a factory label.
func labelMeta(name string) (color, description string) {
	switch name {
	case labelReady:
		return "0E8A16", "Ready for minifactory to pick up"
	case labelInProgress:
		return "FBCA04", "Being worked on by minifactory"
	case labelCompleted:
		return "5319E7", "Minifactory finished the work — branch pushed"
	case labelCreatedPR:
		return "1D76DB", "Minifactory opened a PR — awaiting human review and merge"
	case labelDone:
		return "1A7F37", "PR merged — work complete"
	case labelRejected:
		return "B60205", "PR closed without merging — work rejected"
	}
	return "CCCCCC", ""
}

// factoryLabels lists every label in the lifecycle, in order.
var factoryLabels = []string{labelReady, labelInProgress, labelCompleted, labelCreatedPR, labelDone, labelRejected}

// ensureFactoryLabels creates the factory's label set on the repo when
// missing. Creating an existing label 422s, which ensureLabel already
// treats as success. Invoked by `init`.
func ensureFactoryLabels(org, repo string) error {
	for _, name := range factoryLabels {
		color, desc := labelMeta(name)
		if err := ensureLabel(org, repo, name, color, desc); err != nil {
			return fmt.Errorf("label %s: %w", name, err)
		}
	}
	return nil
}

// createIssue opens a GitHub issue on org/repo and returns its number.
func createIssue(org, repo, title, body string) (int, error) {
	raw, err := ghDo("POST", "/repos/"+org+"/"+repo+"/issues",
		map[string]any{"title": title, "body": body})
	if err != nil {
		return 0, err
	}
	var is struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(raw, &is); err != nil {
		return 0, fmt.Errorf("parse created issue: %w", err)
	}
	if is.Number == 0 {
		return 0, fmt.Errorf("GitHub returned no issue number")
	}
	return is.Number, nil
}

// addIssueLabel adds label to an issue, preserving its other labels. The
// label is created on the repo first when missing. Used by `issue --issue`.
func addIssueLabel(repoURL string, num int, label string) error {
	org, repo, ok := parseGithubRepo(repoURL)
	if !ok {
		return fmt.Errorf("cannot parse repo %q", repoURL)
	}
	color, desc := labelMeta(label)
	if err := ensureLabel(org, repo, label, color, desc); err != nil {
		return fmt.Errorf("ensure label: %w", err)
	}
	raw, err := ghDo("GET", "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(num)+"/labels", nil)
	if err != nil {
		return fmt.Errorf("read issue labels: %w", err)
	}
	var cur []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &cur); err != nil {
		return fmt.Errorf("parse issue labels: %w", err)
	}
	seen := map[string]bool{}
	labels := []string{}
	for _, l := range cur {
		if !seen[l.Name] {
			seen[l.Name] = true
			labels = append(labels, l.Name)
		}
	}
	if !seen[label] {
		labels = append(labels, label)
	}
	if _, err := ghDo("PUT", "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(num)+"/labels",
		map[string]any{"labels": labels}); err != nil {
		return fmt.Errorf("set issue labels: %w", err)
	}
	return nil
}

// swapIssueLabel replaces one label with another on the task's GitHub issue,
// creating the new label first if needed. Any other labels on the issue are
// preserved. Best-effort: failures are logged, never fatal. Silent no-op
// for manual issues.
func swapIssueLabel(t *Task, remove, add, color, description string) {
	if t.IssueNumber == 0 || githubToken == "" {
		return
	}
	org, repo, ok := parseGithubRepo(t.RepoURL)
	if !ok {
		return
	}
	if err := ensureLabel(org, repo, add, color, description); err != nil {
		logf(t.ID, "WARNING: ensure %s label failed: %v", add, err)
		return
	}
	raw, err := ghDo("GET", "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(t.IssueNumber)+"/labels", nil)
	if err != nil {
		logf(t.ID, "WARNING: read issue labels failed: %v", err)
		return
	}
	var cur []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &cur); err != nil {
		logf(t.ID, "WARNING: parse issue labels failed: %v", err)
		return
	}
	seen := map[string]bool{}
	var labels []string
	for _, l := range cur {
		if l.Name == remove || seen[l.Name] {
			continue
		}
		seen[l.Name] = true
		labels = append(labels, l.Name)
	}
	if !seen[add] {
		labels = append(labels, add)
	}
	if _, err := ghDo("PUT", "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(t.IssueNumber)+"/labels",
		map[string]any{"labels": labels}); err != nil {
		logf(t.ID, "WARNING: replace issue labels failed: %v", err)
	}
}

// labelIssueInProgress swaps the task's factory-ready label for factory-inProgress.
func labelIssueInProgress(t *Task) {
	color, desc := labelMeta(labelInProgress)
	swapIssueLabel(t, labelReady, labelInProgress, color, desc)
}

// labelIssueCompleted swaps the task's factory-inProgress label for
// factory-completed: the factory's work is done (tests passed, branch pushed).
func labelIssueCompleted(t *Task) {
	color, desc := labelMeta(labelCompleted)
	swapIssueLabel(t, labelInProgress, labelCompleted, color, desc)
}

// labelIssueCreatedPR swaps the task's factory-completed label for
// factory-createdPR: the PR is open and awaiting human review/merge.
func labelIssueCreatedPR(t *Task) {
	color, desc := labelMeta(labelCreatedPR)
	swapIssueLabel(t, labelCompleted, labelCreatedPR, color, desc)
}

// labelIssueMerged swaps the task's factory-createdPR label for completed:
// the PR was merged. Applied by `sync`.
func labelIssueMerged(t *Task) {
	color, desc := labelMeta(labelDone)
	swapIssueLabel(t, labelCreatedPR, labelDone, color, desc)
}

// labelIssueRejected swaps the task's factory-createdPR label for
// factory-rejected: the PR was closed without merging. Applied by `sync`.
func labelIssueRejected(t *Task) {
	color, desc := labelMeta(labelRejected)
	swapIssueLabel(t, labelCreatedPR, labelRejected, color, desc)
}

// labelIssueReady resets the task's issue to the start of the lifecycle:
// every factory lifecycle label is removed and factory-ready is added.
// Used by `retry` when a task is re-queued from scratch (this also clears a
// stale factory-rejected / factory-inProgress label). Best-effort: failures
// are logged, never fatal. Silent no-op for manual issues.
func labelIssueReady(t *Task) {
	if t.IssueNumber == 0 || githubToken == "" {
		return
	}
	org, repo, ok := parseGithubRepo(t.RepoURL)
	if !ok {
		return
	}
	color, desc := labelMeta(labelReady)
	if err := ensureLabel(org, repo, labelReady, color, desc); err != nil {
		logf(t.ID, "WARNING: ensure %s label failed: %v", labelReady, err)
		return
	}
	raw, err := ghDo("GET", "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(t.IssueNumber)+"/labels", nil)
	if err != nil {
		logf(t.ID, "WARNING: read issue labels failed: %v", err)
		return
	}
	var cur []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &cur); err != nil {
		logf(t.ID, "WARNING: parse issue labels failed: %v", err)
		return
	}
	lifecycle := map[string]bool{}
	for _, l := range factoryLabels {
		lifecycle[l] = true
	}
	seen := map[string]bool{}
	var labels []string
	for _, l := range cur {
		if lifecycle[l.Name] || seen[l.Name] {
			continue
		}
		seen[l.Name] = true
		labels = append(labels, l.Name)
	}
	if !seen[labelReady] {
		labels = append(labels, labelReady)
	}
	if _, err := ghDo("PUT", "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(t.IssueNumber)+"/labels",
		map[string]any{"labels": labels}); err != nil {
		logf(t.ID, "WARNING: reset issue labels failed: %v", err)
	}
}

// commentOnIssue posts a status comment on the task's GitHub issue, if the
// task came from one. Best-effort: failures are logged, never fatal.
// Silent no-op for manual issues.
func commentOnIssue(t *Task, body string) {
	if t.IssueNumber == 0 || githubToken == "" {
		return
	}
	org, repo, ok := parseGithubRepo(t.RepoURL)
	if !ok {
		return
	}
	if err := postIssueComment(org, repo, t.IssueNumber, body); err != nil {
		logf(t.ID, "WARNING: issue comment failed: %v", err)
	}
}

// createPR opens a pull request for branch. Returns the PR URL, or "" when
// the remote isn't GitHub or no token is configured (branch is pushed anyway).
func createPR(t *Task, branch string) string {
	org, repo, ok := parseGithubRepo(t.RepoURL)
	if !ok || githubToken == "" {
		logf(t.ID, "WARNING: no GitHub remote/token — branch pushed, open PR manually")
		return ""
	}
	ib, err := ghDo("GET", "/repos/"+org+"/"+repo, nil)
	if err != nil {
		logf(t.ID, "WARNING: PR create failed (%v); branch pushed, open PR manually", err)
		return ""
	}
	var info map[string]any
	json.Unmarshal(ib, &info)
	base, _ := info["default_branch"].(string)
	if base == "" {
		base = "main"
	}
	prBody := t.Body + "\n\n---\nBuilt by minifactory. Tests passed in an isolated sandbox."
	if t.IssueNumber != 0 {
		prBody += "\nCloses #" + strconv.Itoa(t.IssueNumber) + "."
	}
	pb, err := ghDo("POST", "/repos/"+org+"/"+repo+"/pulls", map[string]any{
		"title": t.Title,
		"head":  branch,
		"base":  base,
		"body":  prBody,
	})
	if err != nil {
		logf(t.ID, "WARNING: PR create failed (%v); branch pushed, open PR manually", err)
		return ""
	}
	var pr map[string]any
	json.Unmarshal(pb, &pr)
	u, _ := pr["html_url"].(string)
	logf(t.ID, "PR opened: %s", u)
	return u
}
