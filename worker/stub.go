// In-sandbox stub agent for minifactory. Runs INSIDE the worker container.
//
// It:
//  1. reads /work/task.json,
//  2. makes a REAL small code change (implements the contract the repo's
//     failing test expects: factory_addon.greet),
//  3. runs the repo's pytest suite,
//  4. writes /work/result.json with {changed_files, tests_passed, summary}.
//
// The host validates result.json; anything malformed or failing -> task failed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var work = "/work"

// runTests runs the repo test suite; overridable in unit tests.
var runTests = func() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-m", "pytest", "-q")
	cmd.Dir = work
	return cmd.CombinedOutput()
}

func last(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func run() int {
	tb, err := os.ReadFile(filepath.Join(work, "task.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "task.json:", err)
		return 1
	}
	var task map[string]string
	if err := json.Unmarshal(tb, &task); err != nil {
		fmt.Fprintln(os.Stderr, "task.json:", err)
		return 1
	}

	changed := []string{}
	addon := filepath.Join(work, "factory_addon.py")
	if _, err := os.Stat(addon); os.IsNotExist(err) {
		code := "def greet(name):\n    return f\"hello {name} from factory\"\n"
		if err := os.WriteFile(addon, []byte(code), 0644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		changed = append(changed, "factory_addon.py")
	}
	// The stub brings its own test so the pipeline is exercisable even on
	// repos that have no test suite of their own.
	testf := filepath.Join(work, "test_addon.py")
	if _, err := os.Stat(testf); os.IsNotExist(err) {
		tcode := "from factory_addon import greet\n\n\ndef test_greet():\n    assert greet(\"factory\") == \"hello factory from factory\"\n"
		if err := os.WriteFile(testf, []byte(tcode), 0644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		changed = append(changed, "test_addon.py")
	}

	revision := false
	if rb, rerr := os.ReadFile(filepath.Join(work, "review.json")); rerr == nil {
		revision = true
		revf := filepath.Join(work, "REVISION")
		prev, _ := os.ReadFile(revf)
		n := strings.Count(string(prev), "revision pass #") + 1
		entry := fmt.Sprintf("revision pass #%d\nreview: %s\n", n, strings.TrimSpace(string(rb)))
		if err := os.WriteFile(revf, append(prev, []byte(entry)...), 0644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		changed = append(changed, "REVISION")
	}

	out, err := runTests()
	passed := err == nil

	title := task["title"]
	if len(title) > 80 {
		title = title[:80]
	}
	status := "passed"
	if !passed {
		status = "FAILED"
	}
	kind := "stub agent"
	if revision {
		kind = "stub agent REVISION pass"
	}
	res, _ := json.Marshal(map[string]any{
		"changed_files": changed,
		"tests_passed":  passed,
		"summary":       fmt.Sprintf("%s for '%s': pytest %s\n%s", kind, title, status, last(string(out), 2000)),
	})
	os.WriteFile(filepath.Join(work, "result.json"), res, 0644)

	fmt.Println(last(string(out), 2000))
	if passed {
		return 0
	}
	return 1
}

func main() {
	os.Exit(run())
}
