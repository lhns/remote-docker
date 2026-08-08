#!/usr/bin/env bash
# A daemon per account (ADR 0019), end to end, with TWO accounts.
#
# Separate from integration.sh rather than folded into it, because the claim
# being tested needs a second enrolled account and a differently configured
# workspace -- and because integration.sh must keep passing UNCHANGED in shared
# mode, which is the escape hatch. Two scripts prove both modes; one script
# with a flag would prove whichever branch it happened to take.
#
# The claim is narrow and worth stating exactly: accounts stop seeing each
# other's containers. It is NOT isolation. Each per-account daemon runs
# privileged, so a determined account can still break out and reach another's;
# what changes is that nobody does so by accident.
#
# Requires: docker, and a kernel with NFS client support.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-peruser
SSH_PORT=22223

# Two accounts, which is the whole point of this file.
A=alice
B=bob

DOCKER_TIMEOUT=180

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

cleanup() {
    echo
    echo "== cleanup =="
    for pid in ${CLIENT_A_PID:-} ${CLIENT_B_PID:-}; do
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
echo "== 2. enrol two accounts =="
mkdir -p "$WORK/keys" "$WORK/wsstate"
for account in "$A" "$B"; do
    enrol "$account" "$WORK/state-$account" || { bad "no key generated for $account"; exit 1; }
done
ok "two keypairs staged as $A.pub and $B.pub"

echo
echo "== 3. start the workspace with a daemon per account =="
# true, written here rather than defaulted in the library: this suite exists to
# test that mode, and it says so in its own file.
if start_workspace true; then
    ok "workspace container started with WORKSPACE_PER_USER_DIND=true"
else
    bad "workspace container failed to start"
    exit 1
fi

info "waiting for both accounts to be provisioned"
if wait_provisioned "$A" "$B"; then
    ok "both accounts provisioned"
else
    bad "the accounts were never provisioned"
    dump_workspace_log 40
    exit 1
fi

# The shared `docker` group grants a socket reaching the PARENT daemon, which
# holds every account's dind. In this mode nobody may be in it, or the
# separation ends at the first shell.
for account in "$A" "$B"; do
    if hostdocker exec "$CONTAINER" id -nG "$account" 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
        bad "$account is still in the docker group; it can reach the parent daemon"
    else
        ok "$account is not in the docker group"
    fi
done

info "waiting for the parent dockerd"
wait_parent_dockerd

echo
echo "== 4. a session each =="
mkdir -p "$WORK/project-$A" "$WORK/project-$B"
echo "alice's file" >"$WORK/project-$A/marker"
echo "bob's file"   >"$WORK/project-$B/marker"

start_session() {
    local account=$1 endpoint=$2 log=$3 dir=$4
    (
        cd "$dir" || exit 1
        REMOTE_DOCKER_STATE_DIR="$WORK/state-$account" \
        REMOTE_DOCKER_HOST=127.0.0.1 \
        REMOTE_DOCKER_PORT=$SSH_PORT \
        REMOTE_DOCKER_USER="$account" \
        REMOTE_DOCKER_ENDPOINT="$endpoint" \
        REMOTE_DOCKER_IDLE_TIMEOUT=8s \
        "$WORK/remote-docker" start --foreground
    ) >"$log" 2>&1 &
    echo $!
}

# Each account's CURRENT endpoint, tracked rather than written out at each use.
#
# Sessions get restarted below, onto new sockets, and a helper pinned to the
# first one keeps answering -- with nothing, silently, because a dead socket
# reads as an empty result rather than an error. That produced a baseline of []
# and an assertion that failed while the thing it tested was working.
A_SOCK="$WORK/a.sock"
B_SOCK="$WORK/b.sock"

CLIENT_A_PID=$(start_session "$A" "$A_SOCK" "$WORK/a.log" "$WORK/project-$A")
CLIENT_B_PID=$(start_session "$B" "$B_SOCK" "$WORK/b.log" "$WORK/project-$B")

# A cold dind has to be pulled and booted, which is the slowest thing here.
#
# The client pids are passed so a client that dies at startup ends the wait
# immediately. Without that this suite spent its full patience and then
# reported a timeout, naming the symptom instead of the cause.
if wait_endpoint "$A_SOCK" "$CLIENT_A_PID" && wait_endpoint "$B_SOCK" "$CLIENT_B_PID"; then
    ok "both accounts have a working docker endpoint"
else
    bad "an endpoint never came up"
    sed 's/^/    A: /' "$WORK/a.log" | tail -20
    sed 's/^/    B: /' "$WORK/b.log" | tail -20
    dump_workspace_log 40
    exit 1
fi

da() { timeout "$DOCKER_TIMEOUT" docker -H "unix://$A_SOCK" "$@"; }
db() { timeout "$DOCKER_TIMEOUT" docker -H "unix://$B_SOCK" "$@"; }

# Pulled per account, because a daemon per account means a layer cache per
# account -- which is the cost this design accepts, and it shows up here first:
# an unpulled image put "Unable to find image locally" into the output an
# assertion was reading.
info "pulling test images into each account's daemon"
for image in alpine:3 nginx:alpine; do
    da pull -q "$image" >/dev/null 2>&1 || info "could not pre-pull $image for $A"
    db pull -q "$image" >/dev/null 2>&1 || info "could not pre-pull $image for $B"
done

echo
echo "== 5. the daemons really are different =="
ida=$(da info --format '{{.ID}}' 2>/dev/null)
idb=$(db info --format '{{.ID}}' 2>/dev/null)
if [ -n "$ida" ] && [ "$ida" != "$idb" ]; then
    ok "each account is talking to a different daemon"
else
    bad "both accounts reached the same daemon ($ida)"
fi

echo
echo "== 6. one account cannot see the other's containers =="
# THE assertion. Everything else in this file supports it.
if da run -d --name alice-secret alpine:3 sleep 300 >/dev/null 2>&1; then
    ok "alice started a container"
else
    bad "alice could not start a container"
fi

if db ps --all --format '{{.Names}}' 2>/dev/null | grep -qx alice-secret; then
    bad "bob can see alice's container -- the accounts are not separated"
else
    ok "bob cannot see alice's container"
fi

# And cannot reach it by name either, which is the operation somebody would
# actually try.
if db stop alice-secret >/dev/null 2>&1; then
    bad "bob stopped alice's container"
else
    ok "bob cannot stop alice's container"
fi

if da ps --format '{{.Names}}' 2>/dev/null | grep -qx alice-secret; then
    ok "alice still sees her own"
else
    bad "alice lost sight of her own container"
fi

echo
echo "== 7. a bind mount resolves, which proves the in-netns NFS listener =="
# The reverse tunnel is bound INSIDE each account's dind. If that were wrong,
# the volume would fail to mount and this reads the file rather than guessing.
#
# stderr is captured, not discarded. It was briefly sent to /dev/null to keep
# image-pull noise out of $out -- which also emptied the failure message, so a
# broken mount reported "failed:" and nothing else. The images are pre-pulled
# above instead, which removes the noise at its source.
if out=$(da run --rm -v "$WORK/project-$A:/w" alpine:3 cat /w/marker 2>&1); then
    if [ "$out" = "alice's file" ]; then
        ok "alice's bind mount resolves through her own daemon"
    else
        bad "alice's bind mount gave [$out]"
    fi
else
    bad "alice's bind mount failed: $(echo "$out" | tail -3)"
fi

if out=$(db run --rm -v "$WORK/project-$B:/w" alpine:3 cat /w/marker 2>&1); then
    if [ "$out" = "bob's file" ]; then
        ok "bob's bind mount resolves through his own daemon"
    else
        bad "bob's bind mount gave [$out]"
    fi
else
    bad "bob's bind mount failed: $(echo "$out" | tail -3)"
fi

echo
echo "== 8. both accounts publish the same port =="
# Impossible on a shared daemon: the second bind collides. Two namespaces make
# it ordinary.
if da run -d --name alice-web -p 18090:80 nginx:alpine >/dev/null 2>&1 &&
   db run -d --name bob-web   -p 18090:80 nginx:alpine >/dev/null 2>&1; then
    ok "both accounts published 8080 without colliding"
else
    bad "publishing the same port from both accounts failed"
    da logs alice-web 2>&1 | tail -5
    db logs bob-web 2>&1 | tail -5
fi

echo
echo "== 9. a shell points at its own daemon, AND CAN USE IT =="
# Asserting the variable was set is what let a real bug ship: /run/rd was
# created 0750 root:root, so every account's DOCKER_HOST named a socket behind
# a directory it could not enter. The variable was perfect and `docker ps` in a
# shell said "permission denied while trying to connect to the Docker daemon
# socket". So the shell is made to actually USE it.
shell_out=$(timeout 90 ssh -i "$WORK/state-$A/id_ed25519"     -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null     -o BatchMode=yes -p "$SSH_PORT" "$A@127.0.0.1"     'echo "HOST=$DOCKER_HOST"; docker ps --format "{{.Names}}" 2>&1 | head -5'     2>/dev/null </dev/null | tr -d '\015')

case "$shell_out" in
    *"HOST=unix:///run/rd/$A/docker.sock"*)
        ok "a shell's DOCKER_HOST is that account's own daemon" ;;
    *"HOST="*)
        bad "a shell's DOCKER_HOST is $(echo "$shell_out" | head -1)" ;;
    *)
        bad "a shell got no DOCKER_HOST; it would find the parent daemon" ;;
esac

# alice-secret is running on her daemon by now, so her own shell must see it.
# Any permission problem reaching the socket shows up here instead.
if echo "$shell_out" | grep -q "permission denied"; then
    bad "the account cannot reach its own docker socket: $(echo "$shell_out" | tail -1)"
elif echo "$shell_out" | grep -qx alice-secret; then
    ok "and a shell can actually use it"
else
    bad "a shell could not list its own containers: $(echo "$shell_out" | tail -2 | tr '
' ' ')"
fi

# The storage driver, which is the difference between `docker run` taking a
# second and taking two minutes.
#
# vfs has no copy-on-write and copies the whole image on every create. dockerd
# picks it silently when the graph filesystem refuses overlay2 -- which is what
# a Ceph- or NFS-backed data directory does -- so nothing fails and everything
# is slow. A real workspace hit exactly this.
driver=$(da info --format '{{.Driver}}' 2>/dev/null)
if [ -z "$driver" ]; then
    bad "could not read the storage driver from $A's daemon"
elif [ "$driver" = "vfs" ]; then
    bad "$A's daemon is on vfs; every container create copies the whole image"
else
    ok "$A's daemon is on a copy-on-write storage driver ($driver)"
fi

echo
echo "== 10. the workspace restarts and the daemons come back =="
# The case that would otherwise lose everybody's work: the agent comes back,
# finds every daemon's name taken, and `docker run --name` conflicts rather
# than replacing.
#
# What survives is deliberately stated as CONTAINERS EXISTING, not running.
# A restarted dockerd starts only containers with a restart policy, and the
# account's own containers do not have one -- that is the account's business.
# The daemon itself does, so it comes back and its graph comes back with it.
before=$(da ps --all --format '{{.Names}}' 2>/dev/null | sort | tr '
' ' ')
dind_before=$(hostdocker exec "$CONTAINER" docker inspect "rd-dind-$A" --format '{{.Id}}' 2>/dev/null)

kill "$CLIENT_A_PID" 2>/dev/null; wait "$CLIENT_A_PID" 2>/dev/null; CLIENT_A_PID=""
kill "$CLIENT_B_PID" 2>/dev/null; wait "$CLIENT_B_PID" 2>/dev/null; CLIENT_B_PID=""

hostdocker restart "$CONTAINER" >/dev/null 2>&1
info "waiting for the agent and the daemons to come back"
for _ in $(seq 1 120); do
    if hostdocker exec "$CONTAINER" docker inspect "rd-dind-$A"             --format '{{.State.Status}}' 2>/dev/null | grep -qx running; then
        break
    fi
    sleep 1
done

# Asserted as "exactly one", not by grepping a log line.
#
# A duplicate is the failure adoption exists to prevent, and the log message
# is an implementation detail that was also a race: the agent runs Adopt the
# moment it starts, while the parent dockerd is still bringing the daemons
# back up, so it legitimately finds nothing running to adopt and Ensure does
# the work on demand instead. The outcome is what the design promises.
count=$(hostdocker exec "$CONTAINER" docker ps --all     --filter "name=^/rd-dind-$A$" --format '{{.Names}}' 2>/dev/null | grep -c .)
if [ "$count" = "1" ]; then
    ok "exactly one daemon for $A after the restart, not a second one beside it"
else
    bad "$count daemons named rd-dind-$A after the restart"
    hostdocker exec "$CONTAINER" docker ps --all --format '{{.Names}} {{.Status}}' 2>&1 | tail -10
fi

A_SOCK="$WORK/a2.sock"
CLIENT_A_PID=$(start_session "$A" "$A_SOCK" "$WORK/a2.log" "$WORK/project-$A")
if ! wait_endpoint "$A_SOCK" "$CLIENT_A_PID"; then
    bad "alice's endpoint never came back after the restart"
    # The client's own log, which is where the reason is. Without it this
    # failure reads as "slow" and costs a CI round trip to learn otherwise.
    sed 's/^/    A: /' "$WORK/a2.log" | tail -20
    dump_workspace_log 40
fi

after=$(da ps --all --format '{{.Names}}' 2>/dev/null | sort | tr '
' ' ')
if [ -n "$before" ] && [ "$before" = "$after" ]; then
    ok "alice's containers survived the restart"
else
    bad "alice's containers changed across the restart: [$before] -> [$after]"
fi

# Reused, not replaced. A new id would mean the old daemon was abandoned with
# everything in it, which is what adoption exists to prevent.
dind_after=$(hostdocker exec "$CONTAINER" docker inspect "rd-dind-$A" --format '{{.Id}}' 2>/dev/null)
if [ -n "$dind_before" ] && [ "$dind_before" = "$dind_after" ]; then
    ok "the same daemon container was reused, not replaced"
else
    bad "alice's daemon was replaced: [$dind_before] -> [$dind_after]"
fi

echo
echo "== 11. the account's storage outlives its daemon container =="
# The persistence promise, stated exactly: the graph volume is named and
# labelled so the container in front of it is disposable. An upgrade removes
# and recreates that container; if the storage were anonymous, every account's
# work would go with it.
if hostdocker exec "$CONTAINER" docker volume inspect "rd-dind-$A-lib"         --format '{{index .Labels "remote-docker.daemon"}}' 2>/dev/null | grep -qx 1; then
    ok "the graph volume is labelled, so an operator can see what must not be pruned"
else
    bad "the graph volume carries no label; a prune would take it with nothing naming it"
fi

images_before=$(da images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | sort | tr '
' ' ')

# Remove the daemon CONTAINER, keeping the volume -- which is what an upgrade
# does, and what adoption does after a redeploy.
kill "$CLIENT_A_PID" 2>/dev/null; wait "$CLIENT_A_PID" 2>/dev/null; CLIENT_A_PID=""
hostdocker exec "$CONTAINER" docker rm -f "rd-dind-$A" >/dev/null 2>&1

A_SOCK="$WORK/a3.sock"
CLIENT_A_PID=$(start_session "$A" "$A_SOCK" "$WORK/a3.log" "$WORK/project-$A")
wait_endpoint "$A_SOCK" "$CLIENT_A_PID" || bad "alice's endpoint never came back after her daemon was destroyed"

images_after=$(da images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | sort | tr '
' ' ')
if [ -n "$images_before" ] && [ "$images_before" = "$images_after" ]; then
    ok "alice's images survived her daemon container being destroyed"
else
    bad "alice's images did not survive: [$images_before] -> [$images_after]"
fi

if da ps --all --format '{{.Names}}' 2>/dev/null | grep -qx alice-secret; then
    ok "and so did her containers"
else
    bad "alice's containers did not survive her daemon being recreated"
fi

echo
if [ "$FAIL" -ne 0 ]; then
    dump_workspace_log
fi

summary
