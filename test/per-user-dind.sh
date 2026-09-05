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
#
# The extra mount stands in for the real case: a workspace with a private
# registry mounts /etc/docker/daemon.json into its own daemon, and each
# account's daemon needs the same file or a pull that works on the workspace
# fails inside every account. A marker is used instead of a real daemon.json,
# which would have to name a registry this suite does not have.
mkdir -p "$WORK/dindconf"
echo "reached the inner daemon" >"$WORK/dindconf/marker"

# WORKSPACE_DIND_IMAGE is the workspace's OWN image, which is what a real
# deployment runs (the Helm chart sets exactly this, and elevate passes
# WORKSPACE_IMAGE where it can). Without it the fallback is stock docker:dind,
# which carries none of the tooling this workspace decided it needs --
# fuse-overlayfs above all -- so this suite would exercise an image no
# deployment should be using and miss anything that depends on it.
#
# It has to be LOADED into the workspace's daemon as well, below: the image was
# built on the runner, and the daemon that starts each account's dind is the
# workspace's own.
if start_workspace true     -v "$WORK/dindconf:/etc/rd-test:ro"     -e "WORKSPACE_DIND_MOUNTS=/etc/rd-test:/etc/rd-test:ro"     -e "WORKSPACE_DIND_IMAGE=$IMAGE"; then
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
#
# Asked of the UNIX user, which is `rd-<account>` and not the account name
# (ADR 0025). Spelled `$account` this looked like it passed: `id` failed for a
# user that does not exist, the grep found nothing, and "not in the docker
# group" is what a missing user and a correct one produce alike. So the lookup
# has to succeed before the membership means anything.
for account in "$A" "$B"; do
    if ! groups=$(hostdocker exec "$CONTAINER" id -nG "rd-$account" 2>&1); then
        bad "no unix user rd-$account: $groups"
    elif echo "$groups" | tr ' ' '\n' | grep -qx docker; then
        bad "rd-$account is still in the docker group; it can reach the parent daemon"
    else
        ok "rd-$account is not in the docker group"
    fi
done

info "waiting for the parent dockerd"
wait_parent_dockerd

# Before any account connects, because the first connection is what starts that
# account's daemon (ADR 0019) and it would otherwise try to pull this image
# from a registry.
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

# Watching is on because a read=cached share requires it: its cache holds actual
# copies, and the watcher is what keeps them honest (ADR 0044). Section 7b would
# otherwise be refused on a rule rather than tested on its mechanism.
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
        REMOTE_DOCKER_WATCH=partial \
        "$WORK/remote-docker" remote start --foreground
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

# WORKSPACE_DIND_MOUNTS, asserted in the daemon rather than in the plan: what
# an operator needs is the file readable by the dockerd doing the pulling, and
# only a running container can say whether it is.
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
# THE assertion. Everything else in this file supports it.
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

# And cannot reach it by name either, which is the operation somebody would
# actually try.
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
echo "== 7b. a read=cached,write=back share, which is a union mounted inside the dind =="
# The ONLY place the mount-namespace entry is exercised. In shared mode the
# agent and the daemon are one filesystem and nothing has to be entered; here
# the union lives inside this account's dind, and the agent has to get in there
# to mount it (ADR 0044).
#
# It also proves the two accounts stay separate at this layer: each union is
# mounted in its own daemon's namespace, so alice's cache cannot be bob's.
if out=$(da run -d --name pud-deleg -v "$WORK/project-$A:/w:read=cached,write=back"     alpine:3 sleep 120 2>&1); then
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

    # The fallthrough, which is what makes an incomplete cache correct: this
    # file did not exist when the union was mounted.
    #
    # Retried rather than read once. Two caches sit between the two sides -- the
    # NFS attribute cache under the union, and libfuse's own entry cache -- and
    # both are about a second, so reading immediately measures the caches rather
    # than the mechanism. How long it took is reported, because that IS the
    # answer to "when does a new file appear".
    echo "after the mount" >"$WORK/project-$A/late.txt"
    fell=""
    for i in $(seq 1 15); do
        out=$(da exec pud-deleg cat /w/late.txt 2>&1)
        if [ "$out" = "after the mount" ]; then
            fell=$i
            break
        fi
        sleep 1
    done
    if [ -n "$fell" ]; then
        ok "a file the cache does not have falls through to the live export (${fell}s)"
    else
        bad "the fallthrough never happened: $(echo "$out" | tail -3)"
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
    da exec "$name" sh -c 'echo "written there" >/w/out.txt' >/dev/null 2>&1
    case "$corner" in
        *back)
            back=$(wait_for_content "$dir/out.txt" "written there" 30)
            if [ "$back" = "written there" ]; then
                ok "$corner: the container's write came back to alice's directory"
            else
                bad "$corner: the write never came back: [$back]"
            fi ;;
        *ephemeral)
            back=$(wait_for_content "$dir/out.txt" "" 30)
            if [ -z "$back" ]; then
                ok "$corner: the container's write stayed in the workspace, 30s on"
            else
                bad "$corner: an ephemeral write came back: [$back]"
            fi ;;
    esac
    da rm -f "$name" >/dev/null 2>&1
done

echo
echo "== 8. two accounts publish, and the limit is this machine =="
# Where the collision lives now that the port is the client's (ADR 0008). The
# workspace no longer binds the
# number anybody asked for, so neither daemon can refuse the other. What can
# refuse is the CLIENT, because the requested number is opened here, and both
# accounts in this suite are driven from one runner: two people on two machines
# would both get 18090.
if da run -d --name alice-web -p 18090:80 nginx:alpine >/dev/null 2>&1; then
    ok "$A published 18090"
else
    bad "$A could not publish 18090"
    da logs alice-web 2>&1 | tail -5
fi

# The forward has to be OPEN before the refusal can be about anything: the
# ports manager reconciles on container events, so asking a second too early
# probes a port nobody is listening on yet and the create succeeds.
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

# And nothing on the workspace was in the way: a different local number works
# at once, on that account's own daemon.
if db run -d --name bob-web -p 18091:80 nginx:alpine >/dev/null 2>&1; then
    ok "$B published 18091 beside $A, on its own daemon"
else
    bad "$B could not publish at all"
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
echo "== 10. the workspace restarts and a daemon comes back when its account connects =="
# The case that would otherwise lose everybody's work: the agent comes back,
# finds every daemon's name taken, and `docker run --name` conflicts rather
# than replacing.
#
# What survives is deliberately stated as CONTAINERS EXISTING, not running.
# A restarted dockerd starts only containers with a restart policy, and neither
# the account's containers nor the daemon itself has one: the agent is the only
# supervisor (ADR 0019), so the daemon starts when its account next connects and
# brings its graph with it.
before=$(da ps --all --format '{{.Names}}' 2>/dev/null | sort | tr '
' ' ')
dind_before=$(hostdocker exec "$CONTAINER" docker inspect "rd-dind-$A" --format '{{.Id}}' 2>/dev/null)

kill "$CLIENT_A_PID" 2>/dev/null; wait "$CLIENT_A_PID" 2>/dev/null; CLIENT_A_PID=""
kill "$CLIENT_B_PID" 2>/dev/null; wait "$CLIENT_B_PID" 2>/dev/null; CLIENT_B_PID=""

hostdocker restart "$CONTAINER" >/dev/null 2>&1
info "waiting for the workspace's own daemon to come back"
for _ in $(seq 1 120); do
    if hostdocker exec "$CONTAINER" docker info >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

# Asserted rather than assumed, because it is what ADR 0019 trades away: no
# restart policy, no session yet, so nothing has started it.
if outputs '^(exited|created)$' hostdocker exec "$CONTAINER" docker inspect "rd-dind-$A" --format '{{.State.Status}}'; then
    ok "$A's daemon stayed down until $A connects"
else
    bad "something restarted $A's daemon: [$LAST_OUTPUT]"
fi

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

if outputs '^alice-secret$' da ps --all --format '{{.Names}}'; then
    ok "and so did her containers"
else
    bad "alice's containers did not survive her daemon being recreated"
fi

echo
echo "== 12. the NFS export is not reachable from a shell =="
# The reverse forward binds 127.0.0.1 inside the account's own dind namespace
# (agent/internal/sshd/forward_tcpip.go). A shell runs in the workspace
# container's namespace, so it cannot reach the export, not even its own
# account's. Opening a socket asks no forwarding policy, so the namespace is
# the only thing deciding here.
#
# With one daemon for everybody (ADR 0012) the export binds in the namespace
# the shells run in and this does not hold; test/integration.sh measures it.
alice_port=$(cd "$WORK/project-$A" && REMOTE_DOCKER_STATE_DIR="$WORK/state-$A" \
    REMOTE_DOCKER_HOST=127.0.0.1 REMOTE_DOCKER_PORT="$SSH_PORT" \
    REMOTE_DOCKER_USER="$A" REMOTE_DOCKER_ENDPOINT="$A_SOCK" \
    timeout 60 "$WORK/remote-docker" remote status 2>/dev/null |
    awk '/^account/ {print $NF}')

if [ -z "$alice_port" ]; then
    bad "could not read $A's tunnel port, so nothing was probed"
else
    # One probe, run in both namespaces, so the two answers are comparable. nc
    # is busybox's, present in the workspace image and in alpine.
    probe="nc -w 2 127.0.0.1 $alice_port </dev/null && echo CONNECTED || echo REFUSED"

    # A container holding a bind mount keeps the export in use, so the forward
    # stays bound while the probes run. Without it an idle release unbinds the
    # port and every probe below is refused for the wrong reason, which is a
    # test that cannot fail.
    da run -d --name alice-hold -v "$WORK/project-$A:/w" alpine:3 sleep 300 >/dev/null 2>&1

    # The positive control, and the claim the threat model's flow 3 makes about
    # host networking: a container that joins the daemon's namespace lands
    # where the export is bound, and reaches every share, not only its own
    # mounts.
    inside=$(da run --rm --network host alpine:3 sh -c "$probe" 2>/dev/null | tr -d '\015')
    case "$inside" in
    *CONNECTED*) ok "the export answers inside $A's daemon namespace, so the port is live" ;;
    *) bad "the export did not answer inside $A's own namespace: [$inside]. The probes below prove nothing" ;;
    esac

    # A shell waits for its account's daemon (the session sets DOCKER_HOST from
    # Ensure), and $B has not reconnected since section 10 restarted the
    # workspace. Without this the probe pays for a cold dind boot and times out
    # reporting nothing, which looks nothing like what it tests.
    info "starting $B's daemon, so the shell probe does not pay for its boot"
    hostdocker exec "$CONTAINER" docker start "rd-dind-$B" >/dev/null 2>&1
    for _ in $(seq 1 90); do
        if hostdocker exec "$CONTAINER" docker exec "rd-dind-$B" docker version >/dev/null 2>&1; then
            break
        fi
        sleep 2
    done

    for who in "$A" "$B"; do
        reach=$(timeout 120 ssh -i "$WORK/state-$who/id_ed25519" \
            -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            -o BatchMode=yes -p "$SSH_PORT" "$who@127.0.0.1" "$probe" \
            2>/dev/null </dev/null | tr -d '\015')
        case "$reach" in
        *CONNECTED*) bad "SECURITY: $who's shell reached the NFS export on $alice_port" ;;
        *REFUSED*)   ok "$who's shell cannot reach the export on $alice_port" ;;
        *)           bad "the probe from $who's shell said nothing: [$reach]" ;;
        esac
    done

    da rm -f alice-hold >/dev/null 2>&1
fi

echo
if [ "$FAIL" -ne 0 ]; then
    dump_workspace_log
fi

summary
