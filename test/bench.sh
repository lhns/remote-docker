#!/usr/bin/env bash
# How fast is a mounted directory, and where does the time go?
#
# Every mount consistency mode is a claim about speed, and a claim about speed
# with no measurement is an opinion. This is the measurement. ADR 0002 recorded
# two numbers -- artifacts off the share is worth ~20x, protocol tuning ~2x --
# and no way to reproduce them.
#
# TWO KNOBS, and without them the numbers are a lie. CI tunnels over loopback:
# ~0 RTT and effectively infinite throughput, which is exactly what a remote
# workspace does not have. netem shapes both, so a mode that only pays off at
# distance shows it, and one that only pays off on a thin link shows that.
#
# Shaping goes on the WORKSPACE container's loopback, because that is where the
# traffic is: the local volume driver mounts NFS in the DAEMON's namespace --
# the workspace container in shared mode -- and bind-mounts the result into the
# container, so `addr=127.0.0.1` is the workspace's own lo. Per-account mode
# mounts inside each dind and would need the qdisc there instead, which is a
# different measurement.
#
# Not a gate: this is minutes long and reports numbers rather than passing or
# failing, so it runs on workflow_dispatch.
#
# Needs docker, a kernel with NFS client support, and iproute2, which is
# installed into the RUNNING workspace rather than into the image: a measurement
# tool is not something the product ships.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
# shellcheck disable=SC2034  # IMAGE and CONTAINER are read by lib.sh.
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-bench
PIN=bench-workload
SSH_PORT=22224
ACCOUNT=bench

DOCKER_TIMEOUT=600
dockert() { timeout "$DOCKER_TIMEOUT" docker "$@"; }

# Sized for CI. Every operation on every file costs a round trip, so at 80ms a
# tree ten times this size is an hour rather than a table.
FILES=${BENCH_FILES:-300}
DIRS=${BENCH_DIRS:-20}
WRITES=${BENCH_WRITES:-100}

# One row per mode per shape. The shape is what netem is told, as delay:rate,
# where a rate of 0 means unshaped; the mode is Docker's consistency word,
# written on the mount exactly as a person would write it (ADR 0042).
SHAPES=${BENCH_SHAPES:-"0ms:0 20ms:0 80ms:0 0ms:10mbit"}
MODES=${BENCH_MODES:-"consistent cached delegated"}

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

cleanup() {
    echo
    echo "== cleanup =="
    # Before the client goes: the workload container is on the workspace daemon
    # and is only reachable through the session. A short timeout because on an
    # abort the session may already be gone.
    timeout 30 docker rm -f "$PIN" >/dev/null 2>&1
    if [ -n "${CLIENT_PID:-}" ]; then
        kill "$CLIENT_PID" 2>/dev/null
        wait "$CLIENT_PID" 2>/dev/null
    fi
    hostdocker rm -f "$CONTAINER" >/dev/null 2>&1
    rm -rf "$WORK"
}
trap cleanup EXIT

# rtt is what the shaping actually produced, measured rather than assumed:
# netem delays each egress and loopback is not a wire, so a 20ms delay is not a
# 20ms round trip. busybox and iputils print different labels and the same
# min/avg/max, so the average is taken from the value list.
rtt() {
    hostdocker exec "$CONTAINER" ping -c 3 -q 127.0.0.1 2>/dev/null |
        awk -F'= ' '/min\/avg\/max/ { split($2, v, "/"); printf "%.1f", v[2] }'
}

shape() {
    local delay=$1 rate=$2
    local spec="delay $delay"
    [ "$rate" != "0" ] && spec="$spec rate $rate"
    hostdocker exec "$CONTAINER" tc qdisc replace dev lo root netem $spec >/dev/null 2>&1
}

unshape() { hostdocker exec "$CONTAINER" tc qdisc del dev lo root >/dev/null 2>&1; }

# nfsops reads the per-operation counters for the NFS mounts inside the
# workspace. This is what says whether a workload is metadata-bound
# (GETATTR/LOOKUP/ACCESS) or data-bound (READ/WRITE), which is the question that
# picks the layer to fix -- and the question compression cannot be argued
# without.
nfsops() {
    hostdocker exec "$CONTAINER" cat /proc/self/mountstats 2>/dev/null |
        awk '
            /^device .* fstype nfs/ { in_nfs = 1; next }
            /^device / { in_nfs = 0 }
            in_nfs && /^[[:space:]]+(GETATTR|LOOKUP|ACCESS|READ|WRITE):/ {
                gsub(":", "", $1); count[$1] += $2
            }
            END { for (op in count) printf "%s=%d ", op, count[op] }
        '
}

# delta subtracts two nfsops readings, so a row reports what ITS workloads cost
# rather than what the mount has done since it was made.
delta() {
    awk -v before="$1" -v after="$2" 'BEGIN {
        split(before, b, " "); split(after, a, " ")
        for (i in b) { split(b[i], p, "="); was[p[1]] = p[2] }
        for (i in a) { split(a[i], p, "="); if (p[1] != "") printf "%s=%d ", p[1], p[2] - was[p[1]] }
    }'
}

# elapsed prints the seconds a command took, to hundredths.
#
# The workloads run through exec in a container that is already up. A container
# per workload timed the tunnel's round trips for creating one -- seconds of it
# at 80ms -- on top of what is being measured, and the mount is REFCOUNTED, so
# `--rm` dropped the count to zero and unmounted: /proc/self/mountstats had
# nothing left to report and every nfs_ops column came back empty.
elapsed() {
    local start end
    start=$(date +%s.%N)
    "$@" >/dev/null 2>&1
    end=$(date +%s.%N)
    awk -v s="$start" -v e="$end" 'BEGIN { printf "%.2f", e - s }'
}

echo "== build =="
build_image && ok "image builds" || { bad "image build failed"; exit 1; }
build_client && ok "client builds" || { bad "client build failed"; exit 1; }

export REMOTE_DOCKER_STATE_DIR="$WORK/state"
export REMOTE_DOCKER_HOST=127.0.0.1
export REMOTE_DOCKER_PORT=$SSH_PORT
export REMOTE_DOCKER_USER=$ACCOUNT
export REMOTE_DOCKER_ENDPOINT="$WORK/docker.sock"

echo
echo "== workspace =="
mkdir -p "$WORK/keys" "$WORK/wsstate"
enrol "$ACCOUNT" "$REMOTE_DOCKER_STATE_DIR" || { bad "enrol failed"; exit 1; }
start_workspace false || { bad "the workspace failed to start"; exit 1; }
wait_provisioned "$ACCOUNT" || { bad "the account was never provisioned"; exit 1; }
wait_parent_dockerd || exit 1
if hostdocker exec "$CONTAINER" apk add --no-cache iproute2 >/dev/null 2>&1; then
    ok "workspace up, with shaping available"
else
    bad "iproute2 did not install; only the unshaped row is meaningful"
fi

echo
echo "== the session =="
# Watching, because `cached` rests on it: the client pokes what changed, and a
# long attribute cache with nothing to invalidate it is refused rather than
# served (ADR 0042).
REMOTE_DOCKER_WATCH=partial "$WORK/remote-docker" remote start --foreground >"$WORK/up.log" 2>&1 &
CLIENT_PID=$!
if wait_endpoint "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID"; then
    ok "the local Docker endpoint answers"
else
    bad "the endpoint never came up"
    sed 's/^/        /' "$WORK/up.log"
    exit 1
fi
export DOCKER_HOST="unix://$REMOTE_DOCKER_ENDPOINT"

echo
echo "== a project-shaped tree: $FILES files across $DIRS directories =="
PROJECT="$WORK/project"
filler=$(head -c 400 /dev/zero | tr '\0' 'x')
for d in $(seq 1 "$DIRS"); do mkdir -p "$PROJECT/pkg$d"; done
for f in $(seq 1 "$FILES"); do
    printf 'package p%d\n// %s\n' "$f" "$filler" \
        >"$PROJECT/pkg$((f % DIRS + 1))/file$f.go"
done
mkdir -p "$PROJECT/out"
ok "tree built"

# The image is pulled and the volume created before anything is timed, so the
# first row does not carry a pull.
dockert run --rm -v "$PROJECT:/w" alpine:3 true >/dev/null 2>&1
ok "warm"

printf '\n%-12s %-11s %-8s %-8s %-8s %-8s %-8s %s\n' \
    shape mode rtt_ms start_s walk_s read_s write_s nfs_ops
printf '%s\n' "$(printf '=%.0s' $(seq 1 95))"

for spec in $SHAPES; do
    delay=${spec%%:*}
    rate=${spec##*:}

    if [ "$delay" = "0ms" ] && [ "$rate" = "0" ]; then
        unshape
    elif ! shape "$delay" "$rate"; then
        info "cannot shape $spec; skipping the row"
        continue
    fi
    measured=$(rtt)

    for mode in $MODES; do
        # One container per row, started before anything is timed and holding
        # the mount for the whole of it: a fresh mount, so the row starts with
        # a cold attribute cache, and a live one, so the counters can still be
        # read at the end.
        #
        # The mode is written on the mount exactly as a person writes it. A
        # share whose consistency changed has its volume rebuilt, which is what
        # makes switching free, so these rows exercise that too.
        #
        # start_s is that container coming up, and it is a column rather than
        # overhead: a delegated share copies the whole tree across before the
        # container exists, and a mode whose reads are cheap because it paid up
        # front has to show what it paid.
        dockert rm -f "$PIN" >/dev/null 2>&1
        start=$(elapsed dockert run -d --name "$PIN" -v "$PROJECT:/w:$mode" \
            alpine:3 sleep 3600)
        if ! outputs true dockert inspect -f '{{.State.Running}}' "$PIN"; then
            bad "no workload container for $spec $mode: $LAST_OUTPUT"
            continue
        fi

        before=$(nfsops)
        walk=$(elapsed dockert exec "$PIN" sh -c 'find /w -type f | wc -l')
        read=$(elapsed dockert exec "$PIN" sh -c 'find /w -name "*.go" -exec cat {} +')
        # Escaped: seq and the loop variable belong to the shell in the container.
        burst="for i in \$(seq 1 $WRITES); do echo built >/w/out/\$i; done"
        write=$(elapsed dockert exec "$PIN" sh -c "$burst")
        after=$(nfsops)
        dockert rm -f "$PIN" >/dev/null 2>&1

        printf '%-12s %-11s %-8s %-8s %-8s %-8s %-8s %s\n' \
            "$delay/$rate" "$mode" "$measured" "$start" "$walk" "$read" "$write" \
            "$(delta "$before" "$after")"
    done
done

unshape

# The cache's own numbers, which the table above cannot hold: they are about a
# mode that has a state -- filling, settled -- rather than a speed.
#
# This is what "a mode with no row does not ship" means for the cache (ADR
# 0044). Four claims, four columns:
#
#   cold      a read straight after the container starts, before the fill has
#             got anywhere. Should look like `cached`, because that is what it
#             IS: a miss goes to the live mount.
#   settle    how long until the cache holds the whole tree. The number
#             compression would improve, and the one that scales with the tree
#             rather than with the latency.
#   warm      the same read afterwards. The gap between cold and warm is the
#             entire feature.
#   invalidate  an edit here, then the container reading it.
#   writeback   the container writing, then this machine reading it.
echo
printf '%-12s %-8s %-8s %-8s %-8s %-11s %-10s
'     shape rtt_ms cold_s settle_s warm_s invalidate_s writeback_s
printf '%s
' "$(printf '=%.0s' $(seq 1 72))"

for spec in $SHAPES; do
    case " $MODES " in *" delegated "*) ;; *) break ;; esac

    delay=${spec%%:*}
    rate=${spec##*:}
    if [ "$delay" = "0ms" ] && [ "$rate" = "0" ]; then
        unshape
    elif ! shape "$delay" "$rate"; then
        continue
    fi
    measured=$(rtt)

    dockert rm -f "$PIN" >/dev/null 2>&1
    if ! dockert run -d --name "$PIN" -v "$PROJECT:/w:delegated"         alpine:3 sleep 3600 >"$WORK/bench-deleg.log" 2>&1; then
        # What docker said, and what the workspace was doing. A row that is
        # simply absent from a benchmark reads as a mode that was not measured
        # rather than one that failed, which is the more misleading of the two.
        bad "no delegated container for $spec"
        sed 's/^/        /' "$WORK/bench-deleg.log"
        hostdocker logs "$CONTAINER" 2>&1 |
            grep -iE "union|cache|fuse" | tail -10 | sed 's/^/        workspace: /'
        continue
    fi

    # Straight away, before the fill has had a chance: this is the miss path.
    cold=$(elapsed dockert exec "$PIN" sh -c 'find /w -name "*.go" -exec cat {} +')

    # Settled is what the session itself says, rather than a guess at how long
    # a copy ought to take.
    settle_start=$(date +%s.%N)
    settled=timeout
    for _ in $(seq 1 300); do
        if outputs 'cache .*files, cached$' "$WORK/remote-docker" remote status; then
            settled=$( { date +%s.%N; } | awk -v s="$settle_start" '{ printf "%.2f", $1 - s }')
            break
        fi
        sleep 1
    done

    warm=$(elapsed dockert exec "$PIN" sh -c 'find /w -name "*.go" -exec cat {} +')

    # An edit here, and how long until the container sees it.
    echo "edited at $(date +%s)" >"$PROJECT/pkg1/invalidate-probe"
    inv_start=$(date +%s.%N)
    inv=timeout
    for _ in $(seq 1 60); do
        if outputs 'edited at' dockert exec "$PIN" cat /w/pkg1/invalidate-probe; then
            inv=$( { date +%s.%N; } | awk -v s="$inv_start" '{ printf "%.2f", $1 - s }')
            break
        fi
        sleep 1
    done

    # And the other direction, which no earlier mode had at all.
    dockert exec "$PIN" sh -c 'echo "from the container" >/w/writeback-probe' >/dev/null 2>&1
    wb_start=$(date +%s.%N)
    wb=timeout
    for _ in $(seq 1 60); do
        if [ -f "$PROJECT/writeback-probe" ]; then
            wb=$( { date +%s.%N; } | awk -v s="$wb_start" '{ printf "%.2f", $1 - s }')
            break
        fi
        sleep 1
    done
    if [ "$wb" = timeout ]; then
        # A number that is missing has to say why, or the table reports a
        # feature as slow when it did not run at all.
        hostdocker logs "$CONTAINER" 2>&1 |
            grep -iE "cache layer|union|cache request" | tail -5 | sed 's/^/        workspace: /'
    fi
    rm -f "$PROJECT/writeback-probe" "$PROJECT/pkg1/invalidate-probe"

    printf '%-12s %-8s %-8s %-8s %-8s %-11s %-10s
'         "$delay/$rate" "$measured" "$cold" "$settled" "$warm" "$inv" "$wb"
    dockert rm -f "$PIN" >/dev/null 2>&1
done

unshape
echo
echo "Compare a mode against consistent at the same shape: what a mode has to"
echo "show is that its wall-clock stops tracking the latency knob. nfs_ops is"
echo "where the time went, and a mode with no row does not ship."
