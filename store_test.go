package main

import (
	"os"
	"testing"
)

func TestStoreOldFormatAndRevisionFields(t *testing.T) {
	defer testDirs(t)()
	os.MkdirAll(dataDir, 0755)
	old := `[{"id":"a","repo_url":"r","title":"t","body":"b","state":"pr_open","branch":"b","pr_url":"u","created_at":1}]`
	if err := os.WriteFile(storePath(), []byte(old), 0644); err != nil {
		t.Fatal(err)
	}
	var got Task
	withStore(func(ts []Task) ([]Task, error) { got = ts[0]; return ts, nil })
	if got.ID != "a" || got.Revisions != 0 || got.LastPushSHA != "" || got.LastPushAt != 0 {
		t.Fatalf("bad migration: %+v", got)
	}
	withStore(func(ts []Task) ([]Task, error) {
		ts[0].Revisions, ts[0].LastPushSHA, ts[0].LastPushAt, ts[0].State = 1, "abc", 12.5, stateRevising
		return ts, nil
	})
	withStore(func(ts []Task) ([]Task, error) { got = ts[0]; return ts, nil })
	if got.Revisions != 1 || got.LastPushSHA != "abc" || got.LastPushAt != 12.5 || got.State != "revising" {
		t.Fatalf("round trip: %+v", got)
	}
	if _, err := parseStateFilter("revising"); err != nil {
		t.Fatal(err)
	}
}
