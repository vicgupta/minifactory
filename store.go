package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
)

// Task is one factory work item. Stored as a JSON array in data/queue.json.
type Task struct {
	ID        string  `json:"id"`
	RepoURL   string  `json:"repo_url"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	State     string  `json:"state"`
	Branch    string  `json:"branch"`
	PRURL     string  `json:"pr_url"`
	CreatedAt float64 `json:"created_at"`
	// IssueNumber is the GitHub issue this task came from (0 = manual issue).
	IssueNumber int `json:"issue_number,omitempty"`
	// Children holds the GitHub issue numbers this task was split into
	// (state "split"). sync watches them and completes the parent when
	// all are closed.
	Children []int `json:"children,omitempty"`
}

func storePath() string { return filepath.Join(dataDir, "queue.json") }

// withStore runs fn against the task list under an exclusive flock, then
// persists the result. Single writer in practice; the lock keeps concurrent
// timer cycles from corrupting the file.
func withStore(fn func(tasks []Task) ([]Task, error)) error {
	os.MkdirAll(dataDir, 0755)
	f, err := os.OpenFile(storePath(), os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	var tasks []Task
	if st, _ := f.Stat(); st.Size() > 0 {
		if err := json.NewDecoder(f).Decode(&tasks); err != nil {
			return err
		}
	}
	tasks, err = fn(tasks)
	if err != nil {
		return err
	}
	if tasks == nil {
		tasks = []Task{}
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(tasks); err != nil {
		return err
	}
	return f.Sync()
}

// listTasks returns a snapshot of all tasks.
func listTasks() ([]Task, error) {
	var out []Task
	err := withStore(func(tasks []Task) ([]Task, error) {
		out = tasks
		return tasks, nil
	})
	return out, err
}

// setState updates a task's state (and branch/pr_url when non-empty).
func setState(id, state, branch, prURL string) {
	withStore(func(tasks []Task) ([]Task, error) {
		for i := range tasks {
			if tasks[i].ID == id {
				tasks[i].State = state
				if branch != "" {
					tasks[i].Branch = branch
				}
				if prURL != "" {
					tasks[i].PRURL = prURL
				}
			}
		}
		return tasks, nil
	})
}
