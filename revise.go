package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	maxRevisions = 3
	labelRevise  = "factory-revise"
)

// detectReviseTrigger reports whether the PR carries the revise label and,
// if so, returns the feedback bundle built from its comments.
func detectReviseTrigger(org, repo, num string) (bool, string, error) {
	base := "/repos/" + org + "/" + repo
	raw, err := ghDo("GET", base+"/issues/"+num+"/labels", nil)
	if err != nil {
		return false, "", err
	}
	var labels []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &labels); err != nil {
		return false, "", err
	}
	found := false
	for _, l := range labels {
		if l.Name == labelRevise {
			found = true
		}
	}
	if !found {
		return false, "", nil
	}
	return true, buildFeedbackBundle(base, num), nil
}

// buildFeedbackBundle collects human review and issue comments on the PR.
// Best-effort: fetch errors just yield a shorter bundle.
func buildFeedbackBundle(base, num string) string {
	var parts []string
	for _, src := range []struct{ path, label string }{
		{"/pulls/" + num + "/comments", "review comment"},
		{"/issues/" + num + "/comments", "comment"},
	} {
		raw, err := ghDo("GET", base+src.path, nil)
		if err != nil {
			continue
		}
		var cs []struct {
			Body string `json:"body"`
			Path string `json:"path"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		json.Unmarshal(raw, &cs)
		for _, c := range cs {
			if strings.HasPrefix(c.Body, "minifactory:") || strings.TrimSpace(c.Body) == "" {
				continue
			}
			loc := ""
			if c.Path != "" {
				loc = " on " + c.Path
			}
			parts = append(parts, fmt.Sprintf("- %s by %s%s: %s", src.label, c.User.Login, loc, strings.TrimSpace(c.Body)))
		}
	}
	return strings.Join(parts, "\n")
}

// removePRLabel drops the revise label so the trigger is consumed once.
func removePRLabel(org, repo, num, label string) {
	ghDo("DELETE", "/repos/"+org+"/"+repo+"/issues/"+num+"/labels/"+label, nil)
}

// applyRevise handles a detected trigger on a pr_open task: start a revision
// below the cap, otherwise post a single "limit reached" comment. The label
// is removed either way so it is consumed once.
func applyRevise(t *Task, org, repo, num, bundle string) bool {
	started, notify := startRevision(t, bundle)
	if num != "" && githubToken != "" {
		removePRLabel(org, repo, num, labelRevise)
		if n, err := strconv.Atoi(num); err == nil && notify {
			postIssueComment(org, repo, n, "minifactory: revision limit reached, needs a human")
		}
	}
	return started
}

// startRevision stores the bundle and moves the task to revising when under
// the cap. At the cap it reports notify=true only the first time.
func startRevision(t *Task, bundle string) (started, notify bool) {
	id := t.ID
	withStore(func(ts []Task) ([]Task, error) {
		for i := range ts {
			if ts[i].ID != id {
				continue
			}
			if ts[i].Revisions >= maxRevisions {
				notify = !ts[i].RevisionCapNotified
				ts[i].RevisionCapNotified = true
				continue
			}
			ts[i].Revisions++
			ts[i].Feedback = bundle
			ts[i].State = "revising"
			started = true
			t.Revisions, t.Feedback, t.State = ts[i].Revisions, bundle, "revising"
		}
		return ts, nil
	})
	return
}

func cmdRevise(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "revise requires an id")
		return 2
	}
	tasks, err := listTasks()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for i := range tasks {
		t := &tasks[i]
		if t.ID != args[0] {
			continue
		}
		if t.State != "pr_open" {
			fmt.Fprintf(os.Stderr, "task %s is %s, not pr_open\n", t.ID, t.State)
			return 1
		}
		org, repo, ok := parseGithubRepo(t.RepoURL)
		num := ""
		bundle := ""
		if ok && t.PRURL != "" {
			num = strings.TrimSuffix(t.PRURL, "/")
			num = num[strings.LastIndex(num, "/")+1:]
			if githubToken != "" {
				bundle = buildFeedbackBundle("/repos/"+org+"/"+repo, num)
			}
		}
		if !applyRevise(t, org, repo, num, bundle) {
			fmt.Fprintf(os.Stderr, "revision limit reached for %s (%d/%d), needs a human\n", t.ID, t.Revisions, maxRevisions)
			return 1
		}
		fmt.Printf("revising %s (rev %d/%d)\n", t.ID, t.Revisions, maxRevisions)
		return 0
	}
	fmt.Fprintln(os.Stderr, "no such task: "+args[0])
	return 1
}
