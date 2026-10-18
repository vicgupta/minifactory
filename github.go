package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// githubAPIBase is the API root; tests point it at an httptest server.
var githubAPIBase = "https://api.github.com"

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
	req, err := http.NewRequest(method, githubAPIBase+path, body)
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
	return runCmd(2*time.Minute, workdir, nil, "", "git", args...)
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
	labelRevise     = "factory-revise"
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
	case labelRevise:
		return "D93F0B", "Reviewer requested changes — minifactory will revise the PR"
	}
	return "CCCCCC", ""
}

// factoryLabels lists every label in the lifecycle, in order.
var factoryLabels = []string{labelReady, labelInProgress, labelCompleted, labelCreatedPR, labelDone, labelRejected, labelRevise}

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

// setIssueLabels replaces an issue's labels with labels. Best-effort:
// failures are logged, never fatal. Silent no-op for manual issues
// (num == 0) or when GitHub isn't configured.
func setIssueLabels(org, repo string, num int, labels []string) error {
	if num == 0 || githubToken == "" {
		return nil
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
	out := []string{}
	for _, l := range cur {
		if seen[l.Name] {
			continue
		}
		seen[l.Name] = true
		out = append(out, l.Name)
	}
	for _, l := range labels {
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	if _, err := ghDo("PUT", "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(num)+"/labels",
		map[string]any{"labels": out}); err != nil {
		return fmt.Errorf("set issue labels: %w", err)
	}
	return nil
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
	if err := setIssueLabels(org, repo, num, labels); err != nil {
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
	if err := setIssueLabels(org, repo, t.IssueNumber, labels); err != nil {
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
	if err := setIssueLabels(org, repo, t.IssueNumber, labels); err != nil {
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

// setTaskIssueState opens or closes the task's GitHub issue, if the task
// came from one. Best-effort: failures are logged, never fatal. Silent
// no-op for manual issues.
func setTaskIssueState(t *Task, state string) {
	if t.IssueNumber == 0 || githubToken == "" {
		return
	}
	org, repo, ok := parseGithubRepo(t.RepoURL)
	if !ok {
		return
	}
	if _, err := ghDo("PATCH", "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(t.IssueNumber),
		map[string]any{"state": state}); err != nil {
		logf(t.ID, "WARNING: set issue state %s failed: %v", state, err)
	}
}

// createPR opens a pull request for branch. Returns the PR URL, or "" when
// the remote isn't GitHub or no token is configured (branch is pushed anyway).
// When the run record is flagged over budget, the PR body carries a warning.
func createPR(t *Task, branch string, rec *RunRecord) string {
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
	if rec != nil && rec.OverBudget {
		prBody += "\n\u26a0\ufe0f Over budget: this run's reported model cost exceeded MAX_COST_USD_PER_TASK."
	}
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

// ghUser is the user object embedded in GitHub review/comment payloads.
type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// PRReview is a submitted pull-request review.
type PRReview struct {
	ID    int64  `json:"id"`
	User  ghUser `json:"user"`
	State string `json:"state"`
	Body  string `json:"body"`
}

// PRReviewComment is an inline review comment on a diff.
type PRReviewComment struct {
	ID       int64  `json:"id"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	DiffHunk string `json:"diff_hunk"`
	Body     string `json:"body"`
	User     ghUser `json:"user"`
}

// IssueComment is a conversation comment on an issue or PR.
type IssueComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User ghUser `json:"user"`
}

// isBotUser reports whether u is a GitHub bot account.
func isBotUser(u ghUser) bool {
	return u.Type == "Bot" || strings.HasSuffix(strings.ToLower(u.Login), "[bot]")
}

// isFactoryBody reports whether body is one of the factory's own comments.
func isFactoryBody(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), "minifactory:")
}

func prPath(org, repo string, num int, tail string) string {
	return "/repos/" + org + "/" + repo + "/" + tail + "/" + strconv.Itoa(num)
}

// getPRLabels returns the label names on a PR/issue.
func getPRLabels(org, repo string, num int) ([]string, error) {
	raw, err := ghDo("GET", prPath(org, repo, num, "issues")+"/labels?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	var ls []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, fmt.Errorf("parse labels: %w", err)
	}
	names := []string{}
	for _, l := range ls {
		names = append(names, l.Name)
	}
	return names, nil
}

// getPRReviews returns human reviews (bots and factory reviews dropped).
func getPRReviews(org, repo string, num int) ([]PRReview, error) {
	raw, err := ghDo("GET", prPath(org, repo, num, "pulls")+"/reviews?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	var all []PRReview
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("parse reviews: %w", err)
	}
	out := []PRReview{}
	for _, r := range all {
		if !isBotUser(r.User) && !isFactoryBody(r.Body) {
			out = append(out, r)
		}
	}
	return out, nil
}

// getPRReviewComments returns human inline review comments.
func getPRReviewComments(org, repo string, num int) ([]PRReviewComment, error) {
	raw, err := ghDo("GET", prPath(org, repo, num, "pulls")+"/comments?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	var all []PRReviewComment
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("parse review comments: %w", err)
	}
	out := []PRReviewComment{}
	for _, c := range all {
		if !isBotUser(c.User) && !isFactoryBody(c.Body) {
			out = append(out, c)
		}
	}
	return out, nil
}

// getIssueComments returns human issue/PR conversation comments.
func getIssueComments(org, repo string, num int) ([]IssueComment, error) {
	raw, err := ghDo("GET", prPath(org, repo, num, "issues")+"/comments?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	var all []IssueComment
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("parse issue comments: %w", err)
	}
	out := []IssueComment{}
	for _, c := range all {
		if !isBotUser(c.User) && !isFactoryBody(c.Body) {
			out = append(out, c)
		}
	}
	return out, nil
}

// postPRComment posts a conversation comment on a PR.
func postPRComment(org, repo string, num int, body string) error {
	return postIssueComment(org, repo, num, body)
}

// removeIssueLabel removes a label; a 404 (label not on the issue) is fine.
func removeIssueLabel(org, repo string, num int, label string) error {
	_, err := ghDo("DELETE", prPath(org, repo, num, "issues")+"/labels/"+url.PathEscape(label), nil)
	if err != nil && strings.Contains(err.Error(), "-> 404") {
		return nil
	}
	return err
}
