package main

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestAgentCommandIsNonInteractive(t *testing.T) {
	// Every runner must be fully non-interactive: a permission prompt or a
	// stdin read is a hang until the agent timeout, because nobody can
	// answer it in a factory run.
	_, _, args, ok := agentCommand("claude", "do it", 30)
	if !ok {
		t.Fatal("claude: not ok")
	}
	if !slices.Contains(args, "--dangerously-skip-permissions") {
		t.Errorf("claude args %v: missing --dangerously-skip-permissions", args)
	}

	_, _, args, ok = agentCommand("codex", "do it", 30)
	if !ok {
		t.Fatal("codex: not ok")
	}
	for _, want := range []string{"--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox"} {
		if !slices.Contains(args, want) {
			t.Errorf("codex args %v: missing %s", args, want)
		}
	}

	_, _, args, ok = agentCommand("opencode", "do it", 30)
	if !ok {
		t.Fatal("opencode: not ok")
	}
	if !slices.Contains(args, "--auto") {
		t.Errorf("opencode args %v: missing --auto", args)
	}

	if _, _, _, ok = agentCommand("bogus", "do it", 30); ok {
		t.Error("bogus agent name: expected not ok")
	}
}

func TestAgentCommandPromptPassedThrough(t *testing.T) {
	for _, name := range []string{"claude", "codex", "opencode"} {
		_, _, args, ok := agentCommand(name, "the task", 30)
		if !ok {
			t.Fatalf("%s: not ok", name)
		}
		if !slices.Contains(args, "the task") {
			t.Errorf("%s args %v: prompt missing", name, args)
		}
	}
}

func TestAgentEnvDropsSecrets(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/minifactory")
	t.Setenv("GITHUB_TOKEN", "gh-secret")
	t.Setenv("GITHUB_REPO", "vicgupta/minifactory")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "claude-secret")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/ssh-agent.sock")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	os.Unsetenv("TERM")

	env := agentEnv([]string{"CODEX_TOKEN=codex-secret"})
	has := func(prefix string) bool {
		return slices.ContainsFunc(env, func(kv string) bool {
			return strings.HasPrefix(kv, prefix)
		})
	}
	for _, secret := range []string{
		"GITHUB_TOKEN=", "GITHUB_REPO=", "CLAUDE_CODE_OAUTH_TOKEN=",
		"ANTHROPIC_API_KEY=", "SSH_AUTH_SOCK=", "AWS_SECRET_ACCESS_KEY=",
	} {
		if has(secret) {
			t.Errorf("agentEnv leaked %q", secret)
		}
	}
	for _, want := range []string{
		"PATH=/usr/bin:/bin", "HOME=/home/minifactory",
		"TERM=dumb", "CODEX_TOKEN=codex-secret",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("agentEnv missing %q (got %v)", want, env)
		}
	}
}

func TestRunAgentCmdScrubsEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "super-secret")
	t.Setenv("ANTHROPIC_API_KEY", "other-secret")
	rc, out := runAgentCmd(10*time.Second, "", nil, "",
		"sh", "-c", `echo "github=[${GITHUB_TOKEN-unset}]"; echo "anthropic=[${ANTHROPIC_API_KEY-unset}]"; echo "term=[$TERM]"`)
	if rc != 0 {
		t.Fatalf("runAgentCmd failed: rc=%d out=%q", rc, out)
	}
	for _, want := range []string{"github=[unset]", "anthropic=[unset]", "term=[dumb]"} {
		if !strings.Contains(out, want) {
			t.Errorf("scrubbed child env missing %q (out=%q)", want, out)
		}
	}
	if strings.Contains(out, "super-secret") || strings.Contains(out, "other-secret") {
		t.Errorf("secret leaked into child output: %q", out)
	}
}

func TestParseAgentTimeout(t *testing.T) {
	cases := []struct {
		name, in string
		want     time.Duration
	}{
		{"empty uses default", "", defaultAgentTimeout},
		{"45m", "45m", 45 * time.Minute},
		{"90s", "90s", 90 * time.Second},
		{"invalid falls back", "abc", defaultAgentTimeout},
		{"zero falls back", "0", defaultAgentTimeout},
		{"negative falls back", "-5m", defaultAgentTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAgentTimeout(tc.in); got != tc.want {
				t.Errorf("parseAgentTimeout(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
