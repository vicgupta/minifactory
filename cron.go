package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// cron.go: rootless scheduling via the user's own crontab.
//
// install-cron manages a per-user crontab entry that runs one factory
// cycle a minute, so minifactory works for users without root access (no
// systemd unit, no sudo). Overlap protection comes from flock: cron has
// none of its own, and two concurrent run-once cycles would race on
// data/queue.json.

// cronMarker identifies the crontab block managed by install-cron. The
// block is marker + MINIFACTORY_DIR assignment + the cron line.
const cronMarker = "# minifactory: managed by `minifactory install-cron` — do not edit"

// shellQuote quotes a path for sh/cron when it needs it.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// validCronSchedule reports whether s looks like a cron schedule: exactly
// five whitespace-separated fields. Field semantics are left to cron.
func validCronSchedule(s string) bool {
	f := strings.Fields(s)
	if len(f) != 5 {
		return false
	}
	for _, p := range f {
		if p == "" {
			return false
		}
	}
	return true
}

// cronBlock builds the managed crontab block: marker, the factory dir,
// and the full cycle command (auto) guarded by flock.
func cronBlock(schedule, exe, dir string) string {
	var b strings.Builder
	b.WriteString(cronMarker + "\n")
	b.WriteString("MINIFACTORY_DIR=" + shellQuote(dir) + "\n")
	fmt.Fprintf(&b, "%s flock -n %s %s auto\n",
		schedule, shellQuote(filepath.Join(dir, "cycle.lock")), shellQuote(exe))
	return b.String()
}

// upsertCronBlock returns current with any managed block replaced by block,
// or block appended when none exists. The result ends with a newline when
// non-empty.
func upsertCronBlock(current, block string) string {
	if i := strings.Index(current, cronMarker); i >= 0 {
		rest := current[i:]
		lines := strings.SplitN(rest, "\n", 4)
		tail := ""
		if len(lines) == 4 {
			tail = lines[3]
		}
		out := current[:i] + block + tail
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		return out
	}
	out := current
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + block
}

// removeCronBlock drops the managed block (marker + the two lines after
// it) from current. It reports whether a block was found.
func removeCronBlock(current string) (string, bool) {
	i := strings.Index(current, cronMarker)
	if i < 0 {
		return current, false
	}
	rest := current[i:]
	lines := strings.SplitN(rest, "\n", 4)
	tail := ""
	if len(lines) == 4 {
		tail = lines[3]
	}
	return current[:i] + tail, true
}

// readCrontab returns the user's current crontab, or "" when they have
// none. A missing crontab is not an error.
func readCrontab() (string, error) {
	rc, out := runCmd(10*time.Second, "", nil, "", "crontab", "-l")
	if rc != 0 {
		if strings.Contains(strings.ToLower(out), "no crontab") {
			return "", nil
		}
		return "", fmt.Errorf("crontab -l failed: %s", strings.TrimSpace(out))
	}
	return out, nil
}

// writeCrontab replaces the user's crontab with content.
func writeCrontab(content string) error {
	if rc, out := runCmd(10*time.Second, "", nil, content, "crontab", "-"); rc != 0 {
		return fmt.Errorf("crontab install failed: %s", strings.TrimSpace(out))
	}
	return nil
}

// cmdInstallCron implements `minifactory install-cron [--dir path]
// [--schedule expr] [--remove] [--force]`.
func cmdInstallCron(args []string) int {
	if _, err := exec.LookPath("flock"); err != nil {
		fmt.Fprintln(os.Stderr, "warning: `flock` not found on PATH — the cron entry needs it for overlap protection")
	}
	if hasFlag(args, "remove") {
		current, err := readCrontab()
		if err != nil {
			fmt.Fprintln(os.Stderr, "install-cron:", err)
			return 1
		}
		updated, found := removeCronBlock(current)
		if !found {
			fmt.Println("install-cron: no minifactory entry in crontab, nothing to remove")
			return 0
		}
		if err := writeCrontab(updated); err != nil {
			fmt.Fprintln(os.Stderr, "install-cron:", err)
			return 1
		}
		fmt.Println("install-cron: removed minifactory entry from crontab")
		return 0
	}

	dir := flagVal(args, "dir")
	if dir == "" {
		dir = baseDir
	}
	schedule := flagVal(args, "schedule")
	if schedule == "" {
		schedule = "* * * * *"
	}
	if !validCronSchedule(schedule) {
		fmt.Fprintf(os.Stderr, "install-cron: invalid schedule %q: want five space-separated fields (e.g. \"*/5 * * * *\")\n", schedule)
		return 2
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "install-cron: cannot locate own binary: %v\n", err)
		return 1
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	block := cronBlock(schedule, exe, dir)

	current, err := readCrontab()
	if err != nil {
		fmt.Fprintln(os.Stderr, "install-cron:", err)
		return 1
	}
	if strings.Contains(current, cronMarker) && !hasFlag(args, "force") {
		fmt.Println("install-cron: crontab already has a minifactory entry (use --force to replace or --remove to drop it):")
		fmt.Print(block)
		return 0
	}
	if err := writeCrontab(upsertCronBlock(current, block)); err != nil {
		fmt.Fprintln(os.Stderr, "install-cron:", err)
		return 1
	}
	fmt.Printf("install-cron: installed in your crontab — one cycle %s via flock\n", scheduleDesc(schedule))
	return 0
}

// scheduleDesc renders a cron schedule as a short human phrase for the
// common cases, falling back to the raw expression.
func scheduleDesc(schedule string) string {
	if schedule == "* * * * *" {
		return "every minute"
	}
	if strings.HasPrefix(schedule, "*/") {
		if f := strings.Fields(schedule); len(f) == 5 && f[1] == "*" && f[2] == "*" && f[3] == "*" && f[4] == "*" {
			return "every " + strings.TrimPrefix(f[0], "*/") + " minutes"
		}
	}
	return fmt.Sprintf("on schedule %q", schedule)
}
