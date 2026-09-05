#!/usr/bin/env bash
# Does a share behave like a bind mount?
#
# The same probe, in the same image, runs twice: once on the runner's own
# daemon against a native bind mount, which is the oracle, and once through a
# client session against a share in the default mode (read=direct,
# write=through). test/fs-conformance/diff.sh compares the two transcripts
# step by step; every difference must be listed, with a reason, in
# test/fs-conformance/deviations-linux.txt, and every listed difference must
# still be observed.
#
# The probe is run against the oracle twice first. Two native runs that
# disagree would make every later difference noise, so that is asserted
# before anything crosses a share.
#
# Requires what test/integration.sh requires: docker, and a kernel with NFS
# client support. FSPROBE_LARGE=1 adds the probe's large-file group.
#
# The transcripts and the report are copied OUTSIDE the work directory, to
# FS_CONFORMANCE_ARTIFACTS (default /tmp/fs-conformance-artifacts), because
# the work directory is removed on exit and the workflow uploads them after.
# The first run's report is what fills the deviations file.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-fsconf
SSH_PORT=22226
ACCOUNT=fsconf
PROBE_IMAGE=fsprobe:ci
ARTIFACTS=${FS_CONFORMANCE_ARTIFACTS:-/tmp/fs-conformance-artifacts}

# A full probe run is many round trips over the tunnel, so it gets more than
# lib.sh's default two minutes.
DOCKER_TIMEOUT=600

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

PROBE_ARGS=()
[ "${FSPROBE_LARGE:-}" = 1 ] && PROBE_ARGS+=(--large)

cleanup() {
    # The oracle ran as root on the runner's daemon, so what it left under
    # $WORK is root's and rm -rf as the runner user cannot take it. The same
    # image, as root, can.
    hostdocker run --rm -v "$WORK:/work" --entrypoint sh "$PROBE_IMAGE" \
        -c 'rm -rf /work/native /work/native2' >/dev/null 2>&1
    cleanup_suite "${CLIENT_PID:-}"
}
trap cleanup EXIT

# keep copies one file into the artifact directory, if it exists.
keep() {
    [ -f "$1" ] && cp "$1" "$ARTIFACTS/"
}
mkdir -p "$ARTIFACTS"

echo "== 1. build the probe image =="
if build_probe fsprobe "$WORK/fsprobe"; then
    ok "fsprobe builds"
else
    bad "fsprobe build failed"
    exit 1
fi

mkdir -p "$WORK/ctx"
cp "$REPO/test/fs-conformance/Dockerfile" "$WORK/fsprobe" "$WORK/ctx/"
if hostdocker build -t "$PROBE_IMAGE" "$WORK/ctx" >"$WORK/probe-build.log" 2>&1; then
    ok "the probe image builds"
else
    bad "the probe image build failed"
    tail -30 "$WORK/probe-build.log" | sed 's/^/        /'
    exit 1
fi

echo
echo "== 2. the oracle: a native bind mount on the runner's daemon =="
mkdir -p "$WORK/native" "$WORK/native2"
if hostdocker run --rm -v "$WORK/native:/w" "$PROBE_IMAGE" "${PROBE_ARGS[@]}" /w \
    >"$WORK/transcript-native.txt" 2>"$WORK/probe-native.err"; then
    ok "the probe ran against a native bind mount ($(wc -l <"$WORK/transcript-native.txt") steps)"
else
    bad "the probe failed on a native bind mount"
    tail -10 "$WORK/probe-native.err" | sed 's/^/        /'
    exit 1
fi

# A fresh directory, because leftovers from the first run would be a
# difference in the input rather than in the filesystem.
if hostdocker run --rm -v "$WORK/native2:/w" "$PROBE_IMAGE" "${PROBE_ARGS[@]}" /w \
    >"$WORK/transcript-native2.txt" 2>/dev/null &&
    cmp -s "$WORK/transcript-native.txt" "$WORK/transcript-native2.txt"; then
    ok "the probe is deterministic"
else
    bad "two native runs disagree, so no difference below can be attributed to the share"
    diff "$WORK/transcript-native.txt" "$WORK/transcript-native2.txt" | head -20 | sed 's/^/        /'
fi

echo
echo "== 3. build the workspace image and the client =="
if build_image; then
    ok "image builds"
else
    bad "image build failed"
    exit 1
fi
if build_client; then
    ok "client builds"
else
    bad "client build failed"
    exit 1
fi

export REMOTE_DOCKER_STATE_DIR="$WORK/state"
export REMOTE_DOCKER_HOST=127.0.0.1
export REMOTE_DOCKER_PORT=$SSH_PORT
export REMOTE_DOCKER_USER=$ACCOUNT
export REMOTE_DOCKER_ENDPOINT="$WORK/docker.sock"

echo
echo "== 4. enrol this machine and start the workspace =="
mkdir -p "$WORK/keys" "$WORK/wsstate"
if enrol "$ACCOUNT" "$REMOTE_DOCKER_STATE_DIR"; then
    ok "keypair generated and staged as $ACCOUNT.pub"
else
    bad "enroll produced no public key"
    exit 1
fi

# The SHARED daemon, as test/integration.sh runs it: the session's endpoint
# then reaches the workspace's own dockerd, which is where `docker load`
# below has to land.
if start_workspace false; then
    ok "workspace container started"
else
    bad "workspace container failed to start"
    exit 1
fi

info "waiting for the account to be provisioned"
if wait_provisioned "$ACCOUNT"; then
    ok "the agent provisioned the account"
else
    bad "the account was never provisioned"
    dump_workspace_log 30
    exit 1
fi

info "waiting for dockerd inside the workspace"
wait_parent_dockerd

echo
echo "== 5. open a session =="
mkdir -p "$WORK/share"
cd "$WORK/share" || exit 1
"$WORK/remote-docker" remote start --foreground >"$WORK/up.log" 2>&1 &
CLIENT_PID=$!

if wait_endpoint "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID"; then
    ok "the local Docker endpoint answers"
else
    bad "the Docker endpoint never came up"
    sed 's/^/        /' "$WORK/up.log"
    exit 1
fi

export DOCKER_HOST="unix://$REMOTE_DOCKER_ENDPOINT"

# The workspace's daemon has its own image store and has never seen the
# probe image; through the session's endpoint, so the load takes the same
# path a user's `docker load` would.
if hostdocker save "$PROBE_IMAGE" | dockert load >"$WORK/load.log" 2>&1 &&
    dockert image inspect "$PROBE_IMAGE" >/dev/null 2>&1; then
    ok "the probe image is in the workspace's daemon"
else
    bad "the probe image did not load into the workspace"
    sed 's/^/        /' "$WORK/load.log"
    exit 1
fi

echo
echo "== 6. the probe against a share =="
if dockert run --rm -v "$WORK/share:/w" "$PROBE_IMAGE" "${PROBE_ARGS[@]}" /w \
    >"$WORK/transcript-linux.txt" 2>"$WORK/probe-linux.err"; then
    ok "the probe ran against a share ($(wc -l <"$WORK/transcript-linux.txt") steps)"
else
    bad "the probe failed on a share"
    tail -10 "$WORK/probe-linux.err" | sed 's/^/        /'
    dump_workspace_log 30
fi

echo
echo "== 7. compare =="
# The report is printed whether or not it passes: the first run's is what
# the deviations file is filled from, and a later failure's names the step.
if bash "$REPO/test/fs-conformance/diff.sh" \
    "$WORK/transcript-native.txt" "$WORK/transcript-linux.txt" \
    "$REPO/test/fs-conformance/deviations-linux.txt" >"$WORK/report.txt" 2>&1; then
    ok "every difference between the share and a bind mount is listed with a reason"
else
    bad "the share differs from a bind mount in ways deviations-linux.txt does not explain"
fi
sed 's/^/        /' "$WORK/report.txt"

for f in transcript-native.txt transcript-native2.txt transcript-linux.txt report.txt; do
    keep "$WORK/$f"
done
echo
info "transcripts and report kept in $ARTIFACTS"

summary
