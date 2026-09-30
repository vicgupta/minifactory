package main

import "testing"

func TestParseMaxTurns(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty uses default", "", defaultMaxTurns},
		{"whitespace uses default", "   ", defaultMaxTurns},
		{"valid value", "80", 80},
		{"valid value with spaces", " 60 ", 60},
		{"one is allowed", "1", 1},
		{"zero falls back", "0", defaultMaxTurns},
		{"negative falls back", "-5", defaultMaxTurns},
		{"non-numeric falls back", "many", defaultMaxTurns},
		{"float falls back", "30.5", defaultMaxTurns},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseMaxTurns(tc.in); got != tc.want {
				t.Errorf("parseMaxTurns(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseSizeVerdict(t *testing.T) {
	// fit
	if sv := parseSizeVerdict([]byte(`{"verdict":"fit","reason":"small"}`)); sv == nil || sv.Verdict != "fit" {
		t.Errorf("fit verdict rejected: %+v", sv)
	}
	// split with 3 valid subtasks
	split := `{"verdict":"split","reason":"too big","subtasks":[
		{"title":"one","body":"do one"},
		{"title":"two","body":"do two"},
		{"title":"three","body":"do three"}]}`
	sv := parseSizeVerdict([]byte(split))
	if sv == nil || sv.Verdict != "split" || len(sv.Subtasks) != 3 {
		t.Fatalf("valid split rejected: %+v", sv)
	}
	// verdict is case/space tolerant (with valid subtasks)
	if sv := parseSizeVerdict([]byte(`{"verdict":" Split ","subtasks":[{"title":"a","body":"b"},{"title":"c","body":"d"}]}`)); sv == nil || sv.Verdict != "split" {
		t.Error("padded/cased split verdict should parse")
	}
	// rejects: garbage, unknown verdict, missing verdict
	for _, bad := range []string{
		`not json`,
		`{"verdict":"maybe"}`,
		`{"reason":"no verdict"}`,
		`{"verdict":"split","subtasks":[]}`,
		`{"verdict":"split","subtasks":[{"title":"only one","body":"x"}]}`,
		`{"verdict":"split","subtasks":[{"title":"","body":"x"},{"title":"y","body":"y"}]}`,
		`{"verdict":"split","subtasks":[{"title":"y","body":""},{"title":"z","body":"z"}]}`,
	} {
		if got := parseSizeVerdict([]byte(bad)); got != nil {
			t.Errorf("expected nil for %q, got %+v", bad, got)
		}
	}
	// rejects: more than 8 subtasks
	many := `{"verdict":"split","subtasks":[`
	for i := 0; i < 9; i++ {
		if i > 0 {
			many += ","
		}
		many += `{"title":"t","body":"b"}`
	}
	many += `]}`
	if got := parseSizeVerdict([]byte(many)); got != nil {
		t.Errorf("expected nil for 9 subtasks, got %+v", got)
	}
}
