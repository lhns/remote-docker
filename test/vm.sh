#!/usr/bin/env bash
#
# The agent on a machine, not in a container (ADR 0025).
#
# The claim is narrow and worth stating exactly: the same binary, with
# WORKSPACE_ENABLE_DIND=false because this machine already has a dockerd,
# serves the same workspace. There is no VM mode and no second code path --
# both daemon modes read the switch they always read, and this runs each.
#
# The runner IS the machine, which is what makes this testable at all, and why
# it is worth having: every other "this works on X" in the project either runs
# in CI or says plainly that it does not.
#
# What this does NOT prove: any distro but Ubuntu, any docker but the runner's,
# and systemd. The unit file is not exercised here -- what is under test is the
# agent as a guest, not systemd's ability to run a binary.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
SSH_PORT=${SSH_PORT:-2299}
ACCOUNT=vmtest
AGENT_PID=
CLIENT_PID=

# shellcheck source=lib.sh
. "$REPO/test/lib.sh"

cleanup() {
    [ -n "$CLIENT_PID" ] && kill "$CLIENT_PID" 2>/dev/null
    [ -n "$AGENT_PID" ] && sudo kill "$AGENT_PID" 2>/dev/null
    # The per-account daemon outlives the agent deliberately -- it holds
    # somebody's containers (ADR 0019) -- so this suite takes its own away.
    hostdocker rm -f "rd-dind-$ACCOUNT" >/dev/null 2>&1
    hostdocker volume rm -f "rd-dind-$ACCOUNT-lib" >/dev/null 2>&1
    sudo userdel -r "rd-$ACCOUNT" >/dev/null 2>&1
    sudo rm -rf "$WORK"
}
trap cleanup EXIT

# start_agent runs the agent as root, in the background, with its output where
# this suite can print it.
start_agent() {
    local per_user_dind=$1
    # SC2024: the redirect is the CALLING user's on purpose. The log has to be
    # readable by this suite, which does not run as root.
    # shellcheck disable=SC2024
    sudo -b env \
        WORKSPACE_ENABLE_DIND=false \
        WORKSPACE_PER_USER_DIND="$per_user_dind" \
        WORKSPACE_STATE_DIR="$WORK/wsstate" \
        WORKSPACE_KEYS_DIR="$WORK/keys" \
        WORKSPACE_HOSTKEY_DIR="$WORK/wsstate/host_keys" \
        "$WORK/remote-dockerd" serve --addr ":$SSH_PORT" \
        >"$WORK/agent-$per_user_dind.log" 2>&1
    for _ in $(seq 1 30); do
        AGENT_PID=$(pgrep -f "$WORK/remote-dockerd serve" | head -1)
        [ -n "$AGENT_PID" ] && return 0
        sleep 1
    done
    return 1
}

# wait_unix_account waits for the unix user behind an account, which is
# `rd-<account>` and not the account name.
wait_unix_account() {
    for _ in $(seq 1 60); do
        id "rd-$1" >/dev/null 2>&1 && return 0
        sleep 1
    done
    return 1
}

dump_agent_log() {
    echo "== agent log =="
    tail -"${2:-40}" "$WORK/agent-$1.log" 2>/dev/null | sed 's/^/        /'
}

# session starts a client in the background against the configured endpoint.
#
# The workspace comes from the environment rather than from flags, as it does
# in the other suites, so every client command sees it and not only the one
# that opens the session. REMOTE_DOCKER_ENDPOINT is a PATH and not a URL: the
# client puts the unix:// on itself, and passing one produced a DOCKER_HOST of
# `unix://unix:///...` and a socket nothing was ever going to find.
session() {
    local log=$1
    (cd "$WORK/project" && "$WORK/remote-docker" remote start --foreground >"$log" 2>&1) &
    CLIENT_PID=$!
}

echo "== 1. what this machine provides =="
if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
    ok "a docker engine with its CLI on PATH"
else
    bad "no usable docker on this machine; nothing below can work"
    summary
fi

if command -v useradd >/dev/null; then
    ok "the shadow tools"
else
    bad "no useradd, so the agent cannot provision accounts"
fi

# Only shared mode needs this, because only shared mode mounts NFS on the
# machine itself. Reported either way, so a failure in section 5 arrives with
# its cause already on screen rather than as a volume that will not mount.
if command -v mount.nfs >/dev/null; then
    ok "an NFS client, so shared-daemon mode can mount"
    HAVE_NFS=true
else
    info "no NFS client here; shared-daemon mode cannot mount on this machine"
    HAVE_NFS=false
fi

echo
export REMOTE_DOCKER_STATE_DIR="$WORK/state"
export REMOTE_DOCKER_HOST=127.0.0.1
export REMOTE_DOCKER_PORT=$SSH_PORT
export REMOTE_DOCKER_USER=$ACCOUNT
export REMOTE_DOCKER_ENDPOINT="$WORK/docker.sock"

echo "== 2. build both binaries =="
mkdir -p "$WORK/keys" "$WORK/wsstate/host_keys" "$WORK/state" "$WORK/project"
echo "served from the machine" >"$WORK/project/marker"

if (cd "$REPO/agent" && CGO_ENABLED=0 go build -o "$WORK/remote-dockerd" ./cmd/remote-dockerd); then
    ok "the agent builds"
else
    bad "the agent did not build"
    summary
fi
if build_client; then
    ok "the client builds"
else
    bad "the client did not build"
    summary
fi

echo
echo "== 3. a daemon per account, which is the default =="
if start_agent true; then
    ok "the agent started on this machine, with no container around it"
else
    bad "the agent did not start"
    dump_agent_log true
    summary
fi

if enrol "$ACCOUNT" "$WORK/state"; then
    ok "enrolled a key"
else
    bad "could not enrol"
    summary
fi

if wait_unix_account "$ACCOUNT"; then
    ok "the agent provisioned rd-$ACCOUNT on this machine"
else
    bad "no unix account appeared"
    dump_agent_log true
    summary
fi

# The account name is ours; the unix name is not. The client logs in as
# `vmtest` and the machine knows `rd-vmtest` (ADR 0025).
if id "$ACCOUNT" >/dev/null 2>&1; then
    bad "the account took the bare name in this machine's passwd file"
else
    ok "the machine's own namespace is untouched by the account name"
fi

echo
echo "== 4. a session, and a bind mount through it =="
session "$WORK/client.log"

if wait_endpoint "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID"; then
    ok "the client reached the agent and served an endpoint"
else
    bad "no endpoint; the session never came up"
    tail -30 "$WORK/client.log" | sed 's/^/        /'
    dump_agent_log true
    summary
fi

# The whole point of the project, against an agent with no container around it.
if out=$(cd "$WORK/project" && timeout 300 \
        "$WORK/remote-docker" run --rm -v "$WORK/project:/w" alpine:3 cat /w/marker 2>&1) &&
    echo "$out" | grep -q "served from the machine"; then
    ok "a bind mount resolved through NFS to this machine's own directory"
else
    bad "the bind mount did not resolve: $(echo "$out" | tail -3 | tr '\n' ' ')"
fi

if out=$(timeout 60 "$WORK/remote-docker" remote status 2>&1) &&
    echo "$out" | grep -q "^status"; then
    ok "remote status answers against a machine workspace"
else
    bad "status failed: $(echo "$out" | tail -2 | tr '\n' ' ')"
fi

kill "$CLIENT_PID" 2>/dev/null
CLIENT_PID=
sudo kill "$AGENT_PID" 2>/dev/null
AGENT_PID=
hostdocker rm -f "rd-dind-$ACCOUNT" >/dev/null 2>&1

echo
echo "== 5. one shared daemon, which is this machine's own =="
# The mode where the machine itself mounts NFS, so also the one that needs a
# client. Skipped rather than failed when there is none: the absence is a
# property of the runner and section 1 has already said so.
if [ "$HAVE_NFS" != true ]; then
    info "skipped: no NFS client on this machine"
else
    if start_agent false; then
        ok "the agent started in shared-daemon mode"
    else
        bad "the agent did not start in shared mode"
        dump_agent_log false
    fi

    session "$WORK/client2.log"

    if wait_endpoint "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID"; then
        ok "a session against the machine's own daemon"
    else
        bad "no endpoint in shared mode"
        tail -30 "$WORK/client2.log" | sed 's/^/        /'
        dump_agent_log false
    fi

    if out=$(cd "$WORK/project" && timeout 300 \
            "$WORK/remote-docker" run --rm -v "$WORK/project:/w" alpine:3 cat /w/marker 2>&1) &&
        echo "$out" | grep -q "served from the machine"; then
        ok "a bind mount resolved with the machine's own daemon mounting it"
    else
        bad "the shared-mode bind mount did not resolve: $(echo "$out" | tail -3 | tr '\n' ' ')"
    fi
fi

summary
