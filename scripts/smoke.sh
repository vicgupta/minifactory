#!/bin/bash
# minifactory smoke test. Run as root on the VPS. Exits 0 on PASS, 1 on FAIL.
set -u
MF=/root/minifactory-go
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

if [ $pass = 1 ]; then echo "SMOKE PASS"; else echo "SMOKE FAIL"; fi
exit $((1-pass))
