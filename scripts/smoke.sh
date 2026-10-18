#!/bin/bash
# minifactory smoke test. Run as root on the VPS. Exits 0 on PASS, 1 on FAIL.
set -u
MF=/root/minifactory-go
export MINIFACTORY_DIR="$MF"  # .env/queue/logs live in $MF, not beside the binary or in the demo cwd
S=/tmp/minifactory-smoke
rm -rf "$S"; mkdir -p "$S/demo"
cd "$S/demo"
git init -q -b main
git config user.email "factory@test"; git config user.name "factory"

# The contract: this test FAILS until the stub agent implements factory_addon.
cat > test_addon.py <<'EOF'
from factory_addon import greet

def test_greet():
    assert greet("vic") == "hello vic from factory"

def test_sanity():
    assert 1 + 1 == 2
EOF
git add -A; git commit -qm "init"
if python3 -c "import factory_addon" 2>/dev/null; then
  echo "FAIL: factory_addon should not exist before the stub runs"; exit 1
fi
echo "ok: contract test fails before the factory runs (as expected)"

git init -q -b main --bare "$S/remote.git"
git remote add origin "$S/remote.git"
git push -q origin main

ID=$("$MF/minifactory" issue --repo "file://$S/remote.git" \
      --title "smoke: add greeting addon" --body "demo task")
echo "task: $ID"
"$MF/minifactory" run-once

STATE=$("$MF/minifactory" list --json | python3 -c \
  "import json,sys; print([t for t in json.load(sys.stdin) if t['id']=='$ID'][0]['state'])")
LOG="$MF/logs/$ID.log"
pass=1
[ "$STATE" = "pr_open" ] || { echo "FAIL: state=$STATE (want pr_open)"; pass=0; }
grep -q -- "--network none" "$LOG" && { echo "FAIL: sandbox must have full egress (--network none found)"; pass=0; }
grep -q "2 passed" "$LOG" || { echo "FAIL: no pytest pass evidence in task log"; pass=0; }
grep -q "agent selected: stub" "$LOG" || { echo "FAIL: stub agent not selected"; pass=0; }
git --git-dir="$S/remote.git" branch --list "factory/*" | grep -q . \
  || { echo "FAIL: factory branch not pushed"; pass=0; }

# --- revision loop (needs stub revision support: `minifactory revise`) ---
state_of() {
  "$MF/minifactory" list --json | python3 -c \
    "import json,sys; print([t for t in json.load(sys.stdin) if t['id']=='$ID'][0]['state'])"
}
tip() { git --git-dir="$S/remote.git" rev-parse "factory/$ID" 2>/dev/null; }
MAX_REV="${MAX_REVISIONS:-3}"

BRANCH_TIP0=$(tip)
[ -n "$BRANCH_TIP0" ] || { echo "FAIL: no factory/$ID branch before revision"; pass=0; }

"$MF/minifactory" revise "$ID" --feedback "smoke: please revise" \
  || { echo "FAIL: revise command failed"; pass=0; }
"$MF/minifactory" run-once
TIP1=$(tip)
STATE=$(state_of)
[ "$STATE" = "pr_open" ] || { echo "FAIL: after revision state=$STATE (want pr_open)"; pass=0; }
[ -n "$TIP1" ] && [ "$TIP1" != "$BRANCH_TIP0" ] \
  || { echo "FAIL: revision did not add a commit to factory/$ID"; pass=0; }
git --git-dir="$S/remote.git" merge-base --is-ancestor "$BRANCH_TIP0" "$TIP1" 2>/dev/null \
  || { echo "FAIL: revision was not a fast-forward of the old tip"; pass=0; }
echo "ok: revision 1 fast-forwarded factory/$ID"

# No new feedback: a second run-once must not start another revision.
"$MF/minifactory" run-once
TIP2=$(tip)
[ "$TIP2" = "$TIP1" ] || { echo "FAIL: revision repeated without new feedback"; pass=0; }
[ "$(state_of)" = "pr_open" ] || { echo "FAIL: state changed without new feedback"; pass=0; }

# Keep requesting revisions until the cap; the extra attempt must be refused.
prev=$TIP2
for i in $(seq 2 "$MAX_REV"); do
  "$MF/minifactory" revise "$ID" --feedback "smoke: revision $i" >/dev/null 2>&1
  "$MF/minifactory" run-once
  cur=$(tip)
  [ "$cur" != "$prev" ] || { echo "FAIL: revision $i did not advance the branch"; pass=0; }
  prev=$cur
done
"$MF/minifactory" revise "$ID" --feedback "smoke: over the cap" >/dev/null 2>&1
"$MF/minifactory" run-once
[ "$(tip)" = "$prev" ] || { echo "FAIL: MAX_REVISIONS ($MAX_REV) not enforced"; pass=0; }
echo "ok: revision cap checked (MAX_REVISIONS=$MAX_REV)"

if [ $pass = 1 ]; then echo "SMOKE PASS"; else echo "SMOKE FAIL"; fi
exit $((1-pass))
