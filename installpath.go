package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// installpath.go: where the user-facing minifactory binary lives.
//
// The default is always user-local: ~/.local/bin/minifactory. It never
// defaults to /usr/local/bin, /usr/bin, or any other root-owned directory,
// and placing it never needs sudo. An explicit BIN_DIR-style override
// (resolved by the caller / installer) wins over the default; this file
// only decides the default and the PATH/profile bookkeeping around it.
// Factory state is unrelated: it stays in the cwd .env dir or
// ~/.minifactory (see resolveBaseDirWith) and does not move with the binary.

// legacyBinaryPath is the old suggested install location. It is only ever
// detected so installs can warn about a stale competing copy — it is never
// a default, a fallback, or an update target.
const legacyBinaryPath = "/usr/local/bin/minifactory"

// profileMarker marks the PATH block install.sh / ensurePathInProfiles add
// to shell profiles. Its presence makes profile edits idempotent.
const profileMarker = "# minifactory"

// pathExportLine is the single PATH line in the marked profile block.
const pathExportLine = `export PATH="$HOME/.local/bin:$PATH"`

// defaultBinDir returns the default user-local binary directory for home.
func defaultBinDir(home string) string {
	if home == "" {
		return filepath.Join(".local", "bin")
	}
	return filepath.Join(home, ".local", "bin")
}

// resolveBinDir applies the override-wins rule: a non-empty override is
// returned verbatim (cleaned); otherwise the user-local default is used.
func resolveBinDir(override, home string) string {
	if strings.TrimSpace(override) != "" {
		return filepath.Clean(override)
	}
	return defaultBinDir(home)
}

// defaultBinaryPath is the default full path of the user-facing binary.
func defaultBinaryPath(home string) string {
	return filepath.Join(defaultBinDir(home), "minifactory")
}

// binaryPathIn returns the binary path inside an explicit bin dir.
func binaryPathIn(binDir string) string {
	return filepath.Join(binDir, "minifactory")
}

// pathContainsDir reports whether dir is an entry of a PATH-style list.
// A literal $HOME/.local/bin (or ~) entry counts for the default dir so a
// profile that already exports it is recognised before expansion.
func pathContainsDir(pathEnv, dir string) bool {
	if dir == "" {
		return false
	}
	clean := filepath.Clean(dir)
	for _, e := range filepath.SplitList(pathEnv) {
		if e == "" {
			continue
		}
		if filepath.Clean(e) == clean {
			return true
		}
	}
	return false
}

// ensurePathBlock returns content with the marked minifactory PATH block
// appended, unless the block (or the export line itself) is already there.
// Running it twice adds exactly one block.
func ensurePathBlock(content string) (string, bool) {
	if strings.Contains(content, profileMarker) || strings.Contains(content, pathExportLine) {
		return content, false
	}
	var b strings.Builder
	b.WriteString(content)
	if content != "" && !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}
	if content != "" {
		b.WriteString("\n")
	}
	b.WriteString(profileMarker + "\n" + pathExportLine + "\n")
	return b.String(), true
}

// pathExportLineForDir is the export line for binDir: the canonical
// $HOME form for the user-local default (matching install.sh), otherwise
// the literal directory (matching install.sh's BIN_DIR-override line).
func pathExportLineForDir(binDir, home string) string {
	if home != "" && filepath.Clean(binDir) == defaultBinDir(home) {
		return pathExportLine
	}
	return fmt.Sprintf(`export PATH="%s:$PATH"`, binDir)
}

// ensurePathBlockForLine is ensurePathBlock for an explicit export line.
func ensurePathBlockForLine(content, line string) (string, bool) {
	if line != pathExportLine && strings.Contains(content, line) {
		if strings.Contains(content, profileMarker) || strings.Contains(content, line) {
			return content, false
		}
	}
	if line == pathExportLine {
		return ensurePathBlock(content)
	}
	if strings.Contains(content, profileMarker) || strings.Contains(content, line) {
		return content, false
	}
	var b strings.Builder
	b.WriteString(content)
	if content != "" && !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}
	if content != "" {
		b.WriteString("\n")
	}
	b.WriteString(profileMarker + "\n" + line + "\n")
	return b.String(), true
}

// profileHasPathBlock reports whether any profile for home/shell already
// carries the marked block (or the export line) for binDir.
func profileHasPathBlock(home, shell, binDir string) bool {
	line := pathExportLineForDir(binDir, home)
	for _, pf := range profilePaths(home, shell) {
		b, err := os.ReadFile(pf)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), profileMarker) || strings.Contains(string(b), line) {
			return true
		}
	}
	return false
}

// profilePaths returns the profile files PATH setup should touch for home
// and shell: always ~/.profile, plus the interactive rc for the shell
// (zsh -> ~/.zshrc, everything else/default -> ~/.bashrc), de-duplicated.
func profilePaths(home, shell string) []string {
	if home == "" {
		return nil
	}
	rc := filepath.Join(home, ".bashrc")
	if strings.Contains(shell, "zsh") {
		rc = filepath.Join(home, ".zshrc")
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range []string{filepath.Join(home, ".profile"), rc} {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// ensurePathInProfiles appends the marked PATH block to each profile file
// for home/shell that does not already carry it, creating files as needed.
// It returns the files it changed. It is a no-op when the block is present.
func ensurePathInProfiles(home, shell string) ([]string, error) {
	return ensurePathInProfilesForDir(home, shell, defaultBinDir(home))
}

// ensurePathInProfilesForDir is ensurePathInProfiles for an explicit bin
// dir (the standard ~/.local/bin, or a BIN_DIR override): same marker,
// same profile targets, same idempotence, creating files as needed (as
// install.sh does with touch). Only files under home are ever touched.
func ensurePathInProfilesForDir(home, shell, binDir string) ([]string, error) {
	line := pathExportLineForDir(binDir, home)
	var changed []string
	for _, p := range profilePaths(home, shell) {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			return changed, fmt.Errorf("reading %s: %w", p, err)
		}
		next, added := ensurePathBlockForLine(string(b), line)
		if !added {
			continue
		}
		if err := os.WriteFile(p, []byte(next), 0o644); err != nil {
			return changed, fmt.Errorf("writing %s: %w", p, err)
		}
		changed = append(changed, p)
	}
	return changed, nil
}

// --- init PATH step ---

// pathResult is the outcome of init's PATH/profile step.
type pathResult struct {
	State    string
	Detail   string
	BinDir   string
	Exe      string
	Changed  []string
	Resolved string
	Warning  string
	Err      error
	Standard bool
	Export   string
}

func (r pathResult) manualExportLine() string {
	if r.Export != "" {
		return r.Export
	}
	if r.BinDir == "" {
		return pathExportLine
	}
	return fmt.Sprintf(`export PATH="%s:$PATH"`, r.BinDir)
}

var (
	initExecutable     = func() (string, error) { return os.Executable() }
	initLookPath       = exec.LookPath
	legacyBinaryExists = func() bool { return fileExists(legacyBinaryPath) }
)

var ensurePathInInitFn = ensurePathInInit

func ensurePathInInit() pathResult {
	exe, err := initExecutable()
	if err != nil {
		return pathResult{State: "warning", Detail: "could not determine the running binary: " + err.Error(), Err: err}
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	home, _ := os.UserHomeDir()
	return applyInitPath(exe, home, os.Getenv("SHELL"), os.Getenv("BIN_DIR"))
}

// applyInitPath does both honest things a child process can do: (a) persist
// PATH for future shells via the marked profile block (reusing
// ensurePathInProfilesForDir, install.sh parity), only when binDir is the
// standard ~/.local/bin or BIN_DIR override; (b) prepend binDir to this
// process's PATH before any later init steps. It cannot modify the parent shell.
func applyInitPath(exe, home, shell, binDirOverride string) pathResult {
	exe = filepath.Clean(exe)
	binDir := filepath.Dir(exe)
	res := pathResult{Exe: exe, BinDir: binDir}
	if exe == "." || binDir == "" {
		res.State = "warning"
		res.Detail = "could not determine the running binary's directory"
		return res
	}
	origPath := os.Getenv("PATH")
	wasOnPath := pathContainsDir(origPath, binDir)
	if !wasOnPath {
		newPath := binDir
		if origPath != "" {
			newPath = binDir + string(os.PathListSeparator) + origPath
		}
		_ = os.Setenv("PATH", newPath)
	}
	expected := resolveBinDir(binDirOverride, home)
	if binDirOverride != "" && !filepath.IsAbs(expected) {
		if abs, err := filepath.Abs(expected); err == nil {
			expected = abs
		}
	}
	res.Standard = home != "" && filepath.Clean(binDir) == filepath.Clean(expected)
	res.Export = pathExportLineForDir(binDir, home)
	switch {
	case home == "":
		res.State = "warning"
		res.Detail = "could not determine HOME, so no profile was updated"
	case !res.Standard:
		res.State = "skipped"
		res.Detail = fmt.Sprintf("running from %s, outside the standard install dir (%s), so no profile was updated", exe, expected)
	case wasOnPath:
		res.State = "already set"
		res.Detail = fmt.Sprintf("%s is already on PATH", binDir)
	default:
		changed, err := ensurePathInProfilesForDir(home, shell, binDir)
		if err != nil {
			res.State = "warning"
			res.Detail = "could not update shell profiles"
			res.Err = err
		} else if len(changed) == 0 {
			res.State = "already set"
			res.Detail = fmt.Sprintf("profile block already present for %s - new shells will pick it up automatically", binDir)
		} else {
			res.State = "profile updated"
			res.Changed = changed
			res.Detail = "profile block added"
		}
	}
	if p, err := initLookPath("minifactory"); err == nil {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		res.Resolved = p
		if filepath.Clean(p) != filepath.Clean(exe) {
			if w := legacyBinaryWarning(exe, legacyBinaryExists()); w != "" {
				res.Warning = w
			} else {
				res.Warning = fmt.Sprintf("warning: `minifactory` on PATH resolves to %s, not the running binary %s - remove the other copy so it does not shadow this one", p, exe)
			}
		}
	}
	if res.Warning == "" {
		if w := legacyBinaryWarning(exe, legacyBinaryExists()); w != "" {
			res.Warning = w
		}
	}
	return res
}

func shortProfilePath(home, p string) string {
	if home != "" && filepath.Dir(p) == filepath.Clean(home) {
		return "~/" + filepath.Base(p)
	}
	return p
}

func printPathSummary(home string, r pathResult) {
	switch r.State {
	case "skipped":
		if r.Detail == "--skip-path" {
			fmt.Println("  path: skipped (--skip-path)")
		} else {
			fmt.Printf("  path: skipped - %s\n", r.Detail)
			if r.Resolved != "" {
				fmt.Printf("        resolves in this init run as: %s\n", r.Resolved)
			}
		}
	case "already set":
		line := fmt.Sprintf("  path: already set - %s", r.Detail)
		if r.Resolved != "" {
			line += fmt.Sprintf("; minifactory resolves to %s", r.Resolved)
		}
		fmt.Println(line)
	case "profile updated":
		var names []string
		for _, c := range r.Changed {
			names = append(names, shortProfilePath(home, c))
		}
		fmt.Printf("  path: profile updated (%s) - new shells will pick it up automatically; your current shell was not modified, run this once in it: %s\n", strings.Join(names, ", "), r.manualExportLine())
		if r.Resolved != "" {
			fmt.Printf("        minifactory resolves in this init run as: %s\n", r.Resolved)
		}
	case "warning":
		fmt.Printf("  path: warning - %s", r.Detail)
		if r.Err != nil {
			fmt.Printf(": %v", r.Err)
		}
		fmt.Printf("; run this once in your current shell: %s\n", r.manualExportLine())
		if r.Resolved != "" {
			fmt.Printf("        minifactory resolves in this init run as: %s\n", r.Resolved)
		}
	default:
		fmt.Printf("  path: %s - %s\n", r.State, r.Detail)
	}
	if r.Warning != "" {
		fmt.Printf("  %s\n", r.Warning)
	}
}

// dirWritable reports whether dir exists (or can be created) and accepts a
// new file — self-update uses it to refuse a target it cannot replace and
// to print manual steps instead of failing mid-swap or demanding sudo.
func dirWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".minifactory-write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// manualInstallSteps renders the exact manual recovery for an update whose
// target directory is not writable: download, verify, and copy by hand.
func manualInstallSteps(exe, assetName string) string {
	return fmt.Sprintf("manual update steps:\n"+
		"  1. download %s and sha256sums.txt from https://github.com/%s/releases/latest\n"+
		"  2. verify: sha256sum -c sha256sums.txt (the %s line must say OK)\n"+
		"  3. tar -xzf %s\n"+
		"  4. mkdir -p %s && cp minifactory %s && chmod +x %s\n"+
		"  (target %s is not writable by this user — do not use sudo to force it;\n"+
		"   reinstall into %s instead and make sure that directory is on your PATH)",
		assetName, updateRepo, assetName, assetName,
		filepath.Dir(defaultBinaryPath("$HOME")), shellQuoteForManual(defaultBinaryPath("$HOME")), shellQuoteForManual(defaultBinaryPath("$HOME")),
		exe, defaultBinaryPath("$HOME"))
}

func shellQuoteForManual(s string) string { return s }

// assessBinaryLocation is the pure core of doctor's binary check: given the
// running executable, the user's home, and the current PATH, it reports a
// status and human detail. The legacy location warns (stale competing copy
// risk); the user-local default is ok; anything else is informational as
// long as it resolves on PATH.
func assessBinaryLocation(exe, home, pathEnv string) (checkStatus, string) {
	exe = filepath.Clean(exe)
	def := defaultBinaryPath(home)
	switch {
	case exe == filepath.Clean(legacyBinaryPath):
		return statusWarn, fmt.Sprintf("binary: %s\n      -> this is the old /usr/local/bin location; move it to %s "+
			"(mkdir -p %s && cp %s %s) and remove the old copy so two versions do not compete",
			exe, def, filepath.Dir(def), exe, def)
	case exe == def:
		if pathContainsDir(pathEnv, filepath.Dir(def)) {
			return statusOK, fmt.Sprintf("binary: %s (user-local default, on PATH)", exe)
		}
		return statusWarn, fmt.Sprintf("binary: %s (user-local default)\n      -> %s is not on PATH yet: add the `# minifactory` block via install.sh or `minifactory init`, then open a new shell",
			exe, filepath.Dir(def))
	default:
		if pathContainsDir(pathEnv, filepath.Dir(exe)) {
			return statusInfo, fmt.Sprintf("binary: %s (explicit/custom location, on PATH; user-local default is %s)", exe, def)
		}
		return statusWarn, fmt.Sprintf("binary: %s\n      -> its directory is not on PATH; the user-local default is %s", exe, def)
	}
}

// legacyBinaryWarning describes a stale legacy copy when one exists at the
// old path alongside (not as) the current binary. Empty means no warning.
func legacyBinaryWarning(exe string, legacyExists bool) string {
	if !legacyExists || filepath.Clean(exe) == filepath.Clean(legacyBinaryPath) {
		return ""
	}
	return fmt.Sprintf("warning: an older minifactory also exists at %s — remove it so it does not shadow %s: rm %s (use sudo rm %s only if that file is root-owned)",
		legacyBinaryPath, exe, legacyBinaryPath, legacyBinaryPath)
}
