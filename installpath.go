package main

import (
	"fmt"
	"os"
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
	var changed []string
	for _, p := range profilePaths(home, shell) {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			return changed, fmt.Errorf("reading %s: %w", p, err)
		}
		next, added := ensurePathBlock(string(b))
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
		return statusWarn, fmt.Sprintf("binary: %s (user-local default)\n      -> %s is not on PATH yet: add the `# minifactory` block via install.sh, then open a new shell",
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
