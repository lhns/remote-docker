#!/usr/bin/env bash
# A daemon per account (ADR 0019), end to end, with TWO accounts.
#
# Separate from integration.sh, which tests shared mode: one script with a flag
# would prove whichever branch it happened to take.
#
# The claim is narrow: accounts stop seeing each other's containers. It is NOT
# isolation; each per-account daemon runs privileged.
#
# Requires: docker, and a kernel with NFS client support.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-peruser
SSH_PORT=22223

A=alice
B=bob

DOCKER_TIMEOUT=180

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

cleanup() { cleanup_suite "${CLIENT_A_PID:-}" "${CLIENT_B_PID:-}"; }
trap cleanup EXIT

# wait_dind waits up to 2 x $2 seconds for an account's own daemon to answer.
wait_dind() {
    local account=$1 tries=$2
    for _ in $(seq 1 "$tries"); do
        if hostdocker exec "$CONTAINER" docker exec "rd-dind-$account" \
                docker version >/dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    return 1
}

# dump_dind says why an account's daemon is not usable, asked of the PARENT
# daemon because a daemon that is down cannot answer. The exit code and
# OOMKilled separate "would not start" from "something killed it".
dump_dind() {
    local who=$1
    hostdocker exec "$CONTAINER" docker inspect "rd-dind-$who" --format \
        'state={{.State.Status}} exit={{.State.ExitCode}} oom={{.State.OOMKilled}} restarts={{.RestartCount}} error={{.State.Error}} started={{.State.StartedAt}} finished={{.State.FinishedAt}}' \
        2>&1 | sed "s/^/    rd-dind-$who: /"
    hostdocker exec "$CONTAINER" docker logs --tail 30 "rd-dind-$who" 2>&1 |
        sed "s/^/    rd-dind-$who: /"
}

echo "== 1. build =="
build_all

echo
echo "== 2. enrol two accounts =="
mkdir -p "$WORK/keys" "$WORK/wsstate"
for account in "$A" "$B"; do
    enrol "$account" "$WORK/state-$account" || { bad "no key generated for $account"; exit 1; }
done
ok "two keypairs staged as $A.pub and $B.pub"

echo
echo "== 3. start the workspace with a daemon per account =="
# WORKSPACE_DIND_MOUNTS stands in for a private registry's daemon.json, which
# each account's daemon needs too; a marker file, since there is no registry.
mkdir -p "$WORK/dindconf"
echo "reached the inner daemon" >"$WORK/dindconf/marker"

# WORKSPACE_DIND_IMAGE is the workspace's OWN image, as the Helm chart sets it.
# The fallback, stock docker:dind, lacks fuse-overlayfs, so this suite would
# test an image no deployment should run.
workspace_up true "$A $B" \
    -v "$WORK/dindconf:/etc/rd-test:ro" \
    -e "WORKSPACE_DIND_MOUNTS=/etc/rd-test:/etc/rd-test:ro" \
    -e "WORKSPACE_DIND_IMAGE=$IMAGE"

# The `docker` group reaches the PARENT daemon, which holds every account's
# dind, so in this mode nobody may be in it. The lookup must succeed first: a
# missing user is "not in the docker group" too.
for account in "$A" "$B"; do
    if ! groups=$(hostdocker exec "$CONTAINER" id -nG "rd-$account" 2>&1); then
        bad "no unix user rd-$account: $groups"
    elif echo "$groups" | tr ' ' '\n' | grep -qx docker; then
        bad "rd-$account is still in the docker group; it can reach the parent daemon"
    else
        ok "rd-$account is not in the docker group"
    fi
done

# Before any account connects, since the first connection starts its daemon.
info "loading the workspace image into the workspace's own daemon"
if load_image_into_workspace "$IMAGE"; then
    ok "each account's daemon can start from the workspace image"
else
    bad "could not load $IMAGE into the workspace's daemon"
fi

echo
echo "== 4. a session each =="
mkdir -p "$WORK/project-$A" "$WORK/project-$B"
echo "alice's file" >"$WORK/project-$A/marker"
echo "bob's file"   >"$WORK/project-$B/marker"

session() {
    local account=$1 endpoint=$2 log=$3 dir=$4
    start_session "$WORK/state-$account" "$account" "$endpoint" "$log" "$dir" \
        REMOTE_DOCKER_IDLE_TIMEOUT=8s
}

# Each account's CURRENT endpoint. Sessions restart onto new sockets below, and
# a helper pinned to a dead one silently reads as an empty result.
A_SOCK="$WORK/a.sock"
B_SOCK="$WORK/b.sock"

CLIENT_A_PID=$(session "$A" "$A_SOCK" "$WORK/a.log" "$WORK/project-$A")
CLIENT_B_PID=$(session "$B" "$B_SOCK" "$WORK/b.log" "$WORK/project-$B")

# A cold dind has to boot, which is the slowest thing here.
if wait_endpoint "$A_SOCK" "$CLIENT_A_PID" && wait_endpoint "$B_SOCK" "$CLIENT_B_PID"; then
    ok "both accounts have a working docker endpoint"
else
    bad "an endpoint never came up"
    sed 's/^/    A: /' "$WORK/a.log" | tail -20
    sed 's/^/    B: /' "$WORK/b.log" | tail -20
    dump_workspace_log 40
    exit 1
fi

da() { dockerat "$A_SOCK" "$@"; }
db() { dockerat "$B_SOCK" "$@"; }

# Pulled per account (a layer cache each), so "Unable to find image locally"
# stays out of the output assertions read.
info "pulling test images into each account's daemon"
for image in alpine:3 nginx:alpine; do
    da pull -q "$image" >/dev/null 2>&1 || info "could not pre-pull $image for $A"
    db pull -q "$image" >/dev/null 2>&1 || info "could not pre-pull $image for $B"
done

echo
echo "== 5. the daemons really are different =="
ida=$(da info --format '{{.ID}}' 2>/dev/null)
idb=$(db info --format '{{.ID}}' 2>/dev/null)
if [ -z "$ida" ] || [ -z "$idb" ]; then
    bad "a daemon did not answer: alice [$ida], bob [$idb]"
elif [ "$ida" != "$idb" ]; then
    ok "each account is talking to a different daemon"
else
    bad "both accounts reached the same daemon ($ida)"
fi

# WORKSPACE_DIND_MOUNTS, asserted in the running daemon rather than the plan.
for who in "$A" "$B"; do
    if outputs '^reached the inner daemon$' \
        hostdocker exec "$CONTAINER" docker exec "rd-dind-$who" cat /etc/rd-test/marker; then
        ok "$who's daemon can read the file the workspace was told to give it"
    else
        bad "$who's daemon cannot read it: [$LAST_OUTPUT]"
    fi
done

echo
echo "== 6. one account cannot see the other's containers =="
if da run -d --name alice-secret alpine:3 sleep 300 >/dev/null 2>&1; then
    ok "alice started a container"
else
    bad "alice could not start a container"
fi

if outputs '^alice-secret$' db ps --all --format '{{.Names}}'; then
    bad "bob can see alice's container -- the accounts are not separated"
else
    ok "bob cannot see alice's container"
fi

if db stop alice-secret >/dev/null 2>&1; then
    bad "bob stopped alice's container"
else
    ok "bob cannot stop alice's container"
fi

if outputs '^alice-secret$' da ps --format '{{.Names}}'; then
    ok "alice still sees her own"
else
    bad "alice lost sight of her own container"
fi

echo
echo "== 7. a bind mount resolves, which proves the in-netns NFS listener =="
# The reverse tunnel is bound INSIDE each account's dind; bound anywhere else,
# the volume fails to mount. stderr is kept for the failure message.
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
echo "== 7b. a read=cached,write=back share, which is a union mounted inside the dind =="
# The ONLY place the agent entering a dind's namespaces to mount a union is
# exercised: in shared mode nothing has to be entered (ADR 0044).
if out=$(da run -d --name pud-deleg -v "$WORK/project-$A:/w:read=cached,write=back" \
    alpine:3 sleep 120 2>&1); then
    ok "a container starts against a union inside alice's own daemon"

    # a bare directory passes every other check here (ADR 0044)
    if union_is_fuse da pud-deleg; then
        ok "alice's share is a union, not a directory that resembles one"
    else
        bad "/w is not a fuse mount: [$LAST_OUTPUT]"
        union_diagnostics
    fi

    if out=$(da exec pud-deleg cat /w/marker 2>&1); then
        if [ "$out" = "alice's file" ]; then
            ok "it reads alice's file through the union"
        else
            bad "the union gave [$out]"
        fi
    else
        bad "reading through the union failed: $(echo "$out" | tail -3)"
    fi

    # The fallthrough, for a file created after the union was mounted. Retried:
    # the NFS attribute cache and libfuse's entry cache are about a second each.
    echo "after the mount" >"$WORK/project-$A/late.txt"
    if wait_output '^after the mount$' 15 da exec pud-deleg cat /w/late.txt; then
        ok "a file the cache does not have falls through to the live export (${WAITED}s)"
    else
        bad "the fallthrough never happened: $(echo "$LAST_OUTPUT" | tail -3)"
    fi

    da rm -f pud-deleg >/dev/null 2>&1
else
    bad "a container would not start against a union: $(echo "$out" | tail -3)"
    union_diagnostics
fi

echo
echo "== 7c. the other union corners, inside the same dind =="
# the two direct corners (ADR 0042); 7b covered read=cached,write=back
for corner in "read=direct,write=back" "read=direct,write=ephemeral"; do
    case "$corner" in
        *back) name=pud-direct-back ;;
        *) name=pud-direct-eph ;;
    esac
    dir="$WORK/project-$A-$name"
    mkdir -p "$dir"
    echo "alice's file" >"$dir/marker"
    if ! out=$(da run -d --name "$name" -v "$dir:/w:$corner" alpine:3 sleep 120 2>&1); then
        bad "$corner: a container would not start against a union: $(echo "$out" | tail -3)"
        union_diagnostics
        continue
    fi
    if union_is_fuse da "$name"; then
        ok "$corner: the share is a union inside alice's daemon"
    else
        bad "$corner: /w is not a fuse mount: [$LAST_OUTPUT]"
    fi
    if [ "$(da exec "$name" cat /w/marker 2>&1)" = "alice's file" ]; then
        ok "$corner: it reads alice's file through the union"
    else
        bad "$corner: the union did not serve the file"
    fi
    write_comes_back da "$name" "$dir" "$corner" "${corner#*write=}"
    da rm -f "$name" >/dev/null 2>&1
done

echo
echo "== 8. two accounts publish, and the limit is this machine =="
# The port is the client's (ADR 0008), so only the CLIENT can refuse, and both
# accounts here share one runner: on two machines both would get 18090.
if da run -d --name alice-web -p 18090:80 nginx:alpine >/dev/null 2>&1; then
    ok "$A published 18090"
else
    bad "$A could not publish 18090"
    da logs alice-web 2>&1 | tail -5
fi

# The forward opens on a container event; asked too early, the create succeeds.
info "waiting for $A's forward to open on 18090"
for _ in $(seq 1 60); do
    if timeout 2 bash -c "exec 3<>/dev/tcp/127.0.0.1/18090" 2>/dev/null; then
        break
    fi
    sleep 1
done

if out=$(db run -d --name bob-web -p 18090:80 nginx:alpine 2>&1); then
    bad "two clients on one machine both opened 18090"
    db rm -f bob-web >/dev/null 2>&1
else
    case "$out" in
    *"port is already allocated"*)
        ok "$B is refused 18090 on this machine, in the wording the daemon uses" ;;
    *)
        bad "$B was refused for the wrong reason: $(echo "$out" | tail -1)" ;;
    esac
fi

if db run -d --name bob-web -p 18091:80 nginx:alpine >/dev/null 2>&1; then
    ok "$B published 18091 beside $A, on its own daemon"
else
    bad "$B could not publish at all"
    db logs bob-web 2>&1 | tail -5
fi

echo
echo "== 9. a shell points at its own daemon, AND CAN USE IT =="
# A correct DOCKER_HOST behind a directory the account cannot enter (/run/rd
# at 0750 root:root) shipped once, so the shell has to USE it.
shell_out=$(ssh_account "$WORK/state-$A/id_ed25519" "$A" 90 \
    'echo "HOST=$DOCKER_HOST"; docker ps --format "{{.Names}}" 2>&1 | head -5' \
    2>/dev/null | tr -d '\015')

case "$shell_out" in
    *"HOST=unix:///run/rd/$A/docker.sock"*)
        ok "a shell's DOCKER_HOST is that account's own daemon" ;;
    *"HOST="*)
        bad "a shell's DOCKER_HOST is $(echo "$shell_out" | head -1)" ;;
    *)
        bad "a shell got no DOCKER_HOST; it would find the parent daemon" ;;
esac

if echo "$shell_out" | grep -q "permission denied"; then
    bad "the account cannot reach its own docker socket: $(echo "$shell_out" | tail -1)"
elif echo "$shell_out" | grep -qx alice-secret; then
    ok "and a shell can actually use it"
else
    bad "a shell could not list its own containers: $(echo "$shell_out" | tail -2 | tr '\n' ' ')"
fi

# dockerd silently falls back to vfs, which copies the whole image on every
# create, when the graph filesystem refuses overlay2 (Ceph, NFS).
driver=$(da info --format '{{.Driver}}' 2>/dev/null)
if [ -z "$driver" ]; then
    bad "could not read the storage driver from $A's daemon"
elif [ "$driver" = "vfs" ]; then
    bad "$A's daemon is on vfs; every container create copies the whole image"
else
    ok "$A's daemon is on a copy-on-write storage driver ($driver)"
fi

echo
echo "== 10. the workspace restarts and a daemon comes back when its account connects =="
# The agent comes back to every daemon's name taken, and must adopt rather than
# conflict. What survives is CONTAINERS EXISTING, not running: nothing has a
# restart policy (ADR 0019).
before=$(da ps --all --format '{{.Names}}' 2>/dev/null | sort | tr '\n' ' ')
dind_before=$(hostdocker exec "$CONTAINER" docker inspect "rd-dind-$A" --format '{{.Id}}' 2>/dev/null)

stop_pid "$CLIENT_A_PID"; CLIENT_A_PID=""
stop_pid "$CLIENT_B_PID"; CLIENT_B_PID=""

hostdocker restart "$CONTAINER" >/dev/null 2>&1
info "waiting for the workspace's own daemon to come back"
for _ in $(seq 1 120); do
    if hostdocker exec "$CONTAINER" docker info >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

if outputs '^(exited|created)$' hostdocker exec "$CONTAINER" docker inspect "rd-dind-$A" --format '{{.State.Status}}'; then
    ok "$A's daemon stayed down until $A connects"
else
    bad "something restarted $A's daemon: [$LAST_OUTPUT]"
fi

# "Exactly one", not an adoption log line: Adopt runs while the parent dockerd
# is still starting, may find nothing, and Ensure does the work later instead.
count=$(hostdocker exec "$CONTAINER" docker ps --all \
    --filter "name=^/rd-dind-$A$" --format '{{.Names}}' 2>/dev/null | grep -c .)
if [ "$count" = "1" ]; then
    ok "exactly one daemon for $A after the restart, not a second one beside it"
else
    bad "$count daemons named rd-dind-$A after the restart"
    hostdocker exec "$CONTAINER" docker ps --all --format '{{.Names}} {{.Status}}' 2>&1 | tail -10
fi

A_SOCK="$WORK/a2.sock"
CLIENT_A_PID=$(session "$A" "$A_SOCK" "$WORK/a2.log" "$WORK/project-$A")
if ! wait_endpoint "$A_SOCK" "$CLIENT_A_PID"; then
    bad "alice's endpoint never came back after the restart"
    sed 's/^/    A: /' "$WORK/a2.log" | tail -20
    dump_dind "$A"
    dump_workspace_log 40
fi

after=$(da ps --all --format '{{.Names}}' 2>/dev/null | sort | tr '\n' ' ')
if [ -n "$before" ] && [ "$before" = "$after" ]; then
    ok "alice's containers survived the restart"
else
    bad "alice's containers changed across the restart: [$before] -> [$after]"
fi

# A new id would mean the old daemon was abandoned with everything in it.
dind_after=$(hostdocker exec "$CONTAINER" docker inspect "rd-dind-$A" --format '{{.Id}}' 2>/dev/null)
if [ -n "$dind_before" ] && [ "$dind_before" = "$dind_after" ]; then
    ok "the same daemon container was reused, not replaced"
else
    bad "alice's daemon was replaced: [$dind_before] -> [$dind_after]"
fi

echo
echo "== 11. the account's storage outlives its daemon container =="
# The graph volume is named and labelled so the container in front of it is
# disposable: an upgrade removes and recreates that container.
if outputs '^1$' hostdocker exec "$CONTAINER" docker volume inspect "rd-dind-$A-lib" \
        --format '{{index .Labels "remote-docker.daemon"}}'; then
    ok "the graph volume is labelled, so an operator can see what must not be pruned"
else
    bad "the graph volume carries no label; a prune would take it with nothing naming it"
fi

images_before=$(da images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | sort | tr '\n' ' ')

# Remove the daemon CONTAINER, keeping the volume, as an upgrade does.
stop_pid "$CLIENT_A_PID"; CLIENT_A_PID=""
hostdocker exec "$CONTAINER" docker rm -f "rd-dind-$A" >/dev/null 2>&1

A_SOCK="$WORK/a3.sock"
CLIENT_A_PID=$(session "$A" "$A_SOCK" "$WORK/a3.log" "$WORK/project-$A")
if ! wait_endpoint "$A_SOCK" "$CLIENT_A_PID"; then
    bad "alice's endpoint never came back after her daemon was destroyed"
    dump_dind "$A"
fi

images_after=$(da images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | sort | tr '\n' ' ')
if [ -n "$images_before" ] && [ "$images_before" = "$images_after" ]; then
    ok "alice's images survived her daemon container being destroyed"
else
    bad "alice's images did not survive: [$images_before] -> [$images_after]"
fi

if outputs '^alice-secret$' da ps --all --format '{{.Names}}'; then
    ok "and so did her containers"
else
    bad "alice's containers did not survive her daemon being recreated"
fi

echo
echo "== 12. the NFS export is not reachable from a shell =="
# The reverse forward binds inside the account's own dind namespace
# (agent/internal/sshd/forward_tcpip.go) and a shell runs in the workspace's,
# so the namespace is the only thing deciding here: a socket asks no policy.
# With a shared daemon (ADR 0012) this does not hold.
alice_port=$(cd "$WORK/project-$A" && REMOTE_DOCKER_STATE_DIR="$WORK/state-$A" \
    REMOTE_DOCKER_HOST=127.0.0.1 REMOTE_DOCKER_PORT="$SSH_PORT" \
    REMOTE_DOCKER_USER="$A" REMOTE_DOCKER_ENDPOINT="$A_SOCK" \
    timeout 60 "$WORK/remote-docker" remote status 2>/dev/null | tunnel_port)

if [ -z "$alice_port" ]; then
    bad "could not read $A's tunnel port, so nothing was probed"
else
    # One probe for both namespaces; busybox nc is in both images.
    probe="nc -w 2 127.0.0.1 $alice_port </dev/null && echo CONNECTED || echo REFUSED"

    # Holds the export in use, or an idle release unbinds the port and every
    # probe is refused for the wrong reason.
    da run -d --name alice-hold -v "$WORK/project-$A:/w" alpine:3 sleep 300 >/dev/null 2>&1

    # The positive control: a host-network container in the daemon's namespace
    # reaches the export (docs/threat-model.md, flow 3).
    inside=$(da run --rm --network host alpine:3 sh -c "$probe" 2>/dev/null | tr -d '\015')
    case "$inside" in
    *CONNECTED*) ok "the export answers inside $A's daemon namespace, so the port is live" ;;
    *) bad "the export did not answer inside $A's own namespace: [$inside]. The probes below prove nothing" ;;
    esac

    # A shell waits for its account's daemon (Ensure in
    # agent/internal/sshd/session.go, up to daemons.DefaultReadyTimeout), and
    # $B's is down since section 10 restarted the workspace.
    info "starting $B's daemon, so the shell probe does not pay for its boot"
    started_at=$(date +%s)
    hostdocker exec "$CONTAINER" docker start "rd-dind-$B" >/dev/null 2>&1
    if ! wait_dind "$B" 90; then
        bad "$B's daemon did not answer in 180s, so the probe below proves nothing"
        dump_dind "$B"
    else
        # The one place a healthy daemon's boot is timed: 17.0s while dockerd
        # slept at the unencrypted-listener warning, 1s since (2026-09-08), at
        # wait_dind's 2s resolution.
        info "$B's daemon answered in $(( $(date +%s) - started_at ))s"
    fi

    for who in "$A" "$B"; do
        # The status tells 124 (the shell never opened) from a probe that said
        # nothing. ssh's stderr goes to a file so it cannot read as an answer.
        # 200s outlasts daemons.DefaultReadyTimeout, after which Ensure names
        # its reason; shorter, the failure carries no cause.
        reach=$(ssh_account "$WORK/state-$who/id_ed25519" "$who" 200 "$probe" \
            2>"$WORK/probe-$who.err" | tr -d '\015')
        status=$?
        case "$reach" in
        *CONNECTED*) bad "SECURITY: $who's shell reached the NFS export on $alice_port" ;;
        *REFUSED*)   ok "$who's shell cannot reach the export on $alice_port" ;;
        *)
            why="exit $status"
            if [ "$status" = 124 ]; then
                why="$why, the 200s timeout: the shell never opened"
            fi
            bad "the probe from $who's shell said nothing ($why): [$reach]"
            sed 's/^/    ssh: /' "$WORK/probe-$who.err" | tail -5
            dump_dind "$who"
            ;;
        esac
    done

    da rm -f alice-hold >/dev/null 2>&1
fi

echo
echo "== 13. a daemon that was killed rather than stopped starts again =="
# A stale containerd.pid stops dockerd only while the pid it names is alive,
# so planting pid 1 makes a one-in-eighty failure deterministic
# (daemons.ExecRoot in agent/internal/daemons/plan.go).
EXECROOT=/var/run/docker
PIDFILE=$EXECROOT/containerd/containerd.pid
if ! planted=$(hostdocker exec "$CONTAINER" docker exec "rd-dind-$B" \
        sh -c "echo 1 >$PIDFILE && cat $PIDFILE" 2>&1); then
    bad "could not plant a stale containerd pid in $B's daemon: [$planted]"
else
    # kill, not stop: what a workspace restart does to every per-account daemon.
    hostdocker exec "$CONTAINER" docker kill "rd-dind-$B" >/dev/null 2>&1
    hostdocker exec "$CONTAINER" docker start "rd-dind-$B" >/dev/null 2>&1

    if wait_dind "$B" 60; then
        ok "$B's daemon came back with a stale containerd pid file behind it"
    else
        bad "$B's daemon did not come back after being killed with a stale $PIDFILE"
        dump_dind "$B"
    fi

    # The mechanism too: a daemon that happened to start would hide a missing
    # tmpfs.
    if outputs '^tmpfs$' hostdocker exec "$CONTAINER" docker exec "rd-dind-$B" \
            stat -f -c %T "$EXECROOT"; then
        ok "the exec-root is a tmpfs, so nothing in it survives a restart"
    else
        bad "the exec-root is not a tmpfs: [$LAST_OUTPUT]"
    fi
fi

echo
echo "== 14. an account's daemon binds no TCP API =="
# dind's entrypoint adds tcp://0.0.0.0:2375 unless the command names `dockerd`
# first (the command daemons.Plan builds). Asserted as what the daemon BOUND and
# what a container can REACH, since either alone can pass for the wrong reason.
listeners=$(hostdocker exec "$CONTAINER" docker exec "rd-dind-$A" netstat -lnt 2>&1)
case "$listeners" in
*:2375*|*:2376*)
    bad "SECURITY: $A's daemon is listening on a TCP port: [$listeners]" ;;
*Active*|*Proto*)
    ok "$A's daemon binds no Docker API on 2375 or 2376" ;;
*)
    # No header: netstat did not run, so the absence is evidence of nothing.
    bad "netstat said nothing inside $A's daemon, so no listener was measured: [$listeners]" ;;
esac

# --network host is the daemon's own namespace.
probe2375="nc -w 2 127.0.0.1 2375 </dev/null && echo CONNECTED || echo REFUSED"
reach=$(da run --rm --network host alpine:3 sh -c "$probe2375" 2>/dev/null | tr -d '\015')
case "$reach" in
*CONNECTED*) bad "SECURITY: a container in $A's daemon reached a Docker API on 2375" ;;
*REFUSED*)   ok "a container in $A's daemon finds nothing on 2375" ;;
*)           bad "the 2375 probe said nothing, so it proves nothing: [$reach]" ;;
esac

echo
if [ "$FAIL" -ne 0 ]; then
    # 200, not 60: a daemon that will not stay up logs "container is not
    # running" every 2s, which pushes its first failure off a shorter tail.
    dump_workspace_log 200
fi

summary
