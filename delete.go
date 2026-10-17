package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// delete.go: `minifactory delete-all` tears down a factory installation.
//
// It removes, for the factory dir (--dir, default baseDir):
//   - systemd units (minifactory/minifactory .service + .timer)
//   - the service user's crontab block (from install-cron)
//   - the nologin service user whose home is the factory dir
//   - the dir's managed state: data/, logs/, work/, .env, .env.bak.*,
//     cycle.lock
// The binary itself is left in place and reported.
//
// Destructive, so it prints a plan and exits 2 unless --force is given.
// Steps needing root (units, other users' crontabs, userdel) are done when
// euid == 0, otherwise printed as commands for the operator.

// deleteUnitNames are the systemd unit base names delete-all looks for.
var deleteUnitNames = []string{"minifactory", "minifactory"}

// managedDeleteNames are the factory-dir entries delete-all removes.
// Anything else in the dir (notably the binary and source checkouts) is
// left alone, so running it in a dev tree can't nuke the repo.
var managedDeleteNames = []string{"data", "logs", "work", ".env", "cycle.lock"}

// passwdUser is one /etc/passwd row.
type passwdUser struct {
	name, home, shell string
	uid               int
}

func parsePasswd(text string) []passwdUser {
	var out []passwdUser
	for _, line := range strings.Split(text, "\n") {
		f := strings.Split(line, ":")
		if len(f) < 7 {
			continue
		}
		uid, _ := strconv.Atoi(f[2])
		out = append(out, passwdUser{name: f[0], home: f[5], shell: f[6], uid: uid})
	}
	return out
}

func isNologinShell(shell string) bool {
	base := filepath.Base(strings.TrimSpace(shell))
	return base == "nologin" || base == "false"
}

// serviceUserForDir finds a nologin-shell user whose home directory is dir.
// The service user is identified by its home, not its name, so installs
// under either spelling (minifactory/minifactory) are found.
func serviceUserForDir(dir string) (string, bool) {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "", false
	}
	for _, u := range parsePasswd(string(b)) {
		if filepath.Clean(u.home) == dir && isNologinShell(u.shell) {
			return u.name, true
		}
	}
	return "", false
}

// managedDeleteEntries returns the known managed entries under dir that
// currently exist (plus .env backups).
func managedDeleteEntries(dir string) []string {
	var out []string
	for _, n := range managedDeleteNames {
		if p := filepath.Join(dir, n); exists(p) {
			out = append(out, p)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".env.bak.*")); len(matches) > 0 {
		out = append(out, matches...)
	}
	return out
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// readUserCrontab returns user's crontab ("" = current user), or "" when
// they have none.
func readUserCrontab(u string) (string, error) {
	args := []string{"-l"}
	if u != "" {
		args = []string{"-u", u, "-l"}
	}
	rc, out := runCmd(10*time.Second, "", nil, "crontab", args...)
	if rc != 0 {
		if strings.Contains(strings.ToLower(out), "no crontab") {
			return "", nil
		}
		return "", fmt.Errorf("crontab -l failed: %s", strings.TrimSpace(out))
	}
	return out, nil
}

// writeUserCrontab replaces user's crontab ("" = current user).
func writeUserCrontab(u, content string) error {
	args := []string{"-"}
	if u != "" {
		args = []string{"-u", u, "-"}
	}
	if rc, out := runCmdWithStdin(10*time.Second, "", nil, content, "crontab", args...); rc != 0 {
		return fmt.Errorf("crontab install failed: %s", strings.TrimSpace(out))
	}
	return nil
}

func currentUsername() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// deletePlan is what `minifactory delete-all` will tear down.
type deletePlan struct {
	dir      string
	units    []string // unit base names with files on disk
	cronUser string   // user whose crontab holds our block
	svcUser  string   // nologin service user to delete ("" = none)
	keepUser bool
	entries  []string // managed dir entries to remove
	isRoot   bool
}

func planDeleteAll(dir string, keepUser bool, userOverride string) *deletePlan {
	p := &deletePlan{dir: dir, keepUser: keepUser, isRoot: os.Geteuid() == 0}
	for _, n := range deleteUnitNames {
		if exists("/etc/systemd/system/"+n+".service") ||
			exists("/etc/systemd/system/"+n+".timer") {
			p.units = append(p.units, n)
		}
	}
	if userOverride != "" {
		p.svcUser = userOverride
	} else if !keepUser {
		if u, ok := serviceUserForDir(dir); ok {
			p.svcUser = u
		}
	}
	p.cronUser = ""
	// Prefer the service user's crontab, fall back to the current user's
	// (rootless installs put the entry in the installing user's crontab).
	seen := map[string]bool{}
	for _, u := range []string{p.svcUser, currentUsername()} {
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		if cur, err := readUserCrontab(u); err == nil && strings.Contains(cur, cronMarker) {
			p.cronUser = u
			break
		}
	}
	p.entries = managedDeleteEntries(dir)
	return p
}

func (p *deletePlan) empty() bool {
	return len(p.units) == 0 && p.cronUser == "" && p.svcUser == "" && len(p.entries) == 0
}

func (p *deletePlan) print(w *os.File) {
	fmt.Fprintf(w, "delete-all plan for %s:\n", p.dir)
	if len(p.units) > 0 {
		fmt.Fprintf(w, "  systemd units: %s (disable timer, remove unit files)\n", strings.Join(p.units, ", "))
	}
	if p.cronUser != "" {
		fmt.Fprintf(w, "  crontab entry: user %s (managed block)\n", p.cronUser)
	}
	if p.svcUser != "" {
		fmt.Fprintf(w, "  system user: %s (nologin; processes killed, user deleted with -r)\n", p.svcUser)
	} else if p.keepUser {
		fmt.Fprintf(w, "  system user: kept (--keep-user)\n")
	}
	if len(p.entries) > 0 {
		fmt.Fprintf(w, "  factory state: %s\n", strings.Join(p.entries, ", "))
	}
	if !p.isRoot && (len(p.units) > 0 || (p.svcUser != "" && !p.keepUser)) {
		fmt.Fprintf(w, "  NOTE: not root — unit/user removal needs root; those steps will be printed, not run\n")
	}
	if exe, err := os.Executable(); err == nil {
		fmt.Fprintf(w, "  binary left in place: %s\n", exe)
	}
}

// cmdDeleteAll implements `minifactory delete-all [--dir path] [--user name]
// [--keep-user] [--force]`.
func cmdDeleteAll(args []string) int {
	dir := flagVal(args, "dir")
	if dir == "" {
		dir = baseDir
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "delete-all:", err)
		return 1
	}
	dir = filepath.Clean(abs)
	if dir == "/" {
		fmt.Fprintln(os.Stderr, "delete-all: refusing to operate on /")
		return 2
	}
	p := planDeleteAll(dir, hasFlag(args, "keep-user"), flagVal(args, "user"))
	if p.empty() {
		fmt.Println("delete-all: nothing to remove for", dir)
		return 0
	}
	if !hasFlag(args, "force") {
		p.print(os.Stdout)
		fmt.Println("re-run with --force to execute this plan")
		return 2
	}

	// 1. systemd units
	for _, n := range p.units {
		if p.isRoot {
			runCmd(30*time.Second, "", nil, "systemctl", "disable", "--now", n+".timer")
			os.Remove("/etc/systemd/system/" + n + ".service")
			os.Remove("/etc/systemd/system/" + n + ".timer")
			fmt.Println("delete-all: removed systemd units for", n)
		} else {
			fmt.Printf("delete-all: NEEDS ROOT — systemctl disable --now %s.timer && rm /etc/systemd/system/%s.{service,timer}\n", n, n)
		}
	}
	if p.isRoot && len(p.units) > 0 {
		runCmd(30*time.Second, "", nil, "systemctl", "daemon-reload")
	}

	// 2. crontab block
	if p.cronUser != "" {
		u := ""
		if p.cronUser != currentUsername() {
			u = p.cronUser
		}
		if p.isRoot || u == "" {
			if cur, err := readUserCrontab(u); err != nil {
				fmt.Fprintln(os.Stderr, "delete-all: crontab read:", err)
			} else if updated, found := removeCronBlock(cur); found {
				if err := writeUserCrontab(u, updated); err != nil {
					fmt.Fprintln(os.Stderr, "delete-all: crontab write:", err)
				} else {
					fmt.Println("delete-all: removed crontab entry for user", p.cronUser)
				}
			}
		} else {
			fmt.Printf("delete-all: NEEDS ROOT — remove the minifactory block from %s's crontab\n", p.cronUser)
		}
	}

	// 3. service user
	if p.svcUser != "" && !p.keepUser {
		if p.isRoot {
			runCmd(10*time.Second, "", nil, "pkill", "-u", p.svcUser)
			if rc, out := runCmd(30*time.Second, "", nil, "userdel", "-r", p.svcUser); rc != 0 {
				fmt.Fprintf(os.Stderr, "delete-all: userdel -r %s failed: %s\n", p.svcUser, strings.TrimSpace(out))
			} else {
				fmt.Println("delete-all: deleted user", p.svcUser)
			}
		} else {
			fmt.Printf("delete-all: NEEDS ROOT — userdel -r %s\n", p.svcUser)
		}
	}

	// 4. managed dir entries
	for _, e := range p.entries {
		if err := os.RemoveAll(e); err != nil {
			fmt.Fprintf(os.Stderr, "delete-all: cannot remove %s: %v\n", e, err)
		} else {
			fmt.Println("delete-all: removed", e)
		}
	}
	fmt.Println("delete-all: done")
	return 0
}
