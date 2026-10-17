package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// update.go: self-update from GitHub releases without a Go toolchain.
//
// `minifactory update` fetches the latest release of vicgupta/minifactory,
// downloads the tarball for the current platform, verifies its SHA256
// against the release's sha256sums.txt, smoke-tests the new binary, and
// atomically replaces the running one. `--check` only reports.

// updateRepo is the GitHub repo releases are read from.
const updateRepo = "vicgupta/minifactory"

// updateTimeout bounds the whole update (metadata + download).
const updateTimeout = 5 * time.Minute

type updateAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type updateRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []updateAsset `json:"assets"`
}

// parseVersion parses "v1.2.3" or "1.2.3" into parts.
func parseVersion(tag string) (major, minor, patch int, ok bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(tag), "v"), ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

// versionNewer reports whether want is a newer version than have.
func versionNewer(have, want string) bool {
	hmaj, hmin, hpatch, ok1 := parseVersion(have)
	wmaj, wmin, wpatch, ok2 := parseVersion(want)
	if !ok1 || !ok2 {
		return false
	}
	if wmaj != hmaj {
		return wmaj > hmaj
	}
	if wmin != hmin {
		return wmin > hmin
	}
	return wpatch > hpatch
}

// updateAssetName is the release asset name for a version/platform, e.g.
// minifactory-0.0.21-linux-amd64.tar.gz.
func updateAssetName(ver, goos, goarch string) string {
	ver = strings.TrimPrefix(ver, "v")
	return fmt.Sprintf("minifactory-%s-%s-%s.tar.gz", ver, goos, goarch)
}

// latestRelease fetches the latest GitHub release. The repo is public, so
// no token is needed.
func latestRelease() (*updateRelease, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET",
		"https://api.github.com/repos/"+updateRepo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "minifactory-update")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("checking latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("checking latest release: GitHub API returned %d", resp.StatusCode)
	}
	var rel updateRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parsing latest release: %w", err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("latest release has no tag")
	}
	return &rel, nil
}

// downloadFile fetches url into a temp file and returns its path.
func downloadFile(url string) (string, error) {
	client := &http.Client{Timeout: updateTimeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "minifactory-update")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("downloading %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.CreateTemp("", "minifactory-dl-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}
	return f.Name(), nil
}

// verifyChecksum checks that filePath's SHA256 appears in sumsText (the
// sha256sums.txt format: "<hex>  <filename>").
func verifyChecksum(filePath, sumsText, wantFile string) error {
	h, err := fileSHA256(filePath)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(sumsText, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == wantFile {
			if !strings.EqualFold(f[0], h) {
				return fmt.Errorf("checksum mismatch for %s: release says %s, downloaded %s",
					wantFile, f[0], h)
			}
			return nil
		}
	}
	return fmt.Errorf("no checksum entry for %s in sha256sums.txt — refusing to install", wantFile)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractTarGz extracts a .tar.gz into destDir and returns the paths of
// the extracted regular files. Entries escaping destDir are rejected.
func extractTarGz(src, destDir string) ([]string, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("not a gzip file: %w", err)
	}
	defer gz.Close()
	var out []string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Clean(hdr.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return nil, fmt.Errorf("tar entry escapes destination: %q", hdr.Name)
		}
		dest := filepath.Join(destDir, name)
		w, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(w, tr); err != nil {
			w.Close()
			return nil, err
		}
		w.Close()
		out = append(out, dest)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tarball contained no files")
	}
	return out, nil
}

// smokeTest runs `candidate version` and requires its output to mention
// the expected version.
func smokeTest(candidate, wantVer string) error {
	rc, out := runCmd(15*time.Second, "", nil, candidate, "version")
	if rc != 0 {
		return fmt.Errorf("new binary failed to run (exit %d): %s", rc, strings.TrimSpace(out))
	}
	wantVer = strings.TrimPrefix(wantVer, "v")
	if !strings.Contains(out, wantVer) {
		return fmt.Errorf("new binary reports %q, expected version %s", strings.TrimSpace(out), wantVer)
	}
	return nil
}

// cmdUpdate implements `minifactory update [--check]`.
func cmdUpdate(args []string) int {
	rel, err := latestRelease()
	if err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		return 1
	}
	if !versionNewer(version, rel.TagName) {
		fmt.Printf("update: already on the latest version (%s)\n", version)
		return 0
	}
	fmt.Printf("update: %s -> %s available\n", version, strings.TrimPrefix(rel.TagName, "v"))
	if hasFlag(args, "check") {
		return 0
	}

	wantAsset := updateAssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	var assetURL, sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case wantAsset:
			assetURL = a.BrowserDownloadURL
		case "sha256sums.txt":
			sumsURL = a.BrowserDownloadURL
		}
	}
	if assetURL == "" {
		fmt.Fprintf(os.Stderr, "update: release %s has no binary for %s/%s\n",
			rel.TagName, runtime.GOOS, runtime.GOARCH)
		return 1
	}
	if sumsURL == "" {
		fmt.Fprintf(os.Stderr, "update: release %s has no sha256sums.txt — refusing to install unverified binary\n", rel.TagName)
		return 1
	}

	fmt.Println("update: downloading", wantAsset)
	tarPath, err := downloadFile(assetURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		return 1
	}
	defer os.Remove(tarPath)
	sumsPath, err := downloadFile(sumsURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		return 1
	}
	defer os.Remove(sumsPath)
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		return 1
	}
	if err := verifyChecksum(tarPath, string(sums), wantAsset); err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		return 1
	}
	fmt.Println("update: checksum ok")

	stageDir, err := os.MkdirTemp("", "minifactory-update-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		return 1
	}
	defer os.RemoveAll(stageDir)
	files, err := extractTarGz(tarPath, stageDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		return 1
	}
	candidate := ""
	for _, f := range files {
		if filepath.Base(f) == "minifactory" {
			candidate = f
			break
		}
	}
	if candidate == "" {
		fmt.Fprintln(os.Stderr, "update: tarball has no minifactory binary")
		return 1
	}
	if err := smokeTest(candidate, rel.TagName); err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		return 1
	}
	fmt.Println("update: new binary smoke-tested ok")

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "update: cannot locate running binary:", err)
		return 1
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// Keep one backup of the previous binary, then atomically swap.
	// Rename (not truncate) works even though this process is running
	// from the old binary.
	backup := exe + ".bak"
	os.Remove(backup)
	if err := os.Rename(exe, backup); err != nil {
		fmt.Fprintln(os.Stderr, "update: cannot back up current binary:", err)
		return 1
	}
	if err := os.Rename(candidate, exe); err != nil {
		// Try to restore; report both errors if that fails too.
		if rerr := os.Rename(backup, exe); rerr != nil {
			fmt.Fprintf(os.Stderr, "update: install failed (%v) and rollback failed (%v) — backup at %s\n", err, rerr, backup)
		} else {
			fmt.Fprintln(os.Stderr, "update: install failed, previous binary restored:", err)
		}
		return 1
	}
	fmt.Printf("update: installed %s (previous binary kept at %s)\n", strings.TrimPrefix(rel.TagName, "v"), backup)
	return 0
}
