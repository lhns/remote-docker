#!/usr/bin/env bash
# ONE account, TWO client machines, at the same time (ADR 0029).
#
# The claim: an account is a person and a person has more than one computer, so
# both may hold a session at once. What they share is the daemon, and therefore
# containers and images. What they must NOT share is files, because those live
# on one machine each.
#
# Two clients here are two state directories with a key each, both enrolled
# against `alice` in one key file, which is the format's own multi-key support.
# Different keys mean different client ids, which is what everything below turns
# on.
#
# Separate from per-user-dind.sh, which is two ACCOUNTS. This is two machines of
# one account, and the two suites fail in different places when the same thing
# breaks.
#
# Requires: docker, and a kernel with NFS client support.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-twoclients
SSH_PORT=22224

# One account, two machines.
ACCOUNT=alice
PC=pc
PHONE=phone

DOCKER_TIMEOUT=180

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

cleanup() {
    echo
    echo "== cleanup =="
    for pid in ${PC_PID:-} ${PHONE_PID:-}; do
        kill "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
    done
    hostdocker rm -f "$CONTAINER" >/dev/null 2>&1
    rm -rf "$WORK"
}
trap cleanup EXIT

echo "== 1. build =="
if build_image && build_client; then
    ok "image and client build"
else
    bad "build failed"
    exit 1
fi

echo
echo "== 2. two machines, one account =="
mkdir -p "$WORK/keys" "$WORK/wsstate"
for machine in "$PC" "$PHONE"; do
    REMOTE_DOCKER_STATE_DIR="$WORK/state-$machine" \
        "$WORK/remote-docker" remote enroll >/dev/null 2>&1
    if [ ! -f "$WORK/state-$machine/id_ed25519.pub" ]; then
        bad "no key generated for $machine"
        exit 1
    fi
done

# BOTH keys in ONE file, which is what makes them one account. A key file holds
# several keys and is parsed line by line, so this is the format doing what it
# was built for rather than a trick.
cat "$WORK/state-$PC/id_ed25519.pub" "$WORK/state-$PHONE/id_ed25519.pub" \
    >"$WORK/keys/$ACCOUNT.pub"

if [ "$(grep -c . "$WORK/keys/$ACCOUNT.pub")" -eq 2 ]; then
    ok "two keys enrolled as one account"
else
    bad "the key file does not hold two keys"
    exit 1
fi

echo
echo "== 3. start the workspace =="
# The shared daemon, because the claim is that two machines of one account see
# the same containers. A daemon per account would prove that too and would add
# a second variable to every failure below.
if start_workspace false; then
    ok "workspace container started"
else
    bad "workspace container failed to start"
    exit 1
fi

if wait_provisioned "$ACCOUNT"; then
    ok "the account was provisioned"
else
    bad "the account was never provisioned"
    dump_workspace_log 40
    exit 1
fi
wait_parent_dockerd

echo
echo "== 4. a session on each machine, at the same time =="
mkdir -p "$WORK/project-$PC" "$WORK/project-$PHONE"
echo "from the pc" >"$WORK/project-$PC/marker"
echo "from the phone" >"$WORK/project-$PHONE/marker"

start_session() {
    local machine=$1 endpoint=$2 log=$3 dir=$4
    (
        cd "$dir" || exit 1
        REMOTE_DOCKER_STATE_DIR="$WORK/state-$machine" \
        REMOTE_DOCKER_HOST=127.0.0.1 \
        REMOTE_DOCKER_PORT=$SSH_PORT \
        REMOTE_DOCKER_USER="$ACCOUNT" \
        REMOTE_DOCKER_ENDPOINT="$endpoint" \
        exec "$WORK/remote-docker" remote start --foreground
    ) >"$log" 2>&1 &
    echo $!
}

PC_SOCK="$WORK/pc.sock"
PHONE_SOCK="$WORK/phone.sock"

PC_PID=$(start_session "$PC" "$PC_SOCK" "$WORK/pc.log" "$WORK/project-$PC")
PHONE_PID=$(start_session "$PHONE" "$PHONE_SOCK" "$WORK/phone.log" "$WORK/project-$PHONE")

# One command per machine, each against that machine's own endpoint.
dpc() { timeout "$DOCKER_TIMEOUT" docker -H "unix://$PC_SOCK" "$@"; }
dphone() { timeout "$DOCKER_TIMEOUT" docker -H "unix://$PHONE_SOCK" "$@"; }

# The pids are passed so a client that dies at startup ends the wait at once,
# rather than spending the full patience and reporting the symptom.
if wait_endpoint "$PC_SOCK" "$PC_PID" && wait_endpoint "$PHONE_SOCK" "$PHONE_PID"; then
    ok "both machines have a working docker endpoint at the same time"
else
    bad "an endpoint never came up"
    sed 's/^/    pc: /' "$WORK/pc.log" | tail -20
    sed 's/^/    phone: /' "$WORK/phone.log" | tail -20
    dump_workspace_log 40
    exit 1
fi

# The failure this whole change is about. The second machine used to be refused
# with "tcpip-forward request denied by peer", because one account had one port,
# and the refusal named nothing.
if grep -q "denied by peer" "$WORK/pc.log" "$WORK/phone.log"; then
    bad "a machine was refused its reverse tunnel"
    grep -h "denied by peer" "$WORK/pc.log" "$WORK/phone.log" | head -3
else
    ok "neither machine was refused its reverse tunnel"
fi

echo
echo "== 5. a port each, remembered =="
# Written by the agent beside uidmap: one line per machine, same account.
ports=$(hostdocker exec "$CONTAINER" cat /etc/workspace/clientports 2>/dev/null)
echo "$ports" | sed 's/^/    /'

if [ "$(echo "$ports" | grep -c "^$ACCOUNT:")" -eq 2 ]; then
    ok "the workspace recorded a port for each machine"
else
    bad "the workspace did not record two ports"
fi
if [ "$(echo "$ports" | awk -F: '{print $3}' | sort -u | grep -c .)" -eq 2 ]; then
    ok "the two machines were given different ports"
else
    bad "both machines were given the same port"
fi

echo
echo "== 6. each machine mounts ITS OWN files =="
# The one that matters. Both bind a directory of their own at the same path
# inside the container, and the volume names used to collide on rd-cwd.
pc_saw=$(dpc run --rm -v "$WORK/project-$PC:/w" alpine:3 cat /w/marker 2>&1)
phone_saw=$(dphone run --rm -v "$WORK/project-$PHONE:/w" alpine:3 cat /w/marker 2>&1)

if [ "$pc_saw" = "from the pc" ]; then
    ok "the pc's container read the pc's file"
else
    bad "the pc's container read: $pc_saw"
fi
if [ "$phone_saw" = "from the phone" ]; then
    ok "the phone's container read the phone's file"
else
    bad "the phone's container read: $phone_saw"
fi

echo
echo "== 7. one daemon, so both see the same containers =="
dpc run -d --name shared-by-both alpine:3 sleep 300 >/dev/null 2>&1
if dphone ps --format '{{.Names}}' 2>/dev/null | grep -qx shared-by-both; then
    ok "the phone sees a container the pc started"
else
    bad "the phone cannot see the pc's container"
fi
dpc rm -f shared-by-both >/dev/null 2>&1

echo
echo "== 8. neither machine collects the other's volumes =="
# Each machine's share volume is named for the machine, so a collection on one
# must leave the other's alone. Losing one is not tidy: the daemon recreates a
# missing named volume as an empty local one, so the container comes up with an
# empty directory where the project should be.
before=$(hostdocker exec "$CONTAINER" docker volume ls -q 2>/dev/null | grep -c '^rd-')
(
    cd "$WORK/project-$PC" || exit 1
    REMOTE_DOCKER_STATE_DIR="$WORK/state-$PC" \
    REMOTE_DOCKER_HOST=127.0.0.1 \
    REMOTE_DOCKER_PORT=$SSH_PORT \
    REMOTE_DOCKER_USER="$ACCOUNT" \
        "$WORK/remote-docker" remote gc
) >/dev/null 2>&1
after=$(hostdocker exec "$CONTAINER" docker volume ls -q 2>/dev/null | grep -c '^rd-')

if [ "$before" -ge 2 ] && [ "$after" -ge 1 ]; then
    ok "volumes survive the other machine's collection ($before -> $after)"
else
    bad "a collection on one machine took the other's volumes ($before -> $after)"
fi

# And the phone can still mount, which is what catches a volume having been
# collected and recreated empty.
phone_again=$(dphone run --rm -v "$WORK/project-$PHONE:/w" alpine:3 cat /w/marker 2>&1)
if [ "$phone_again" = "from the phone" ]; then
    ok "the phone still mounts its own directory after the pc collected"
else
    bad "the phone's mount broke after the pc collected: $phone_again"
fi

echo
summary
