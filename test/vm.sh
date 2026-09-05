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
    wait 2>/dev/null
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

# stop_session ends the client and WAITS for it.
#
# Waiting is the point. The endpoint is held until the process is gone, so a
# second session started a tenth of a second later finds it bound and exits
# with "already serving" -- which is the same race `stop && start` is
# documented for, arriving here as "no endpoint in shared mode".
stop_session() {
    [ -z "$CLIENT_PID" ] && return 0
    kill "$CLIENT_PID" 2>/dev/null
    wait "$CLIENT_PID" 2>/dev/null
    CLIENT_PID=
}

# stop_agent ends the agent and waits for its port.
#
# Not a child of this shell -- sudo -b detached it -- so `wait` cannot see it
# and the process table is what there is to ask.
stop_agent() {
    [ -z "$AGENT_PID" ] && return 0
    sudo kill "$AGENT_PID" 2>/dev/null
    for _ in $(seq 1 30); do
        pgrep -f "$WORK/remote-dockerd serve" >/dev/null || break
        sleep 1
    done
    AGENT_PID=
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
# exec, so the subshell BECOMES the client rather than waiting for it. Without
# it `$!` is the subshell's pid, killing that leaves the client running, and the
# next session finds the endpoint held by a process this suite thinks it stopped:
# "another remote-docker is already serving ... (pid N)". The other suites do
# not need it because they have no directory to change into first.
session() {
    local log=$1
    # Watching on, because a delegated share refuses to run without it: its
    # cache holds copies and the watcher is what keeps them honest (ADR 0044).
    # Harmless to every other section, which does not use one.
    (cd "$WORK/project" && exec env REMOTE_DOCKER_WATCH=partial         "$WORK/remote-docker" remote start --foreground >"$log" 2>&1) &
    CLIENT_PID=$!
}

# unions_running counts the fuse-overlayfs servers on this machine.
#
# All of them, because this suite holds one union at a time and the process
# line names the SHARE rather than the directory it came from -- lowerdir,
# upperdir and the merged path are all under /run/rd-union/<id>, and the id is
# a digest nothing here has to hand.
unions_running() {
    sudo pgrep -c fuse-overlayfs 2>/dev/null || true
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

# And what a union needs, which in shared mode is on this machine rather than
# inside a per-account dind. Reported the same way and for the same reason: a
# section 5b that skips says so here first.
if command -v fuse-overlayfs >/dev/null; then
    ok "fuse-overlayfs, so a delegated share can be a union here"
    HAVE_FUSE_OVERLAY=true
else
    info "no fuse-overlayfs here; a delegated share cannot be mounted on this machine"
    HAVE_FUSE_OVERLAY=false
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

stop_session
stop_agent
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

    if [ "$HAVE_FUSE_OVERLAY" != true ]; then
        info "skipped: no fuse-overlayfs here, so there is no union to adopt"
    else
        echo
        echo "== 5b. a union, and an agent restart underneath it =="
        # The deployment where an orphaned union can happen at all. With the
        # agent in a container it is pid 1, so restarting it takes its dockerd
        # and every dind with it and nothing is left to adopt; here the union
        # server outlives the agent that started it (ADR 0025, ADR 0044).
        #
        # In SHARED mode, where the union mounts in the agent's own namespace
        # and so needs fuse-overlayfs on THIS machine rather than inside a
        # per-account dind. Stock docker:dind has not got it, which is why
        # section 4 cannot ask this at all (agent/internal/daemons/plan.go:38).
        #
        # A directory of its own, because one directory is one share and one
        # CONSISTENCY (ADR 0042): asking for a delegated mount of the one
        # section 5 already mounted plainly is refused, correctly.
        UNIONDIR="$WORK/uniondir"
        mkdir -p "$UNIONDIR"
        echo "served from the machine" >"$UNIONDIR/marker"
        if timeout 300 "$WORK/remote-docker" run -d --name vm-deleg         -v "$UNIONDIR:/w:read=cached,write=back" alpine:3 sleep 600 >"$WORK/deleg.log" 2>&1; then
            ok "a container starts against a delegated union"

            if out=$(timeout 60 "$WORK/remote-docker" exec vm-deleg             sh -c 'grep " /w " /proc/mounts' 2>&1) && echo "$out" | grep -q fuse; then
                ok "its share is a union rather than a directory that resembles one"
            else
                bad "/w is not a fuse mount: $(echo "$out" | tail -2 | tr -s '[:space:]' ' ')"
                dump_agent_log false
            fi

            # BEFORE the restart, so a failure afterwards says which half is
            # broken. Without it, "the held share stopped working" cannot be
            # told from a union that never served the file at all.
            if out=$(timeout 60 "$WORK/remote-docker" exec vm-deleg cat /w/marker 2>&1) &&
                echo "$out" | grep -q "served from the machine"; then
                ok "it reads this machine's file through the union"
            else
                bad "the union served nothing before any restart: $(echo "$out" | tail -2 | tr -s '[:space:]' ' ')"
                dump_agent_log false
            fi

            before=$(unions_running)
            stop_agent
            if start_agent false; then
                ok "the agent came back with the daemon still running under it"
            else
                bad "the agent did not come back"
                dump_agent_log false
            fi

            # A NEW container, because that is what asks the agent to prepare the share
            # again. Without adoption the supervisor mounts a second fuse-overlayfs on
            # the same path, over the same upper and work directories -- which
            # overlayfs does not allow and which nothing else here would notice.
            if timeout 300 "$WORK/remote-docker" run --rm             -v "$UNIONDIR:/w:read=cached,write=back" alpine:3 cat /w/marker >"$WORK/deleg2.log" 2>&1; then
                ok "a second container prepares the same share after the restart"
            else
                bad "the share could not be prepared again: $(tail -2 "$WORK/deleg2.log" | tr -s '[:space:]' ' ')"
                dump_agent_log false
            fi

            after=$(unions_running)
            if [ "$before" = 1 ] && [ "$after" = 1 ]; then
                ok "exactly one union server for the share, before and after the restart"
            else
                bad "union servers for the share: $before before the restart, $after after"
                sudo pgrep -af fuse-overlayfs 2>/dev/null | sed 's/^/        /'
            fi

            # And the container that held it throughout still reads through its mount.
            if out=$(timeout 60 "$WORK/remote-docker" exec vm-deleg cat /w/marker 2>&1) &&
                echo "$out" | grep -q "served from the machine"; then
                ok "the container held its share across the agent restart"
            else
                bad "the held share stopped working: $(echo "$out" | tail -2 | tr -s '[:space:]' ' ')"
                dump_agent_log false
            fi
            timeout 60 "$WORK/remote-docker" rm -f vm-deleg >/dev/null 2>&1
        else
            bad "no container against a delegated union: $(tail -3 "$WORK/deleg.log" | tr -s '[:space:]' ' ')"
            dump_agent_log false
        fi
    fi
fi

summary
