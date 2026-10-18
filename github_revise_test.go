package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func withGH(t *testing.T, h http.HandlerFunc) {
	srv := httptest.NewServer(h)
	oldB, oldT := githubAPIBase, githubToken
	githubAPIBase, githubToken = srv.URL, "tok"
	t.Cleanup(func() { srv.Close(); githubAPIBase, githubToken = oldB, oldT })
}

func TestReviseLabelMeta(t *testing.T) {
	if c, d := labelMeta(labelRevise); c == "CCCCCC" || d == "" {
		t.Errorf("labelMeta(revise) = %q,%q", c, d)
	}
	found := false
	for _, n := range factoryLabels {
		found = found || n == labelRevise
	}
	if !found {
		t.Error("factoryLabels missing factory-revise")
	}
	var posted []string
	withGH(t, func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		posted = append(posted, m["name"].(string))
	})
	if err := ensureFactoryLabels("o", "r"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(posted, factoryLabels) {
		t.Errorf("posted %v", posted)
	}
}

func TestGetPRLabelsAndReads(t *testing.T) {
	withGH(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/5/labels":
			w.Write([]byte(`[{"name":"a"},{"name":"factory-revise"}]`))
		case "/repos/o/r/pulls/5/reviews":
			w.Write([]byte(`[{"id":1,"user":{"login":"bob","type":"User"},"state":"CHANGES_REQUESTED","body":"fix"},
{"id":2,"user":{"login":"x[bot]","type":"Bot"},"state":"COMMENTED","body":"b"},
{"id":3,"user":{"login":"me","type":"User"},"state":"COMMENTED","body":"minifactory: hi"}]`))
		case "/repos/o/r/pulls/5/comments":
			w.Write([]byte(`[{"id":1,"path":"a.go","line":3,"diff_hunk":"@@","body":"nit","user":{"login":"bob","type":"User"}},
{"id":2,"path":"a.go","line":4,"body":"z","user":{"login":"ci[bot]","type":"User"}}]`))
		case "/repos/o/r/issues/5/comments":
			w.Write([]byte(`[{"id":1,"body":"human","user":{"login":"bob","type":"User"}},
{"id":2,"body":"bot","user":{"login":"d","type":"Bot"}},
{"id":3,"body":"minifactory: done","user":{"login":"bob","type":"User"}}]`))
		default:
			http.NotFound(w, r)
		}
	})
	if l, err := getPRLabels("o", "r", 5); err != nil || !reflect.DeepEqual(l, []string{"a", "factory-revise"}) {
		t.Errorf("labels %v %v", l, err)
	}
	rv, err := getPRReviews("o", "r", 5)
	if err != nil || len(rv) != 1 || rv[0].State != "CHANGES_REQUESTED" || rv[0].User.Login != "bob" || rv[0].Body != "fix" {
		t.Errorf("reviews %+v %v", rv, err)
	}
	rc, err := getPRReviewComments("o", "r", 5)
	if err != nil || len(rc) != 1 || rc[0].Path != "a.go" || rc[0].Line != 3 || rc[0].DiffHunk != "@@" {
		t.Errorf("review comments %+v %v", rc, err)
	}
	ic, err := getIssueComments("o", "r", 5)
	if err != nil || len(ic) != 1 || ic[0].Body != "human" {
		t.Errorf("issue comments %+v %v", ic, err)
	}
}

func TestPostCommentAndRemoveLabel(t *testing.T) {
	var method, path string
	status := 200
	withGH(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.EscapedPath()
		w.WriteHeader(status)
		w.Write([]byte(`{}`))
	})
	if err := postPRComment("o", "r", 5, "hi"); err != nil || method != "POST" || path != "/repos/o/r/issues/5/comments" {
		t.Errorf("post: %v %s %s", err, method, path)
	}
	if err := removeIssueLabel("o", "r", 5, "a b/c"); err != nil || method != "DELETE" || path != "/repos/o/r/issues/5/labels/a%20b%2Fc" {
		t.Errorf("remove: %v %s %s", err, method, path)
	}
	status = 404
	if err := removeIssueLabel("o", "r", 5, "x"); err != nil {
		t.Errorf("404 should be tolerated: %v", err)
	}
	status = 500
	if err := removeIssueLabel("o", "r", 5, "x"); err == nil {
		t.Error("500 should error")
	}
}
