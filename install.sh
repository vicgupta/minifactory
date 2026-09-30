#!/usr/bin/env bash
#
# install.sh — full installer for minifactory-go.
#
# Run as root on the target host, from this source directory:
#     sudo ./install.sh
#
# What it does:
#   1. preflight checks  — root, go, git, docker, python3, systemd
#   2. environment       — creates/merges $APP_DIR/.env with every
#                          variable the factory reads (secrets are
#                          prompted for, never echoed; file is 0600)
#   3. build             — go vet, go build, go test
#   4. worker image      — docker build factory-worker-go
#   5. systemd           — installs service+timer, enables the Go timer
#   6. post-install checks — binary, timer, image, required variables
#
# Flags:
#   --live-test   also run `./minifactory poll` to verify GitHub API access
#   --help        usage
#
# Environment overrides:
#   MINIFACTORY_DIR   install directory (default /root/minifactory-go)
#
set -euo pipefail

APP_DIR="${MINIFACTORY_DIR:-/root/minifactory-go}"
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIVE_TEST=0

usage() {
    sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
}

for arg in "$@"; do
    case "$arg" in
        --live-test) LIVE_TEST=1 ;;
        --help|-h) usage; exit 0 ;;
        *) echo "unknown flag: $arg" >&2; usage >&2; exit 2 ;;
    esac
done

pass() { echo "  [ok] $1"; }
fail() { echo "  [FAIL] $1" >&2; FAILURES=$((FAILURES+1)); }
FAILURES=0

echo "== 1. preflight checks =="
[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }
pass "running as root"

for cmd in go git docker python3 systemctl; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "missing required command: $cmd" >&2; exit 1; }
    pass "found $cmd"
done

GO_VER="$(go version | grep -oE '[0-9]+\.[0-9]+' | head -1)"
GO_MAJOR="${GO_VER%%.*}"; GO_MINOR="${GO_VER##*.}"
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
mkdir -p "$APP_DIR"/{data,logs,work}

# copy sources into the install dir (so the timer runs a complete tree)
if [ "$SRC_DIR" != "$APP_DIR" ]; then
    for f in "$SRC_DIR"/*.go "$SRC_DIR"/go.mod; do
        [ -e "$f" ] || continue
        cp "$f" "$APP_DIR"/
    done
    [ -f "$SRC_DIR/go.sum" ] && cp "$SRC_DIR/go.sum" "$APP_DIR"/
    for d in worker scripts; do
        [ -d "$SRC_DIR/$d" ] && cp -r "$SRC_DIR/$d" "$APP_DIR"/
    done
    cp "$SRC_DIR"/minifactory-go.service "$SRC_DIR"/minifactory-go.timer "$APP_DIR"/
    pass "sources copied to $APP_DIR"
else
    pass "source dir is the install dir — nothing to copy"
fi

ENV_FILE="$APP_DIR/.env"
declare -A ENV_MAP=()
if [ -f "$ENV_FILE" ]; then
    while IFS='=' read -r k v; do
        case "$k" in ''|\#*) continue ;; esac
        k="$(echo "$k" | xargs)"; v="$(echo "$v" | xargs | sed -e 's/^"//' -e 's/"$//')"
        [ -n "$k" ] && ENV_MAP["$k"]="$v"
    done < "$ENV_FILE"
    pass "loaded existing .env ($((${#ENV_MAP[@]})) keys kept)"
else
    echo "  (no .env yet — will create)"
fi

prompt_secret() { # $1=var $2=description
    local var="$1"
    local desc="$2"
    local cur="${ENV_MAP[$var]:-}"
    local val=""
    if [ -n "$cur" ]; then
        echo "  $var already set (kept)"
        return
    fi
    read -rsp "  enter $var [$desc]: " val || true; echo
    if [ -n "$val" ]; then ENV_MAP["$var"]="$val"; else echo "  (left empty)"; fi
}
prompt_value() { # $1=var $2=description
    local var="$1"
    local desc="$2"
    local cur="${ENV_MAP[$var]:-}"
    local val=""
    if [ -n "$cur" ]; then
        echo "  $var already set (kept)"
        return
    fi
    read -rp "  enter $var [$desc]: " val || true
    if [ -n "$val" ]; then ENV_MAP["$var"]="$val"; else echo "  (left empty)"; fi
}

# required
prompt_secret GITHUB_TOKEN "classic PAT with repo scope"
prompt_value  GITHUB_REPO  "owner/repo, e.g. vicgupta/minitest"
# agent auth — at least one is needed for real (non-stub) runs
prompt_secret CLAUDE_CODE_OAUTH_TOKEN "Claude subscription OAuth token"
prompt_secret CODEX_TOKEN "optional, for the codex agent"
prompt_secret OPENCODE_TOKEN "optional, for the opencode agent"

# capture before the write block below unsets the known keys
if [ -z "${ENV_MAP[CLAUDE_CODE_OAUTH_TOKEN]:-}${ENV_MAP[CODEX_TOKEN]:-}${ENV_MAP[OPENCODE_TOKEN]:-}" ]; then
    NO_AGENT_TOKEN=1
else
    NO_AGENT_TOKEN=0
fi

{
    for k in GITHUB_TOKEN GITHUB_REPO CLAUDE_CODE_OAUTH_TOKEN CODEX_TOKEN OPENCODE_TOKEN; do
        if [[ -v 'ENV_MAP[$k]' ]]; then
            echo "$k=${ENV_MAP[$k]}"
            unset 'ENV_MAP[$k]'
        fi
    done
    for k in "${!ENV_MAP[@]}"; do echo "$k=${ENV_MAP[$k]}"; done
} > "$ENV_FILE"
chmod 600 "$ENV_FILE"
pass ".env written (mode 600, secrets not echoed)"

if [ "$NO_AGENT_TOKEN" -eq 1 ]; then
    echo "  [warn] no agent token set — factory will only run the stub agent"
fi

echo "== 3. build =="
cd "$APP_DIR"
go vet ./... && pass "go vet clean"
go build -o minifactory . && pass "binary built"
go test ./... 2>&1 | tail -3

echo "== 4. worker image =="
[ -f "$APP_DIR/worker/Dockerfile" ] || { echo "worker/Dockerfile missing" >&2; exit 1; }
docker build -q -t factory-worker-go "$APP_DIR/worker/" >/dev/null && pass "factory-worker-go image built"

echo "== 5. systemd =="
sed "s|/root/minifactory-go|$APP_DIR|g" "$APP_DIR/minifactory-go.service" \
    > /etc/systemd/system/minifactory-go.service
cp "$APP_DIR/minifactory-go.timer" /etc/systemd/system/minifactory-go.timer
systemctl daemon-reload
pass "units installed"

systemctl enable --now minifactory-go.timer
pass "minifactory-go.timer enabled and started"

echo "== 6. post-install checks =="
cd "$APP_DIR"
./minifactory list >/dev/null 2>&1 && pass "binary runs (queue readable)" \
    || fail "binary ./minifactory list failed"
[ "$(systemctl is-enabled minifactory-go.timer)" = "enabled" ] \
    && pass "timer enabled" || fail "timer not enabled"
[ "$(systemctl is-active minifactory-go.timer)" = "active" ] \
    && pass "timer active" || fail "timer not active"
docker image inspect factory-worker-go >/dev/null 2>&1 \
    && pass "worker image present" || fail "worker image missing"
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
