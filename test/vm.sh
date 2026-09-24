#!/usr/bin/env bash
#
# The agent on a machine, not in a container (ADR 0025).
#
# The same binary, with WORKSPACE_ENABLE_DIND=false because the runner already
# has a dockerd, in both daemon modes. There is no VM mode.
#
# NOT proven: any distro but Ubuntu, any docker but the runner's, and systemd.
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
    # The per-account daemon outlives the agent deliberately (ADR 0019).
    hostdocker rm -f "rd-dind-$ACCOUNT" >/dev/null 2>&1
    hostdocker volume rm -f "rd-dind-$ACCOUNT-lib" >/dev/null 2>&1
    sudo userdel -r "rd-$ACCOUNT" >/dev/null 2>&1
    sudo rm -rf "$WORK"
}
trap cleanup EXIT

start_agent() {
    local per_user_dind=$1
    # SC2024: the redirect is the CALLING user's, so this suite can read the log.
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

# wait_unix_account waits for `rd-<account>`, not the account name.
wait_unix_account() {
    for _ in $(seq 1 60); do
        id "rd-$1" >/dev/null 2>&1 && return 0
        sleep 1
    done
    return 1
}

# stop_session WAITS for the client: the endpoint is held until the process is
# gone, and the next session would exit with "already serving".
stop_session() {
    [ -z "$CLIENT_PID" ] && return 0
    kill "$CLIENT_PID" 2>/dev/null
    wait "$CLIENT_PID" 2>/dev/null
    CLIENT_PID=
}

# stop_agent polls the process table: sudo -b detached the agent, so `wait`
# cannot see it.
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

# session starts a client in the background, configured from the environment
# so every client command sees it. REMOTE_DOCKER_ENDPOINT is a PATH, not a URL:
# the client adds unix:// itself. exec and watching as in lib.sh's
# start_session.
session() {
    local log=$1
    (cd "$WORK/project" && exec env REMOTE_DOCKER_WATCH=partial \
        "$WORK/remote-docker" remote start --foreground >"$log" 2>&1) &
    CLIENT_PID=$!
}

# rd runs one client command with a timeout; also union_is_fuse's exec-fn.
rd() { timeout 60 "$WORK/remote-docker" "$@"; }

# unions_running counts ALL fuse-overlayfs servers: this suite holds one union
# at a time, and each names only /run/rd-union/<client>/<id>, a digest.
unions_running() {
    sudo pgrep -c fuse-overlayfs 2>/dev/null || true
}

echo "== 1. what this machine provides =="
if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
    ok "a docker engine with its CLI on PATH"
else
    bad "no usable docker on this machine; nothing below can work"
    summary
    exit 1
fi

if command -v useradd >/dev/null; then
    ok "the shadow tools"
else
    bad "no useradd, so the agent cannot provision accounts"
fi

# Only shared mode mounts NFS (and a union) on the machine itself; reported
# here so a skip in section 5 or 5b has its cause on screen.
if command -v mount.nfs >/dev/null; then
    ok "an NFS client, so shared-daemon mode can mount"
    HAVE_NFS=true
else
    info "no NFS client here; shared-daemon mode cannot mount on this machine"
    HAVE_NFS=false
fi

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
    exit 1
fi
if build_client; then
    ok "the client builds"
else
    bad "the client did not build"
    summary
    exit 1
fi

echo
echo "== 3. a daemon per account, which is the default =="
if start_agent true; then
    ok "the agent started on this machine, with no container around it"
else
    bad "the agent did not start"
    dump_agent_log true
    summary
    exit 1
fi

if enrol "$ACCOUNT" "$WORK/state"; then
    ok "enrolled a key"
else
    bad "could not enrol"
    summary
    exit 1
fi

if wait_unix_account "$ACCOUNT"; then
    ok "the agent provisioned rd-$ACCOUNT on this machine"
else
    bad "no unix account appeared"
    dump_agent_log true
    summary
    exit 1
fi

# The client logs in as `vmtest`; the machine knows `rd-vmtest` (ADR 0025).
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
    exit 1
fi

if out=$(cd "$WORK/project" && timeout 300 \
        "$WORK/remote-docker" run --rm -v "$WORK/project:/w" alpine:3 cat /w/marker 2>&1) &&
    echo "$out" | grep -q "served from the machine"; then
    ok "a bind mount resolved through NFS to this machine's own directory"
else
    bad "the bind mount did not resolve: $(echo "$out" | tail -3 | tr '\n' ' ')"
fi

if out=$(rd remote status 2>&1) &&
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
# Skipped, not failed, without an NFS client: that is the runner's property.
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
        # The only deployment where a union outlives the agent: in a container
        # the agent is pid 1 and takes every dind with it (ADR 0025, ADR 0044).
        # Shared mode, because section 3's per-account dind runs stock
        # docker:dind (daemons.DefaultImage), which lacks fuse-overlayfs.
        #
        # A directory of its own: one directory is one share and one mode
        # (ADR 0042), and section 5 mounted the project plainly.
        UNIONDIR="$WORK/uniondir"
        mkdir -p "$UNIONDIR"
        echo "served from the machine" >"$UNIONDIR/marker"
        if timeout 300 "$WORK/remote-docker" run -d --name vm-deleg \
            -v "$UNIONDIR:/w:read=cached,write=back" alpine:3 sleep 600 >"$WORK/deleg.log" 2>&1; then
            ok "a container starts against a delegated union"

            if union_is_fuse rd vm-deleg; then
                ok "its share is a union rather than a directory that resembles one"
            else
                bad "/w is not a fuse mount: [$LAST_OUTPUT]"
                dump_agent_log false
            fi

            # BEFORE the restart, so a failure afterwards says which half broke.
            if out=$(rd exec vm-deleg cat /w/marker 2>&1) &&
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

            # A NEW container asks the agent to prepare the share again; without
            # adoption a second fuse-overlayfs stacks on the same upper.
            if timeout 300 "$WORK/remote-docker" run --rm \
                -v "$UNIONDIR:/w:read=cached,write=back" alpine:3 cat /w/marker >"$WORK/deleg2.log" 2>&1; then
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

            if out=$(rd exec vm-deleg cat /w/marker 2>&1) &&
                echo "$out" | grep -q "served from the machine"; then
                ok "the container held its share across the agent restart"
            else
                bad "the held share stopped working: $(echo "$out" | tail -2 | tr -s '[:space:]' ' ')"
                dump_agent_log false
            fi
            rd rm -f vm-deleg >/dev/null 2>&1
        else
            bad "no container against a delegated union: $(tail -3 "$WORK/deleg.log" | tr -s '[:space:]' ' ')"
            dump_agent_log false
        fi
    fi
fi

summary
