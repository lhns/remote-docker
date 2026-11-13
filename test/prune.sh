#!/usr/bin/env bash
# Pruning of unused images, build cache and dangling anonymous volumes (ADR
# 0056), end to end.
#
#   bash test/prune.sh shared|per-account
#
# The argument is the daemon mode (ADR 0012 or ADR 0019). No client session is
# needed: the agent prunes on its own clock, and this suite asks the daemons
# directly. Three workspaces, one after another, because the settings are the
# agent's environment and a daemon's images live in its container:
#
#   1. AFTER=20s: what goes and what stays, and that named volumes are never touched.
#   2. AFTER=20s and CONTAINERS=true: a stopped container goes, a running one stays.
#   3. ABOVE=1% and AFTER=off: usage pressure removes unused images, once warned.
#   4. AFTER=20s and VOLUMES=on: a dangling anonymous volume goes, named,
#      managed, attached and unlabelled ones stay.
#   5. the same without VOLUMES: nothing is removed.
#   6. ABOVE=1% and VOLUMES=on: a fresh dangling volume waits for the floor.
#
# Every other suite runs with pruning off (start_workspace in lib.sh).
#
# Requires: docker, and a kernel with NFS client support.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-prune
SSH_PORT=22226

ACCOUNT=alice

# Short, so each wait is under two minutes; the defaults are 7d and 1h.
AGE=20
INTERVAL=5

DOCKER_TIMEOUT=180

case "${1:-}" in
    shared) PER_USER_DIND=false ;;
    per-account) PER_USER_DIND=true ;;
    *)
        echo "usage: $0 shared|per-account" >&2
        exit 2
        ;;
esac

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

cleanup() { cleanup_suite; }
trap cleanup EXIT

# wsdocker runs docker against the daemon that is pruned: the shared one, or
# the account's own dind. wsdocker_i keeps stdin open, for a build.
wsdocker() {
    if [ "$PER_USER_DIND" = true ]; then
        hostdocker exec "$CONTAINER" docker exec "rd-dind-$ACCOUNT" docker "$@"
    else
        hostdocker exec "$CONTAINER" docker "$@"
    fi
}
wsdocker_i() {
    if [ "$PER_USER_DIND" = true ]; then
        hostdocker exec -i "$CONTAINER" docker exec -i "rd-dind-$ACCOUNT" docker "$@"
    else
        hostdocker exec -i "$CONTAINER" docker "$@"
    fi
}

# parentdocker runs docker against the workspace's own daemon, the parent of
# every account's.
parentdocker() { hostdocker exec "$CONTAINER" docker "$@"; }

agent_log() { hostdocker logs "$CONTAINER" 2>&1; }

images() { wsdocker image ls --format '{{.Repository}}:{{.Tag}}'; }
containers() { wsdocker ps -a --format '{{.Names}}'; }
volumes() { wsdocker volume ls --format '{{.Name}}'; }

# build_cache_count prints how many build cache records the daemon holds.
build_cache_count() {
    wsdocker system df --format '{{.Type}}|{{.TotalCount}}' 2>&1 | awk -F'|' '$1 == "Build Cache" {print $2}'
}

# has asks whether a listing holds a whole line, and leaves what it saw in
# LAST_OUTPUT.
#
#   has <what> <name> <listing-function>
has() { outputs "^$2\$" "$3"; }

check_present() {
    local what=$1 name=$2 listing=$3
    if has "$what" "$name" "$listing"; then
        ok "$what $name is still there"
    else
        bad "$what $name is gone: [$(tr '\n' ' ' <<<"$LAST_OUTPUT")]"
    fi
}

check_absent() {
    local what=$1 name=$2 listing=$3
    if has "$what" "$name" "$listing"; then
        bad "$what $name is still there: [$(tr '\n' ' ' <<<"$LAST_OUTPUT")]"
    else
        ok "$what $name is gone"
    fi
}

# wait_gone waits for a listing to lose a line, up to <seconds>.
#
#   wait_gone <seconds> <name> <listing-function>
wait_gone() {
    local seconds=$1 name=$2 listing=$3 _
    for _ in $(seq 1 "$seconds"); do
        has x "$name" "$listing" || return 0
        sleep 1
    done
    return 1
}

# workspace_for starts a workspace with the given agent settings and a daemon
# to prune, and waits for it.
#
#   workspace_for <docker-run-arg...>
workspace_for() {
    local args=()
    if [ "$PER_USER_DIND" = true ]; then
        # The workspace's own image, as the chart sets it (why: per-user-dind.sh).
        args=(-e "WORKSPACE_DIND_IMAGE=$IMAGE")
    fi
    workspace_up "$PER_USER_DIND" "$ACCOUNT" "${args[@]}" "$@"
    if [ "$PER_USER_DIND" = true ]; then
        if load_image_into_workspace "$IMAGE"; then
            ok "the account's daemon can start from the workspace image"
        else
            bad "could not load $IMAGE into the workspace's daemon"
        fi
        # A stock ssh login starts the account's daemon (ADR 0019).
        if ssh_account "$WORK/state-$ACCOUNT/id_ed25519" "$ACCOUNT" 240 'docker ps' >/dev/null 2>&1; then
            ok "$ACCOUNT's daemon started"
        else
            bad "$ACCOUNT's daemon did not start"
            dump_workspace_log 40
            exit 1
        fi
    fi
    if wsdocker pull -q alpine:3 >/dev/null 2>&1; then
        ok "alpine:3 is in the daemon"
    else
        bad "could not pull alpine:3"
        exit 1
    fi
}

# make_image makes an image no pull or build is needed for, with <tags...>.
#
#   make_image <name> <tag...>
make_image() {
    local name=$1 tag
    shift
    wsdocker run --name "mk-$name" alpine:3 sh -c "echo $name > /marker" >/dev/null 2>&1
    wsdocker commit "mk-$name" "$1" >/dev/null 2>&1
    for tag in "${@:2}"; do
        wsdocker tag "$1" "$tag" >/dev/null 2>&1
    done
    wsdocker rm "mk-$name" >/dev/null 2>&1
}

echo "== 0. build =="
build_all

echo
echo "== 0b. an account for the daemon =="
mkdir -p "$WORK/keys" "$WORK/wsstate"
if enrol "$ACCOUNT" "$WORK/state-$ACCOUNT"; then
    ok "a key staged as $ACCOUNT.pub"
else
    bad "no key generated for $ACCOUNT"
    exit 1
fi

echo
echo "== 1. images and build cache, by age =="
workspace_for \
    -e "WORKSPACE_PRUNE_AFTER=${AGE}s" \
    -e "WORKSPACE_PRUNE_INTERVAL=${INTERVAL}s" \
    -e WORKSPACE_PRUNE_ABOVE=off

# One image used by a stopped container, one by a running one, one used by
# nothing and tagged twice, and a volume of each kind pruning must not touch.
make_image used prune/used:1
make_image running prune/running:1
make_image unused prune/unused:1 prune/unused:2
wsdocker create --name prune-stopped prune/used:1 true >/dev/null 2>&1
wsdocker run -d --name prune-running prune/running:1 sleep 3600 >/dev/null 2>&1
wsdocker volume create prune-keep >/dev/null 2>&1
wsdocker volume create rd-prune-cache >/dev/null 2>&1

# A build leaves cache. Its image is unused too, so it goes with the others.
printf 'FROM alpine:3\nRUN echo cached > /c\n' | wsdocker_i build -q -t prune/built:1 - >/dev/null 2>&1
BEFORE=$(build_cache_count)
if [ -n "$BEFORE" ] && [ "$BEFORE" -gt 0 ] 2>/dev/null; then
    ok "the build left $BEFORE cache records"
else
    bad "the build left no cache records: [$(wsdocker system df 2>&1 | tr '\n' ' ')]"
fi

info "waiting for an image unused for ${AGE}s to be removed"
if wait_gone $((AGE * 6)) 'prune/unused:1' images; then
    ok "the unused image is gone after its age"
else
    bad "the unused image was never removed: [$(images | tr '\n' ' ')]"
    agent_log | grep prune | tail -20 | sed 's/^/        /'
fi
# The build is the last image the agent met, so it is the last to be old enough.
if wait_gone $((AGE * 3)) 'prune/built:1' images; then
    ok "the image of the build is gone too"
else
    bad "the image of the build is still there: [$(images | tr '
' ' ')]"
fi
check_absent image 'prune/unused:2' images

info "waiting for the build cache to be pruned"
AFTER=$BEFORE
for _ in $(seq 1 $((AGE * 4))); do
    AFTER=$(build_cache_count)
    [ "${AFTER:-0}" -lt "${BEFORE:-0}" ] 2>/dev/null && break
    sleep 1
done
if [ "${AFTER:-0}" -lt "${BEFORE:-0}" ] 2>/dev/null; then
    ok "the build cache shrank from $BEFORE to $AFTER records"
else
    bad "the build cache did not shrink: $BEFORE then $AFTER"
fi

# Everything that must stay is older than the age by now.
check_present image 'prune/used:1' images
check_present image 'prune/running:1' images
check_present container prune-stopped containers
check_present container prune-running containers
check_present volume prune-keep volumes
check_present volume rd-prune-cache volumes
if outputs 'pruned what had gone unused' agent_log; then
    ok "the agent said what it pruned"
else
    bad "no line about a prune in the agent's log"
fi

if [ "$PER_USER_DIND" = true ]; then
    # The image an account's daemon runs is referenced by nothing once the
    # daemon is gone, and must not be pruned for that; nor the account's graph
    # volume, which the daemon's removal leaves (ADR 0056, ADR 0019).
    parentvolumes() { parentdocker volume ls --format '{{.Name}}'; }
    check_present volume "rd-dind-$ACCOUNT-lib" parentvolumes
    parentdocker rm -f "rd-dind-$ACCOUNT" >/dev/null 2>&1
    parentimages() { parentdocker image ls --format '{{.Repository}}:{{.Tag}}'; }
    info "waiting for the workspace's image to be examined with no account daemon"
    sleep $((AGE + INTERVAL * 3))
    check_present image "$IMAGE" parentimages
    check_present volume "rd-dind-$ACCOUNT-lib" parentvolumes
fi

echo
echo "== 2. stopped containers, only when asked =="
workspace_for \
    -e "WORKSPACE_PRUNE_AFTER=${AGE}s" \
    -e "WORKSPACE_PRUNE_INTERVAL=${INTERVAL}s" \
    -e WORKSPACE_PRUNE_ABOVE=off \
    -e WORKSPACE_PRUNE_CONTAINERS=true

wsdocker run --name prune-done alpine:3 true >/dev/null 2>&1
wsdocker run -d --name prune-alive alpine:3 sleep 3600 >/dev/null 2>&1
if wait_gone $((AGE * 6)) prune-done containers; then
    ok "the container stopped for ${AGE}s is removed"
else
    bad "the stopped container is still there: [$(containers | tr '\n' ' ')]"
    agent_log | grep prune | tail -20 | sed 's/^/        /'
fi
check_present container prune-alive containers
if [ "$PER_USER_DIND" = true ]; then
    parentcontainers() { parentdocker ps -a --format '{{.Names}}'; }
    check_present container "rd-dind-$ACCOUNT" parentcontainers
fi

echo
echo "== 3. a disk above the threshold =="
# 1% is always exceeded. The floor is short so the image made here is eligible
# a moment after it is met.
workspace_for \
    -e WORKSPACE_PRUNE_AFTER=off \
    -e WORKSPACE_PRUNE_ABOVE=1% \
    -e WORKSPACE_PRUNE_FLOOR=1s \
    -e "WORKSPACE_PRUNE_INTERVAL=${INTERVAL}s"

# Held, or the pressure rule removes alpine:3 before the images below are made.
wsdocker create --name prune-hold alpine:3 true >/dev/null 2>&1
make_image pressure prune/pressure:1
make_image busy prune/busy:1
wsdocker run -d --name prune-busy prune/busy:1 sleep 3600 >/dev/null 2>&1
wsdocker volume create prune-keep >/dev/null 2>&1
if wait_gone 90 'prune/pressure:1' images; then
    ok "an unused image is removed under pressure without any age"
else
    bad "the unused image survived the pressure: [$(images | tr '\n' ' ')]"
    agent_log | grep prune | tail -20 | sed 's/^/        /'
fi
check_present image 'prune/busy:1' images
check_present container prune-busy containers
check_present volume prune-keep volumes

# Never reachable, so the agent says so once and not on every measurement.
sleep $((INTERVAL * 4))
WARNED=$(agent_log | grep -c 'still above the threshold')
if [ "$WARNED" -eq 1 ]; then
    ok "one warning that nothing is left to prune"
else
    bad "the warning was logged $WARNED times, want once: $(agent_log | grep 'still above' | tail -3)"
fi

# anon_volume makes a container with an anonymous volume and prints the volume's
# name. The container is left in place.
#
#   anon_volume <container>
anon_volume() {
    wsdocker create --name "$1" -v /data alpine:3 true >/dev/null 2>&1
    wsdocker inspect -f '{{range .Mounts}}{{.Name}}{{end}}' "$1" 2>/dev/null
}

# dangling_volume leaves an anonymous volume with no container and prints its
# name: the leak a container removed without -v makes.
#
#   dangling_volume <container>
dangling_volume() {
    local name
    name=$(anon_volume "$1")
    wsdocker rm "$1" >/dev/null 2>&1
    echo "$name"
}

# HEX64 is what the daemon names an anonymous volume. Made by hand it lacks the
# label the daemon adds, so it is not one.
HEX64=$(printf 'ab%.0s' $(seq 1 32))

# volumes_workspace starts a workspace for sections 4 to 6.
#
#   volumes_workspace <docker-run-arg...>
volumes_workspace() {
    workspace_for -e "WORKSPACE_PRUNE_INTERVAL=${INTERVAL}s" "$@"
}

echo
echo "== 4. dangling anonymous volumes, only when asked =="
volumes_workspace -e "WORKSPACE_PRUNE_AFTER=${AGE}s" -e WORKSPACE_PRUNE_ABOVE=off \
    -e WORKSPACE_PRUNE_VOLUMES=on

LEAKED=$(dangling_volume anon-leak)
HELD=$(anon_volume anon-held)
wsdocker volume create prune-keep >/dev/null 2>&1
wsdocker volume create rd-prune-cache >/dev/null 2>&1
wsdocker volume create "$HEX64" >/dev/null 2>&1
if [ -n "$LEAKED" ] && [ -n "$HELD" ]; then
    ok "two anonymous volumes made: $LEAKED and $HELD"
else
    bad "no anonymous volumes: leaked [$LEAKED] held [$HELD]"
fi
if outputs 'com.docker.volume.anonymous' wsdocker volume inspect --format '{{json .Labels}}' "$LEAKED"; then
    ok "the daemon labels an anonymous volume"
else
    bad "the daemon did not label the anonymous volume: [$LAST_OUTPUT]"
fi
if [ "$PER_USER_DIND" = true ]; then
    # Never the parent daemon's volumes in this mode (ADR 0056).
    parentdocker volume create --label com.docker.volume.anonymous "$HEX64" >/dev/null 2>&1
fi

info "waiting for a volume dangling for ${AGE}s to be removed"
if wait_gone $((AGE * 6)) "$LEAKED" volumes; then
    ok "the dangling anonymous volume is gone after its age"
else
    bad "the dangling anonymous volume was never removed: [$(volumes | tr '\n' ' ')]"
    agent_log | grep prune | tail -20 | sed 's/^/        /'
fi
check_present volume "$HELD" volumes
check_present volume prune-keep volumes
check_present volume rd-prune-cache volumes
check_present volume "$HEX64" volumes
if [ "$PER_USER_DIND" = true ]; then
    parentvolumes() { parentdocker volume ls --format '{{.Name}}'; }
    check_present volume "$HEX64" parentvolumes
fi

echo
echo "== 5. the same, with volumes not asked for =="
volumes_workspace -e "WORKSPACE_PRUNE_AFTER=${AGE}s" -e WORKSPACE_PRUNE_ABOVE=off
LEAKED=$(dangling_volume anon-leak)
info "waiting past the age; the volume must stay"
sleep $((AGE + INTERVAL * 3))
check_present volume "$LEAKED" volumes

echo
echo "== 6. a dangling volume under pressure waits for the floor =="
FLOOR=40
volumes_workspace -e WORKSPACE_PRUNE_AFTER=off -e WORKSPACE_PRUNE_ABOVE=1% \
    -e "WORKSPACE_PRUNE_FLOOR=${FLOOR}s" -e WORKSPACE_PRUNE_VOLUMES=on
# Held, or the pressure rule removes alpine:3 once it is past the floor.
wsdocker create --name prune-hold alpine:3 true >/dev/null 2>&1
LEAKED=$(dangling_volume anon-leak)
wsdocker volume create prune-keep >/dev/null 2>&1
sleep $((INTERVAL * 3))
check_present volume "$LEAKED" volumes
if wait_gone $((FLOOR * 4)) "$LEAKED" volumes; then
    ok "the volume went once it was past the floor"
else
    bad "the volume survived the pressure: [$(volumes | tr '\n' ' ')]"
    agent_log | grep prune | tail -20 | sed 's/^/        /'
fi
check_present volume prune-keep volumes

if [ "$FAIL" -ne 0 ]; then
    echo
    echo "== the agent's prune log =="
    agent_log | grep -i prune | tail -40 | sed 's/^/        /'
fi

summary
