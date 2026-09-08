#!/usr/bin/env bash
# THIS client against a PUBLISHED OLDER WORKSPACE, which is the only way the
# compatibility claim can be checked at all.
#
# Every other suite builds both ends from this tree, so both always know every
# command the other speaks. The failure this exists for is the opposite: a
# client asking for a channel the workspace has never heard of. A 0.6.0 client
# against a 0.5.1 workspace HUNG, printing nothing, because the agent ran
# `workspace-cache` as a shell command and then blocked reading a stdin the
# client never closed, while the client blocked reading a greeting nobody would
# send.
#
# The workspace is PULLED rather than built. Building v0.5.1 from its tag would
# prove something about this checkout; pulling the image proves something about
# what is deployed, which is what the report was about.
#
# WORKSPACE_IMAGE overrides the tag, so the same suite answers the question
# again for whatever the oldest supported workspace becomes.
#
# Requires: docker, a kernel with NFS client support, and network access to
# ghcr.io.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
IMAGE=${WORKSPACE_IMAGE:-ghcr.io/lhns/remote-docker-workspace:0.5.1}
CONTAINER=remote-docker-oldworkspace
SSH_PORT=22226
ACCOUNT=alice

DOCKER_TIMEOUT=180

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

cleanup() { cleanup_suite "${CLIENT_PID:-}"; }
trap cleanup EXIT

echo "== 1. this client, that workspace =="
if build_client; then
    ok "client builds"
else
    bad "client build failed"
    exit 1
fi

if hostdocker pull "$IMAGE" >/dev/null 2>&1; then
    ok "pulled $IMAGE"
else
    bad "could not pull $IMAGE"
    exit 1
fi

echo
echo "== 2. a session against it =="
mkdir -p "$WORK/keys" "$WORK/wsstate" "$WORK/project"
echo "written on this machine" >"$WORK/project/marker"

if ! enrol "$ACCOUNT" "$WORK/state"; then
    bad "could not enrol"
    exit 1
fi

# The shared daemon: the oldest deployment shape, and one fewer variable in
# every failure below.
if ! start_workspace false; then
    bad "workspace container failed to start"
    exit 1
fi
if ! wait_provisioned "$ACCOUNT"; then
    bad "the account was never provisioned"
    dump_workspace_log 40
    exit 1
fi
wait_parent_dockerd

SOCK="$WORK/client.sock"
CLIENT_PID=$(start_session "$WORK/state" "$ACCOUNT" "$SOCK" "$WORK/client.log" "$WORK/project")

# The whole bug, measured: the client used to reach this point and stop. It has
# a deadline now, so the endpoint comes up whether or not the workspace serves
# the cache channel.
if wait_endpoint "$SOCK" "$CLIENT_PID"; then
    ok "the endpoint came up against a workspace that does not serve the cache channel"
else
    bad "the endpoint never came up"
    tail -20 "$WORK/client.log" | sed 's/^/    /'
    dump_workspace_log 40
    exit 1
fi

d() { dockerat "$SOCK" "$@"; }
info "pulling the test image"
d pull -q alpine:3 >/dev/null 2>&1 || info "could not pre-pull alpine:3"

echo
echo "== 3. ordinary mounts work, and nothing is said about a cache =="
saw=$(d run --rm -v "$WORK/project:/w" alpine:3 cat /w/marker 2>&1 | tail -1)
if [ "$saw" = "written on this machine" ]; then
    ok "a write=through mount reads this machine's file"
else
    bad "the container read: $saw"
fi

# The user's constraint, made testable. A workspace serving every mount it was
# given is not doing anything wrong, and must not be told about a capability
# nobody asked for.
if grep -qiE "cache channel|does not serve" "$WORK/client.log"; then
    bad "the client said something about the cache channel anyway"
    grep -iE "cache channel|does not serve" "$WORK/client.log" | head -3 | sed 's/^/    /'
else
    ok "nothing was said about the cache channel"
fi

echo
echo "== 4. a mount that needs the cache is refused, naming itself =="
# Refused rather than quietly served as write=through: a silent downgrade moves
# where somebody's writes live.
#
# Bounded by the docker timeout, so a client that hangs here fails the suite
# instead of running out the job's whole budget.
start=$(date +%s)
outputs 'asks for write=back' d run --rm -v "$WORK/project:/w:read=cached,write=back" alpine:3 true
refused=$?
took=$(( $(date +%s) - start ))
echo "$LAST_OUTPUT" | sed 's/^/    /'

if [ "$refused" -eq 0 ]; then
    ok "the mount was refused, naming the mode it asked for"
else
    bad "the mount was not refused by name"
fi
if grep -qE 'fix: update the workspace' <<<"$LAST_OUTPUT"; then
    ok "the refusal carries a remedy"
else
    bad "the refusal carries no remedy"
fi
# Context, never the test: no version comparison gates anything, and the
# workspace's own version is reported because it is what somebody acts on.
if grep -qE 'remote-dockerd [0-9]' <<<"$LAST_OUTPUT"; then
    ok "the refusal names the workspace's version as context"
else
    info "the workspace reported no version, so the refusal named none"
fi
if [ "$took" -lt 60 ]; then
    ok "the refusal took ${took}s"
else
    bad "the refusal took ${took}s, which is a hang wearing a deadline"
fi

summary
