#!/usr/bin/env bash
# Who owns the directory a NAMED VOLUME lands on, asked of BOTH daemons.
#
# Reported from a Windows client against a RHEL 7 workspace: a compose project
# whose image runs as uid 1000 died the moment it started, with
#
#	EACCES: permission denied, mkdir '/home/opencode/.local/share/opencode/repos'
#
# The compose file mounts two named volumes under $HOME and the Dockerfile
# creates neither mountpoint. A fresh named volume inherits mode and ownership
# from the directory already in the image; where the image has none, the daemon
# creates the mountpoint root-owned 0755 and the volume is empty, so the
# container's own uid cannot write into it. That is Docker's rule and not ours,
# but reading the log cannot tell it apart from a share whose reported
# ownership was wrong (ADR 0046), and a user hitting it through us blames us.
#
# So each case runs twice, against the RUNNER's own daemon and through a real
# session, and the suite fails on any DIFFERENCE. The runner's daemon is the
# oracle: an identical answer from both is Docker's behaviour and not ours.
#
# It stays because it asks a second question the same way. Rewriting a named
# volume is forbidden (the volume would be replaced by an export of a directory
# that does not exist), and a rewrite is exactly what would make these two
# daemons disagree.
#
# Requires: docker, and a kernel with NFS client support.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-volown
SSH_PORT=22226
ACCOUNT=volown
TEST_IMAGE=rd-volown-probe:test

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

cleanup() { cleanup_suite "${CLIENT_PID:-}"; }
trap cleanup EXIT

# The probe image is the user's shape reduced to what matters: a uid-1000
# account, one mountpoint that EXISTS in the image and one that does not.
#
#   build_probe_image <docker-fn> <log-name>
build_probe_image() {
    local docker_fn=$1 name=$2 dir="$WORK/probe"
    mkdir -p "$dir"
    cat >"$dir/Dockerfile" <<'DOCKERFILE'
FROM alpine:3
RUN addgroup -g 1000 app && adduser -D -u 1000 -G app app
# The half the user's Dockerfile is missing: a mountpoint that is already there
# and already owned, which is the only thing a fresh volume can inherit from.
RUN mkdir -p /home/app/present && chown 1000:1000 /home/app/present
USER app
ENV HOME=/home/app
WORKDIR /home/app
DOCKERFILE
    "$docker_fn" build -t "$TEST_IMAGE" "$dir" >"$WORK/probe-build-$name.log" 2>&1
}

# mkdir_under runs the probe image with one mount and reduces the outcome to a
# single word: OK, EACCES, or the error itself.
#
#   mkdir_under <docker-fn> <mount-args...> -- <target>
mkdir_under() {
    local docker_fn=$1
    shift
    local args=()
    while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
        args+=("$1")
        shift
    done
    shift
    local target=$1 out
    out=$("$docker_fn" run --rm "${args[@]}" "$TEST_IMAGE" \
        sh -c "mkdir -p $target/repos 2>&1 && echo MKDIR-OK" 2>&1)
    case "$out" in
    *MKDIR-OK*) echo OK ;;
    *[Pp]ermission\ denied*) echo EACCES ;;
    *) echo "OTHER: $(echo "$out" | tr '\n' ' ')" ;;
    esac
}

# compare runs one case on both daemons and reports whether they AGREE, which
# is the whole question. `want` is what the runner's own daemon is expected to
# say, so a change in Docker's behaviour is caught rather than silently adopted.
#
#   compare <description> <want> <mount-args...> -- <target>
compare() {
    local what=$1 want=$2
    shift 2
    local plain ours
    plain=$(mkdir_under hostdocker "$@")
    ours=$(mkdir_under dockert "$@")
    info "$what: plain docker says [$plain], through a session [$ours]"
    if [ "$plain" != "$want" ]; then
        bad "$what: the runner's own daemon said [$plain], expected [$want]"
        return 1
    fi
    if [ "$plain" != "$ours" ]; then
        bad "$what: plain docker [$plain] but through a session [$ours] -- ours"
        return 1
    fi
    ok "$what: both daemons say [$plain]"
}

echo "== 1. build the workspace image and the client =="
if build_image && build_client; then
    ok "image and client build"
else
    bad "image or client build failed"
    exit 1
fi

export REMOTE_DOCKER_STATE_DIR="$WORK/state"
export REMOTE_DOCKER_HOST=127.0.0.1
export REMOTE_DOCKER_PORT=$SSH_PORT
export REMOTE_DOCKER_USER=$ACCOUNT
export REMOTE_DOCKER_ENDPOINT="$WORK/docker.sock"

echo
echo "== 2. enrol and start the workspace =="
mkdir -p "$WORK/keys" "$WORK/wsstate"
if ! enrol "$ACCOUNT" "$REMOTE_DOCKER_STATE_DIR"; then
    bad "enroll produced no public key"
    exit 1
fi
# The shared daemon, stated: what is measured here is Docker's volume rule and
# our share ownership, and the daemon mode changes neither.
if ! start_workspace false; then
    bad "workspace container failed to start"
    exit 1
fi
if ! wait_provisioned "$ACCOUNT"; then
    bad "the account was never provisioned"
    dump_workspace_log 30
    exit 1
fi
wait_parent_dockerd

PROJECT="$WORK/project"
mkdir -p "$PROJECT"
echo marker >"$PROJECT/marker"

echo
echo "== 3. open a session =="
CLIENT_PID=$(start_session "$REMOTE_DOCKER_STATE_DIR" "$ACCOUNT" "$REMOTE_DOCKER_ENDPOINT" "$WORK/up.log" "$PROJECT")
if wait_endpoint "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID"; then
    ok "the local Docker endpoint answers"
else
    bad "the Docker endpoint never came up"
    sed 's/^/        /' "$WORK/up.log"
    exit 1
fi
export DOCKER_HOST="unix://$REMOTE_DOCKER_ENDPOINT"

echo
echo "== 4. build the probe image on both daemons =="
if build_probe_image hostdocker plain && build_probe_image dockert ours; then
    ok "the probe image builds on both daemons"
else
    bad "the probe image failed to build"
    tail -20 "$WORK"/probe-build-*.log | sed 's/^/        /'
    exit 1
fi

echo
echo "== 5. a named volume onto a path the image does not create =="
# The user's case exactly. Expected to FAIL on both, which is what makes it
# Docker's rule rather than ours: the daemon creates the mountpoint root-owned
# and an empty volume has nothing to inherit.
compare "named volume, mountpoint absent from the image" EACCES \
    -v volown-absent:/home/app/absent -- /home/app/absent

echo
echo "== 6. a named volume onto a path the image DOES create, owned by uid 1000 =="
# The same mount with the one line the user's Dockerfile is missing. Expected
# to SUCCEED on both: a fresh volume copies the image directory's mode and
# ownership, so uid 1000 keeps it.
compare "named volume, mountpoint present and chowned in the image" OK \
    -v volown-present:/home/app/present -- /home/app/present

hostdocker volume rm volown-absent volown-present >/dev/null 2>&1
summary
