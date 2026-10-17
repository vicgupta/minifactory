#!/bin/sh
#
# install.sh — installer for minifactory.
#
# Primary install (one line, from anywhere):
#     curl -fsSL https://raw.githubusercontent.com/vicgupta/minifactory/main/install.sh | sh
#
# That piped form runs under POSIX sh (dash included): it detects OS/arch,
# downloads the matching latest-release tarball, verifies its SHA256
# against the release's sha256sums.txt (failing closed on any mismatch),
# installs the binary to ~/.local/bin (never /usr/local/bin, never sudo
# for placement), adds ~/.local/bin to PATH via a marked, idempotent
# profile block, then auto-invokes `minifactory init`.
#
# Other modes from a source checkout:
#     ./install.sh --user   build from source into $BIN_DIR + PATH + init
#     sudo ./install.sh     full server install (root, docker, systemd):
#                           preflight, $APP_DIR/.env, build, worker image,
#                           systemd timer, post-install checks
#
# Flags:
#   --user        user-local install (source build if a checkout is present,
#                 otherwise the release download) — no root/docker/systemd
#   --download    force the release-download install (the curl|sh path)
#   --no-init     skip the auto-invoked `minifactory init`
#   --live-test   server mode only: also run `./minifactory poll`
#   --help        usage
#
# Environment overrides:
#   BIN_DIR               binary directory (default ~/.local/bin; explicit
#                         BIN_DIR wins — the default never falls back to
#                         /usr/local/bin or /usr/bin, and placement never
#                         uses sudo)
#   MINIFACTORY_DIR       factory home / server install directory
#                         (default /root/minifactory-go, server mode only).
#                         Factory state never lives in BIN_DIR: it stays in
#                         the cwd .env dir or ~/.minifactory.
#   MINIFACTORY_SKIP_INIT=1   skip auto-invoking `minifactory init`
#   MINIFACTORY_SKIP_DEPS=1   run init with --skip-deps (no git/docker install)
#   MINIFACTORY_RELEASE_TAG=vX.Y.Z  install that release instead of latest
#   MINIFACTORY_DOWNLOAD_BASE=URL-or-file://dir  download base override
#                         (test seam; default the GitHub release download URL)
#   DRY_RUN=1             download mode: print the plan and exit
#   HOME                  honoured throughout (temp-HOME test seam)
#
set -eu

# BIN_DIR: explicit override wins; otherwise the user-local default.
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
APP_DIR="${MINIFACTORY_DIR:-/root/minifactory-go}"
LEGACY_BIN="/usr/local/bin/minifactory"
REPO="vicgupta/minifactory"
LIVE_TEST=0
USER_ONLY=0
DOWNLOAD=0
NO_INIT=0
# $0 is a real file for ./install.sh / sh install.sh; it is just "sh" when
# the script arrives on stdin via curl|sh — that is how the piped form is
# detected, with no reliance on the caller's working directory.
if [ ! -f "$0" ]; then
    DOWNLOAD=1
fi
SRC_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" 2>/dev/null && pwd || echo ".")
[ -f "$SRC_DIR/go.mod" ] || DOWNLOAD_GUARD=1
DOWNLOAD_GUARD=${DOWNLOAD_GUARD:-0}

usage() {
    sed -n '2,/^set -eu$/p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//' || echo "usage: install.sh [--user|--download] [--no-init] [--live-test]"
}

while [ $# -gt 0 ]; do
    case "$1" in
        --live-test) LIVE_TEST=1 ;;
        --user) USER_ONLY=1 ;;
        --download) DOWNLOAD=1 ;;
        --no-init) NO_INIT=1 ;;
        --help|-h) usage; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 2 ;;
    esac
    shift
done
if [ "$USER_ONLY" -eq 1 ] && [ "$DOWNLOAD_GUARD" -eq 1 ]; then
    DOWNLOAD=1
fi

pass() { echo "  [ok] $1"; }
FAILURES=0
fail() { echo "  [FAIL] $1" >&2; FAILURES=$((FAILURES+1)); }

# --- user-local binary + PATH helpers (all modes) ---

path_has_bin_dir() {
    case ":$PATH:" in
        *":$BIN_DIR:"*) return 0 ;;
        *) return 1 ;;
    esac
}

# ensure_local_bin_on_path: survive new shells, idempotently. Adds the
# marked block to ~/.profile plus the interactive rc for $SHELL
# (zsh -> ~/.zshrc, bash/other/default -> ~/.bashrc), exports PATH for the
# rest of this installer run, and says exactly what happened. Running it
# twice adds exactly one block; if BIN_DIR is already on PATH it reports
# that and changes nothing.
ensure_local_bin_on_path() {
    was_on_path=0
    path_has_bin_dir && was_on_path=1
    export PATH="$BIN_DIR:$PATH"
    if [ "$was_on_path" -eq 1 ]; then
        echo "  [ok] $BIN_DIR is already on PATH — no profile changes"
        return 0
    fi
    if [ "$BIN_DIR" = "$HOME/.local/bin" ]; then
        line='export PATH="$HOME/.local/bin:$PATH"'
    else
        line="export PATH=\"$BIN_DIR:\$PATH\""
    fi
    rc="$HOME/.bashrc"
    case "${SHELL:-}" in
        *zsh) rc="$HOME/.zshrc" ;;
    esac
    for f in "$HOME/.profile" "$rc"; do
        mkdir -p "$(dirname "$f")"
        touch "$f"
        if grep -q '# minifactory' "$f" || grep -Fq "$line" "$f"; then
            echo "  [ok] $f already has the minifactory PATH line — unchanged"
        else
            printf '\n# minifactory\n%s\n' "$line" >> "$f"
            echo "  [ok] added minifactory PATH line to $f"
        fi
    done
    echo "  -> open a new shell, or run: source $HOME/.profile (this shell will not see $BIN_DIR until then)"
}

warn_legacy_binary() {
    if [ -e "$LEGACY_BIN" ] && [ "$LEGACY_BIN" != "$BIN_DIR/minifactory" ]; then
        echo "  [warn] an older minifactory exists at $LEGACY_BIN"
        echo "         the new binary is $BIN_DIR/minifactory — remove the old copy so it does not shadow it:"
        echo "         rm $LEGACY_BIN   (use: sudo rm $LEGACY_BIN  only if that file is root-owned)"
    fi
}

install_user_binary() {
    mkdir -p "$BIN_DIR"
    cp "$1" "$BIN_DIR/minifactory"
    chmod +x "$BIN_DIR/minifactory"
    pass "binary installed to $BIN_DIR/minifactory"
}

verify_user_binary() {
    resolved=""
    if command -v bash >/dev/null 2>&1; then
        resolved=$(HOME="$HOME" bash -lc 'command -v minifactory' 2>/dev/null || true)
    fi
    if [ -n "$resolved" ]; then
        echo "  [ok] command -v minifactory -> $resolved (fresh login shell)"
    elif "$BIN_DIR/minifactory" version >/dev/null 2>&1; then
        echo "  [ok] $BIN_DIR/minifactory runs ($("$BIN_DIR/minifactory" version)) — open a new shell so 'minifactory' itself resolves"
    else
        echo "  [warn] installed binary did not run; check $BIN_DIR/minifactory" >&2
    fi
}

# run_minifactory_init: auto-invoke init by absolute path (PATH may not
# have picked BIN_DIR up yet), in the caller's real cwd/HOME. init itself
# never prompts for values (it writes an empty .env template); only its
# Linux git/docker install may need sudo, which prompts on the terminal —
# so a real tty is handed over via /dev/tty when one exists (the curl
# pipe occupies stdin). With no tty, init runs with --skip-deps rather
# than hanging on a sudo prompt that nobody can answer.
run_minifactory_init() {
    if [ "$NO_INIT" -eq 1 ] || [ "${MINIFACTORY_SKIP_INIT:-}" = "1" ]; then
        echo "  [ok] skipping 'minifactory init' (opt-out set) — run it later: $BIN_DIR/minifactory init"
        return 0
    fi
    echo "== minifactory init =="
    if [ "${MINIFACTORY_SKIP_DEPS:-}" = "1" ]; then
        if "$BIN_DIR/minifactory" init --skip-deps </dev/null; then pass "minifactory init done (--skip-deps)"; else echo "  [warn] 'minifactory init' did not complete — run it manually: $BIN_DIR/minifactory init" >&2; fi
    elif [ -c /dev/tty ] && (exec </dev/tty) 2>/dev/null; then
        if "$BIN_DIR/minifactory" init </dev/tty; then pass "minifactory init done"; else echo "  [warn] 'minifactory init' did not complete — run it manually: $BIN_DIR/minifactory init" >&2; fi
    else
        echo "  (no terminal available — running init with --skip-deps so no sudo prompt can hang; run '$BIN_DIR/minifactory init' in a terminal to install git/docker if needed)"
        if "$BIN_DIR/minifactory" init --skip-deps </dev/null; then pass "minifactory init done (--skip-deps, no tty)"; else echo "  [warn] 'minifactory init' did not complete — run it manually: $BIN_DIR/minifactory init" >&2; fi
    fi
}

finish_user_install() {
    warn_legacy_binary
    ensure_local_bin_on_path
    verify_user_binary
    run_minifactory_init
    echo "install complete: $BIN_DIR/minifactory"
}

# --- release-download install (the curl|sh path) ---

download_install() {
    os=$(uname -s)
    case "$os" in
        Linux) os=linux ;;
        Darwin) os=darwin ;;
        *) echo "unsupported OS: $os (want Linux or Darwin)" >&2; exit 1 ;;
    esac
    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64) arch=amd64 ;;
        arm64|aarch64) arch=arm64 ;;
        *) echo "unsupported architecture: $arch (want x86_64 or arm64/aarch64)" >&2; exit 1 ;;
    esac
    command -v curl >/dev/null 2>&1 || { echo "curl is required to download minifactory" >&2; exit 1; }
    command -v tar >/dev/null 2>&1 || { echo "tar is required to unpack minifactory" >&2; exit 1; }

    tag="${MINIFACTORY_RELEASE_TAG:-}"
    if [ -z "$tag" ] && [ -z "${MINIFACTORY_DOWNLOAD_BASE:-}" ]; then
        echo "resolving the latest $REPO release..."
        tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
        [ -n "$tag" ] || { echo "could not resolve the latest release tag from the GitHub API" >&2; exit 1; }
    fi
    [ -n "$tag" ] || { echo "MINIFACTORY_RELEASE_TAG is required when MINIFACTORY_DOWNLOAD_BASE is set" >&2; exit 1; }
    ver=${tag#v}
    asset="minifactory-${ver}-${os}-${arch}.tar.gz"
    base="${MINIFACTORY_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download/$tag}"
    echo "minifactory $tag for $os/$arch -> $BIN_DIR/minifactory"

    if [ "${DRY_RUN:-}" = "1" ]; then
        echo "DRY_RUN: would download $base/$asset"
        echo "DRY_RUN: would verify it against $base/sha256sums.txt (fail closed on mismatch)"
        echo "DRY_RUN: would install to $BIN_DIR/minifactory, set up PATH, then run: $BIN_DIR/minifactory init"
        return 0
    fi

    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT INT TERM
    echo "downloading $asset..."
    curl -fsSL -o "$tmp/$asset" "$base/$asset" || { echo "download failed: $base/$asset" >&2; exit 1; }
    curl -fsSL -o "$tmp/sha256sums.txt" "$base/sha256sums.txt" || { echo "sha256sums.txt unavailable — refusing to install an unverified binary" >&2; exit 1; }
    expected=$(awk -v a="$asset" '$2==a || $2=="*"a {print $1}' "$tmp/sha256sums.txt" | head -n 1)
    [ -n "$expected" ] || { echo "sha256sums.txt has no entry for $asset — refusing to install" >&2; exit 1; }
    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
    elif command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
    else
        echo "no sha256 tool (sha256sum/shasum) available — refusing to install an unverified binary" >&2; exit 1
    fi
    [ "$expected" = "$actual" ] || { echo "checksum mismatch for $asset (expected $expected, got $actual) — refusing to install" >&2; exit 1; }
    pass "checksum verified for $asset"
    mkdir -p "$tmp/extract"
    tar -xzf "$tmp/$asset" -C "$tmp/extract"
    [ -f "$tmp/extract/minifactory" ] || { echo "release tarball has no minifactory binary" >&2; exit 1; }
    install_user_binary "$tmp/extract/minifactory"
    "$BIN_DIR/minifactory" version >/dev/null 2>&1 && pass "installed binary runs: $("$BIN_DIR/minifactory" version)" || { echo "installed binary failed to run" >&2; exit 1; }
    rm -rf "$tmp"
    trap - EXIT INT TERM
    finish_user_install
}

if [ "$DOWNLOAD" -eq 1 ] && { [ "$USER_ONLY" -eq 1 ] || [ ! -f "$0" ] || [ "$DOWNLOAD_GUARD" -eq 1 ]; }; then
    download_install
    exit 0
fi
if [ "$DOWNLOAD" -eq 1 ] && [ "$USER_ONLY" -eq 0 ]; then
    download_install
    exit 0
fi

if [ "$USER_ONLY" -eq 1 ]; then
    echo "== user-local install (binary -> $BIN_DIR) =="
    mkdir -p "$BIN_DIR"
    if command -v go >/dev/null 2>&1 && [ -f "$SRC_DIR/go.mod" ]; then
        (cd "$SRC_DIR" && go build -o "$BIN_DIR/minifactory" .) && pass "binary built to $BIN_DIR/minifactory"
    elif [ -x "$SRC_DIR/minifactory" ]; then
        install_user_binary "$SRC_DIR/minifactory"
    else
        echo "no Go toolchain and no prebuilt ./minifactory next to install.sh — use the release download instead:" >&2
        echo "  curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sh" >&2
        exit 1
    fi
    finish_user_install
    exit 0
fi

# --- full server install (root, from a source checkout) ---

echo "== 1. preflight checks =="
[ "$(id -u)" -eq 0 ] || { echo "must run as root (or use --user / the curl|sh one-liner for a user-local install)" >&2; exit 1; }
pass "running as root"
for cmd in go git docker python3 systemctl; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "missing required command: $cmd" >&2; exit 1; }
    pass "found $cmd"
done
GO_VER=$(go version | grep -oE '[0-9]+\.[0-9]+' | head -1)
GO_MAJOR=${GO_VER%%.*}; GO_MINOR=${GO_VER##*.}
if [ "$GO_MAJOR" -gt 1 ] || { [ "$GO_MAJOR" -eq 1 ] && [ "$GO_MINOR" -ge 21 ]; }; then
    pass "go $GO_VER >= 1.21"
else
    echo "go >= 1.21 required (found $GO_VER)" >&2; exit 1
fi
docker info >/dev/null 2>&1 || { echo "docker daemon not reachable" >&2; exit 1; }
pass "docker daemon reachable"
[ -d /run/systemd/system ] || { echo "systemd not running as init" >&2; exit 1; }
pass "systemd present"

echo "== 2. environment ($APP_DIR/.env) =="
mkdir -p "$APP_DIR/data" "$APP_DIR/logs" "$APP_DIR/work"
if [ "$SRC_DIR" != "$APP_DIR" ]; then
    for f in "$SRC_DIR"/*.go "$SRC_DIR"/go.mod; do
        [ -e "$f" ] || continue
        cp "$f" "$APP_DIR"/
    done
    [ -f "$SRC_DIR/go.sum" ] && cp "$SRC_DIR/go.sum" "$APP_DIR/" || true
    for d in worker scripts; do
        [ -d "$SRC_DIR/$d" ] && cp -r "$SRC_DIR/$d" "$APP_DIR"/ || true
    done
    cp "$SRC_DIR"/minifactory-go.service "$SRC_DIR"/minifactory-go.timer "$APP_DIR"/
    pass "sources copied to $APP_DIR"
else
    pass "source dir is the install dir — nothing to copy"
fi

ENV_FILE="$APP_DIR/.env"
env_value() {
    # $1=key: print its value from the existing $ENV_FILE ("" when absent)
    [ -f "$ENV_FILE" ] || return 0
    sed -n "s/^$1=//p" "$ENV_FILE" | head -n 1 | sed -e 's/^"//' -e 's/"$//'
}
VAL_GITHUB_TOKEN=$(env_value GITHUB_TOKEN)
VAL_GITHUB_REPO=$(env_value GITHUB_REPO)
VAL_CLAUDE=$(env_value CLAUDE_CODE_OAUTH_TOKEN)
VAL_CODEX=$(env_value CODEX_TOKEN)
VAL_OPENCODE=$(env_value OPENCODE_TOKEN)
if [ -f "$ENV_FILE" ]; then
    pass "existing .env kept (values below are preserved unless re-entered)"
fi
prompt_secret() {
    # $1=variable-name $2=description — sets NEW_<name> when answered
    cur=$(env_value "$1")
    if [ -n "$cur" ]; then echo "  $1 already set (kept)"; return; fi
    printf "  enter %s [%s]: " "$1" "$2"
    if [ -c /dev/tty ] && (exec </dev/tty) 2>/dev/null; then stty -echo </dev/tty 2>/dev/null || true; IFS= read -r val </dev/tty || val=""; stty echo </dev/tty 2>/dev/null || true; echo; else IFS= read -r val || val=""; echo; fi
    case "$1" in
        GITHUB_TOKEN) VAL_GITHUB_TOKEN=${val:-$VAL_GITHUB_TOKEN} ;;
        CLAUDE_CODE_OAUTH_TOKEN) VAL_CLAUDE=${val:-$VAL_CLAUDE} ;;
        CODEX_TOKEN) VAL_CODEX=${val:-$VAL_CODEX} ;;
        OPENCODE_TOKEN) VAL_OPENCODE=${val:-$VAL_OPENCODE} ;;
    esac
    [ -n "$val" ] || echo "  (left empty)"
}
prompt_value() {
    cur=$(env_value "$1")
    if [ -n "$cur" ]; then echo "  $1 already set (kept)"; return; fi
    printf "  enter %s [%s]: " "$1" "$2"
    if [ -c /dev/tty ] && (exec </dev/tty) 2>/dev/null; then IFS= read -r val </dev/tty || val=""; else IFS= read -r val || val=""; fi
    case "$1" in
        GITHUB_REPO) VAL_GITHUB_REPO=${val:-$VAL_GITHUB_REPO} ;;
    esac
    [ -n "$val" ] || echo "  (left empty)"
}
prompt_secret GITHUB_TOKEN "classic PAT with repo scope"
prompt_value GITHUB_REPO "owner/repo, e.g. vicgupta/minitest"
prompt_secret CLAUDE_CODE_OAUTH_TOKEN "Claude subscription OAuth token"
prompt_secret CODEX_TOKEN "optional, for the codex agent"
prompt_secret OPENCODE_TOKEN "optional, for the opencode agent"
{
    echo "GITHUB_TOKEN=$VAL_GITHUB_TOKEN"
    echo "GITHUB_REPO=$VAL_GITHUB_REPO"
    echo "CLAUDE_CODE_OAUTH_TOKEN=$VAL_CLAUDE"
    echo "CODEX_TOKEN=$VAL_CODEX"
    echo "OPENCODE_TOKEN=$VAL_OPENCODE"
    if [ -f "$ENV_FILE" ]; then grep -vE '^(GITHUB_TOKEN|GITHUB_REPO|CLAUDE_CODE_OAUTH_TOKEN|CODEX_TOKEN|OPENCODE_TOKEN)=' "$ENV_FILE" || true; fi
} > "$ENV_FILE.tmp"
mv "$ENV_FILE.tmp" "$ENV_FILE"
chmod 600 "$ENV_FILE"
pass ".env written (mode 600, secrets not echoed)"
if [ -z "$VAL_CLAUDE$VAL_CODEX$VAL_OPENCODE" ]; then
    echo "  [warn] no agent token set — factory will only run the stub agent"
fi

echo "== 3. build =="
cd "$APP_DIR"
go vet ./... && pass "go vet clean"
go build -o minifactory . && pass "binary built"
install_user_binary "$APP_DIR/minifactory"
warn_legacy_binary
ensure_local_bin_on_path
go test ./... 2>&1 | tail -3

echo "== 4. worker image =="
[ -f "$APP_DIR/worker/Dockerfile" ] || { echo "worker/Dockerfile missing" >&2; exit 1; }
if docker build -q -t factory-worker-go "$APP_DIR/worker/" >/dev/null; then
    pass "factory-worker-go image built"
    # A multi-stage build pulls and retains the tagged builder-stage
    # image (golang:...); remove that exact build-only image so
    # `docker images` does not keep listing it. Best-effort: a failed
    # removal warns and the install continues. No prunes.
    BUILDER_IMAGE=$(awk '{ if (tolower($1) == "from") { for (i = 3; i <= NF; i++) { if (tolower($i) == "as") { print $2; exit } } } }' "$APP_DIR/worker/Dockerfile")
    if [ -n "$BUILDER_IMAGE" ]; then
        echo "  removing build-only builder image $BUILDER_IMAGE (builder stage of worker/Dockerfile)"
        echo "  $ docker image rm $BUILDER_IMAGE"
        if docker image rm "$BUILDER_IMAGE" >/dev/null 2>&1; then
            pass "removed build-only builder image $BUILDER_IMAGE"
        else
            echo "  [warn] could not remove builder image $BUILDER_IMAGE — continuing; remove it with: docker image rm $BUILDER_IMAGE" >&2
        fi
    fi
else
    echo "factory-worker-go image build failed" >&2
    exit 1
fi

echo "== 5. systemd =="
# The service calls the binary by its absolute user-local path ($BIN_DIR),
# which always resolves regardless of the unit's minimal PATH.
sed -e "s|/root/minifactory-go|$APP_DIR|g" -e "s|/root/.local/bin|$BIN_DIR|g" "$APP_DIR/minifactory-go.service" \
    > /etc/systemd/system/minifactory-go.service
cp "$APP_DIR/minifactory-go.timer" /etc/systemd/system/minifactory-go.timer
systemctl daemon-reload
pass "units installed"
systemctl enable --now minifactory-go.timer
pass "minifactory-go.timer enabled and started"

echo "== 6. post-install checks =="
cd "$APP_DIR"
./minifactory list >/dev/null 2>&1 && pass "binary runs (queue readable)" || fail "binary ./minifactory list failed"
[ "$(systemctl is-enabled minifactory-go.timer)" = "enabled" ] && pass "timer enabled" || fail "timer not enabled"
[ "$(systemctl is-active minifactory-go.timer)" = "active" ] && pass "timer active" || fail "timer not active"
docker image inspect factory-worker-go >/dev/null 2>&1 && pass "worker image present" || fail "worker image missing"
for k in GITHUB_TOKEN GITHUB_REPO; do
    grep -qE "^$k=.+$" "$ENV_FILE" && pass "$k set" || fail "$k missing/empty in .env"
done
if [ "$LIVE_TEST" -eq 1 ]; then
    echo "== 7. live test (GitHub poll) =="
    ./minifactory poll || fail "poll failed"
fi
echo
if [ "$FAILURES" -eq 0 ]; then
    echo "install complete — all checks passed."
    systemctl list-timers minifactory-go.timer --no-pager | head -3
else
    echo "$FAILURES check(s) FAILED" >&2
    exit 1
fi
