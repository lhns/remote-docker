#!/bin/bash
# What do host-side mount events actually do to a RUNNING container?
#
# Two questions, because the answers differ and the difference drives the
# design of workspace-mount:
#
#   A. host replaces the mount AT the workspace   -> container does NOT follow
#   B. host adds a mount INSIDE the workspace     -> container DOES follow,
#                                                    but only with rslave
#
# This USED to justify workspace-mount being idempotent and warning you to
# restart containers on --force. Per-bind volumes retired that entirely
# (ADR 0006): the daemon mounts each volume itself, so nothing has to propagate
# into a running container.
#
# What it still governs is the convenience mount at ~/workspace, which the
# agent makes for an interactive shell -- and the reason that mount is NOT
# what containers use.
#
# A tmpfs stands in for the NFS mount; propagation semantics are identical.
# Requires: a working docker daemon and the proptest image (see test/Dockerfile).
set -uo pipefail

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  PASS  $*"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL  $*"; }

U=proptestu
MP="/home/$U/workspace"

cleanup() {
    docker rm -f prop-rslave prop-rprivate >/dev/null 2>&1
    umount -l "$MP/inner" 2>/dev/null
    umount -l "$MP" 2>/dev/null
    userdel -r "$U" 2>/dev/null
    umount -l /tmp/privsrc/inner 2>/dev/null
}
trap cleanup EXIT

userdel -r "$U" 2>/dev/null
useradd --uid 10500 --create-home --shell /bin/bash "$U"
mkdir -p "$MP"

# ---- exactly what workspace-mount does ------------------------------------
mountpoint -q /home || mount --bind /home /home
mount --make-rshared /home
mount -t tmpfs tmpfs "$MP"
mount --make-rshared "$MP"
mkdir -p "$MP/inner"
echo x > "$MP/generation-1"

prop_of() { findmnt -no PROPAGATION --target "$1"; }
echo "== mount state after workspace-mount =="
echo "  /home  ->  $(prop_of /home)"
echo "  $MP  ->  $(prop_of "$MP")"
[ "$(prop_of /home)" = shared ] && ok "/home is shared" || bad "/home is shared (got '$(prop_of /home)')"
[ "$(prop_of "$MP")" = shared ] && ok "workspace mount is shared" || bad "workspace mount is shared"

docker run -d --name prop-rslave \
    --mount type=bind,source="$MP",target=/data,bind-propagation=rslave \
    proptest:latest >/dev/null 2>&1 \
    && ok "container starts with bind-propagation=rslave" \
    || bad "container starts with bind-propagation=rslave"

docker run -d --name prop-rprivate \
    --mount type=bind,source="$MP",target=/data,bind-propagation=rprivate \
    proptest:latest >/dev/null 2>&1 \
    && ok "control container starts with rprivate" \
    || bad "control container starts with rprivate"

sleep 3
docker logs prop-rslave 2>/dev/null | grep -q 'generation-1' \
    && ok "container sees the workspace contents" \
    || bad "container sees the workspace contents"

# ---- B: a mount made INSIDE the workspace ---------------------------------
echo "== B: host mounts something INSIDE the workspace =="
mount -t tmpfs tmpfs "$MP/inner"
echo y > "$MP/inner/nested-marker"
sleep 4

docker logs prop-rslave 2>/dev/null | tail -3 | grep -q 'inner=nested-marker' \
    && ok "rslave container SEES the nested mount" \
    || bad "rslave container sees the nested mount"

docker logs prop-rprivate 2>/dev/null | tail -3 | grep -q 'inner=nested-marker' \
    && bad "rprivate container should not see the nested mount, but does" \
    || ok "rprivate container does NOT see it -- this is what rshared+rslave buys"

umount -l "$MP/inner"

# ---- A: host REPLACES the workspace mount ---------------------------------
echo "== A: host replaces the mount at the workspace (a forced remount) =="
umount -l "$MP"
mount -t tmpfs tmpfs "$MP"
mount --make-rshared "$MP"
echo z > "$MP/generation-2"
sleep 4

# This is the documented limitation, asserted so a future change that "fixes"
# it does not go unnoticed.
if docker logs prop-rslave 2>/dev/null | tail -3 | grep -q 'generation-2'; then
    bad "rslave container followed the replacement -- README needs updating"
else
    ok "rslave container does NOT follow the replacement (documented limitation)"
    echo "        it still reports: $(docker logs prop-rslave 2>/dev/null | tail -1)"
fi

docker logs prop-rprivate 2>/dev/null | tail -3 | grep -q 'generation-2' \
    && bad "rprivate container followed the replacement" \
    || ok "rprivate container does not follow it either"

# ---- rslave requires a shared source --------------------------------------
echo "== control: rslave against a NON-shared source =="
mkdir -p /tmp/privsrc/inner
mount -t tmpfs tmpfs /tmp/privsrc/inner
mount --make-rprivate /tmp/privsrc/inner
docker run --rm --name prop-neg \
      --mount type=bind,source=/tmp/privsrc/inner,target=/data,bind-propagation=rslave \
      proptest:latest >/dev/null 2>&1 \
    && bad "docker accepted rslave on a private source (expected refusal)" \
    || ok "docker refuses rslave on a private source -- confirms make-rshared is required"

echo
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
