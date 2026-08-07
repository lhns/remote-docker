#!/bin/bash
# shellcheck disable=SC2034,SC2016  # assertion vars are used inside eval strings
# Functional test for key-watcher + workspace-info against real useradd.
set -uo pipefail

BIN="$(cd "$(dirname "$0")/../image/bin" && pwd)"
ROOT=$(mktemp -d)
export WORKSPACE_STATE_DIR="$ROOT/state"
export WORKSPACE_KEYS_DIR="$ROOT/state/authorized_keys.d"
export WORKSPACE_UID_BASE=10000
export WORKSPACE_PORT_BASE=30000
export WORKSPACE_SHELL=/bin/bash
export WORKSPACE_KEY_POLL_INTERVAL=1
mkdir -p "$WORKSPACE_KEYS_DIR"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  PASS  $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  FAIL  $*"; }
check(){ if eval "$2"; then ok "$1"; else bad "$1  [$2]"; fi; }

getent group docker    >/dev/null || groupadd -r docker
getent group workspace >/dev/null || groupadd -r workspace

mkkey() { ssh-keygen -q -t ed25519 -N '' -C "$1" -f "$ROOT/$1" </dev/null; cp "$ROOT/$1.pub" "$WORKSPACE_KEYS_DIR/$2.pub"; }

run_watcher() { timeout 6 "$BIN/key-watcher" >"$ROOT/watch.log" 2>&1; }

echo "== 1. provision two users from two key files =="
mkkey alice alice
mkkey bob   bob
run_watcher

check "alice exists"                'id alice >/dev/null 2>&1'
check "bob exists"                  'id bob >/dev/null 2>&1'
check "alice uid == 10000"          '[ "$(id -u alice)" = 10000 ]'
check "bob uid == 10001"            '[ "$(id -u bob)" = 10001 ]'
check "alice in docker group"       'id -nG alice | grep -qw docker'
check "alice in workspace group"    'id -nG alice | grep -qw workspace'
check "uidmap persisted"            'grep -q "^alice:10000$" "$WORKSPACE_STATE_DIR/uidmap"'
check "alice ~/workspace created"   '[ -d /home/alice/workspace ]'
check "authorized_keys mode 0600"   '[ "$(stat -c %a /home/alice/.ssh/authorized_keys)" = 600 ]'
check "authorized_keys owned"       '[ "$(stat -c %U /home/alice/.ssh/authorized_keys)" = alice ]'
check "key carries restrictions"    'grep -q "^restrict,pty,port-forwarding ssh-ed25519 " /home/alice/.ssh/authorized_keys'
check "password disabled not locked" 'getent shadow alice | cut -d: -f2 | grep -qx "\*"'
check "alice key != bob key"        '! diff -q /home/alice/.ssh/authorized_keys /home/bob/.ssh/authorized_keys >/dev/null'

echo "== 2. uid->port math matches workspace-mount and workspace-info =="
printf 'WORKSPACE_UID_BASE=%s\nWORKSPACE_PORT_BASE=%s\n' 10000 30000 > "$ROOT/config"
mkdir -p "$ROOT/etcws" && cp "$ROOT/config" "$ROOT/etcws/config"
alice_port=$(su alice -s /bin/sh -c "WORKSPACE_UID_BASE=10000 WORKSPACE_PORT_BASE=30000 $BIN/workspace-info" 2>/dev/null | sed -n 's/^WORKSPACE_NFS_PORT=//p')
bob_port=$(su bob -s /bin/sh -c "WORKSPACE_UID_BASE=10000 WORKSPACE_PORT_BASE=30000 $BIN/workspace-info" 2>/dev/null | sed -n 's/^WORKSPACE_NFS_PORT=//p')
check "alice port 30000"            '[ "$alice_port" = 30000 ]'
check "bob port 30001"              '[ "$bob_port" = 30001 ]'
check "ports are distinct"          '[ "$alice_port" != "$bob_port" ]'
# NB: capture first -- `... | grep -q` would SIGPIPE the writer and, under
# `set -o pipefail`, report a failure that is purely an artifact of the test.
info_out=$(su alice -s /bin/sh -c "$BIN/workspace-info" 2>/dev/null)
check "workspace-info reports mountpoint" \
      '[ -n "$info_out" ] && grep -q "^WORKSPACE_MOUNTPOINT=/home/alice/workspace$" <<<"$info_out"'
check "workspace-info reports mount state" \
      'grep -q "^WORKSPACE_MOUNTED=false$" <<<"$info_out"'

echo "== 3. adding a third user does not disturb the first two =="
mkkey carol carol
run_watcher
check "carol uid == 10002"          '[ "$(id -u carol)" = 10002 ]'
check "alice uid unchanged"         '[ "$(id -u alice)" = 10000 ]'

echo "== 4. removing a key file revokes but keeps the account =="
rm -f "$WORKSPACE_KEYS_DIR/bob.pub"
run_watcher
check "bob account still exists"    'id bob >/dev/null 2>&1'
check "bob home still exists"       '[ -d /home/bob ]'
check "bob authorized_keys emptied" '[ ! -s /home/bob/.ssh/authorized_keys ]'
check "alice still authorized"      '[ -s /home/alice/.ssh/authorized_keys ]'

echo "== 5. re-adding bob reuses his original uid (port stability) =="
cp "$ROOT/bob.pub" "$WORKSPACE_KEYS_DIR/bob.pub"
run_watcher
check "bob uid still 10001"         '[ "$(id -u bob)" = 10001 ]'
check "bob re-authorized"           '[ -s /home/bob/.ssh/authorized_keys ]'

echo "== 6. rotating a key replaces it rather than appending =="
mkkey alice2 alice
run_watcher
check "alice has exactly one key"   '[ "$(wc -l < /home/alice/.ssh/authorized_keys)" = 1 ]'
check "alice has the NEW key"       'grep -q "$(cut -d" " -f2 "$ROOT/alice2.pub")" /home/alice/.ssh/authorized_keys'

echo "== 7. hostile / awkward filenames =="
cp "$ROOT/alice.pub" "$WORKSPACE_KEYS_DIR/Foo.Bar@Example.pub"
echo "this is not a public key" > "$WORKSPACE_KEYS_DIR/garbage.pub"
run_watcher
check "mixed case + dots sanitized" 'id foo-bar-example >/dev/null 2>&1'
check "garbage key file rejected"   '! id garbage >/dev/null 2>&1'
check "garbage logged"              'grep -q "not a valid public key" "$ROOT/watch.log"'

echo "== 8. mount helpers refuse to run outside sudo =="
out=$("$BIN/workspace-mount" 2>&1); rc=$?
check "workspace-mount refuses root"  '[ $rc -eq 64 ]'
check "  with a useful message"       'echo "$out" | grep -q "must be invoked as"'
out=$(SUDO_USER=nosuchuser "$BIN/workspace-umount" 2>&1); rc=$?
check "workspace-umount rejects unknown user" '[ $rc -eq 64 ]'
out=$(SUDO_USER=alice "$BIN/workspace-umount" 2>&1); rc=$?
check "workspace-umount is a no-op when unmounted" '[ $rc -eq 0 ] && echo "$out" | grep -q "not mounted"'

echo
echo "passed=$PASS failed=$FAIL"
for u in alice bob carol foo-bar-example; do userdel -r "$u" 2>/dev/null; done
rm -rf "$ROOT"
[ "$FAIL" -eq 0 ]
