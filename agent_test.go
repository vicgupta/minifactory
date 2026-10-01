package main

import (
	"strings"
	"testing"
)

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

func TestSelectAgent(t *testing.T) {
	oldChoice, oldClaude, oldCodex, oldOpencode := agentChoice, claudeToken, codexToken, opencodeToken
	defer func() {
		agentChoice, claudeToken, codexToken, opencodeToken = oldChoice, oldClaude, oldCodex, oldOpencode
	}()

	set := func(choice, claude, codex, opencode string) {
		agentChoice, claudeToken, codexToken, opencodeToken = choice, claude, codex, opencode
	}
	cases := []struct {
		name                        string
		choice, claude, codex, open string
		want                        string
	}{
		{"no choice, all tokens: claude wins", "", "c", "x", "o", "claude"},
		{"no choice: codex next", "", "", "x", "o", "codex"},
		{"no choice: opencode next", "", "", "", "o", "opencode"},
		{"no choice, no tokens: stub", "", "", "", "", "stub"},
		{"AGENT=opencode beats claude token", "opencode", "c", "x", "o", "opencode"},
		{"AGENT=codex beats claude token", "codex", "c", "x", "o", "codex"},
		{"AGENT=stub forces stub with tokens set", "stub", "c", "x", "o", "stub"},
		{"AGENT=opencode without its token falls back", "opencode", "c", "x", "", "claude"},
		{"AGENT=claude without its token falls back", "claude", "", "x", "o", "codex"},
		{"unknown AGENT falls back", "bogus", "c", "x", "o", "claude"},
		{"AGENT is case-insensitive", "OpenCode", "", "", "o", "opencode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// agentChoice is lowercased at load time in main(); mirror that here.
			set(tc.choice, tc.claude, tc.codex, tc.open)
			agentChoice = strings.ToLower(strings.TrimSpace(agentChoice))
			if got := selectAgent(); got != tc.want {
				t.Errorf("selectAgent() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAgentSelectionReason(t *testing.T) {
	oldChoice, oldOpencode := agentChoice, opencodeToken
	defer func() { agentChoice, opencodeToken = oldChoice, oldOpencode }()

	agentChoice, opencodeToken = "opencode", "o"
	if name, how := agentSelection(); name != "opencode" || how != "AGENT=opencode override" {
		t.Errorf("agentSelection() = (%q, %q)", name, how)
	}
	agentChoice, opencodeToken = "opencode", ""
	if _, how := agentSelection(); how != "token priority (AGENT=opencode unusable)" {
		t.Errorf("agentSelection() how = %q", how)
	}
	agentChoice = ""
	if _, how := agentSelection(); how != "token priority" {
		t.Errorf("agentSelection() how = %q", how)
	}
}

func TestLooksLikeAuthError(t *testing.T) {
	authy := []string{
		`"api_error_status":401,"result":"Failed to authenticate. API Error: 401 Invalid Bearer <redacted>"`,
		`ERROR: unexpected status 401 Unauthorized: Missing Bearer <redacted> basic authentication in header`,
		`Error: Incorrect API key provided`,
		`{"type":"invalid_api_key"}`,
		`401 Unauthorized`,
	}
	for _, s := range authy {
		if !looksLikeAuthError(s) {
			t.Errorf("looksLikeAuthError(%q) = false, want true", firstLine(s, 60))
		}
	}
	notAuth := []string{
		``,
		`ok`,
		`{"changed_files":["a"],"tests_passed":true,"summary":"done"}`,
		`error: build failed: undefined: foo`,
		`Error 404: repository not found`,
	}
	for _, s := range notAuth {
		if looksLikeAuthError(s) {
			t.Errorf("looksLikeAuthError(%q) = true, want false", firstLine(s, 60))
		}
	}
}

func TestTokenShapeOK(t *testing.T) {
	good := func(kind, tok string) {
		if ok, hint := tokenShapeOK(kind, tok); !ok {
			t.Errorf("tokenShapeOK(%q, <good>) = false (%s)", kind, hint)
		}
	}
	bad := func(kind, tok string) {
		if ok, _ := tokenShapeOK(kind, tok); ok {
			t.Errorf("tokenShapeOK(%q, %q) = true, want false", kind, tok)
		}
	}
	good("claude", "sk-ant-oat01-"+strings.Repeat("a", 100))
	good("codex", "sk-"+strings.Repeat("b", 48))
	good("codex", "sk-proj-"+strings.Repeat("c", 40))
	good("opencode", "some-long-enough-token")
	bad("claude", "short-stub")
	bad("claude", strings.Repeat("x", 100)) // no prefix
	bad("codex", "short-stub")
	bad("codex", strings.Repeat("y", 50)) // no sk- prefix
	bad("opencode", "tiny")
}

func TestCodexLoginMatches(t *testing.T) {
	tok := "sk-test-bogus-key-12345"
	if !codexLoginMatches("Logged in using an API key - sk-test-***12345", tok) {
		t.Error("masked matching key not recognized")
	}
	for name, out := range map[string]string{
		"not logged in":   "Not logged in",
		"different key":   "Logged in using an API key - sk-live-***99999",
		"chatgpt session": "Logged in using ChatGPT - user@example.com",
		"empty":           "",
	} {
		if codexLoginMatches(out, tok) {
			t.Errorf("codexLoginMatches(%q) = true, want false", name)
		}
	}
	if codexLoginMatches("Logged in using an API key - sk-test-***12345", "short") {
		t.Error("short token should never match")
	}
}
