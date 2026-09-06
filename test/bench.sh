#!/usr/bin/env bash
# How fast is a mounted directory, and where does the time go?
#
# This is the measurement. ADR 0002 recorded two numbers -- artifacts off the
# share is worth ~20x, protocol tuning ~2x -- and no way to reproduce them.
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
# shellcheck disable=SC2034  # IMAGE is read by lib.sh's build_image.
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-bench
PIN=bench-workload
SSH_PORT=22224
ACCOUNT=bench

# Read by lib.sh's dockert: a shaped link makes every command slow.
DOCKER_TIMEOUT=600

# Sized for CI. Every operation on every file costs a round trip, so at 80ms a
# tree ten times this size is an hour rather than a table.
FILES=${BENCH_FILES:-300}

DIRS=${BENCH_DIRS:-20}
WRITES=${BENCH_WRITES:-100}

# The shape is what netem is told, as delay:rate, where a rate of 0 means
# unshaped; the mode is written on the mount exactly as a person would write
# it (ADR 0042): Docker's word, or one of ours.
SHAPES=${BENCH_SHAPES:-"0ms:0 10ms:0 20ms:0 80ms:0 0ms:10mbit"}
MODES=${BENCH_MODES:-"read=direct,write=through read=cached,write=through read=cached,write=back read=cached,write=ephemeral"}

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
# build_tree makes a project-shaped tree of a given size. Every row of the
# table gets a fresh one, and the first, PROJECT, is the warm-up's.
build_tree() {
    local root=$1 files=$2 d f
    for d in $(seq 1 "$DIRS"); do mkdir -p "$root/pkg$d"; done
    for f in $(seq 1 "$files"); do
        printf 'package p%d\n// %s\n' "$f" "$filler" \
            >"$root/pkg$((f % DIRS + 1))/file$f.go"
    done
    mkdir -p "$root/out"
}

build_tree "$PROJECT" "$FILES"
ok "tree built"

# The image is pulled and the volume created before anything is timed, so the
# first row does not carry a pull.
dockert run --rm -v "$PROJECT:/w" alpine:3 true >/dev/null 2>&1
ok "warm"

# --- the table --------------------------------------------------------------
#
# One row per (prefetch, shape, mode, workload), each in a FRESH container over a
# FRESH tree, because a share id comes from its path and reusing a directory
# reuses its cache volume: from the second row on, every cache would already be
# full.
#
# Prefetch is the client's setting, so the client is restarted per value. All
# run in ONE job on ONE runner, which is the only way their numbers are
# comparable (ADR 0044).
#
# The workloads are the ones the simulator runs (dircache/sim_test.go), so the
# two tables lay side by side and disagreement between them is a finding:
#
#   dense     every file once, which is the one row an eager fill wins
#   subtree   two of twenty directories, which is what a build looks like
#   sparse    six files across the tree, which is what an eager fill is worst at
#   dense3    every file three times in one container: a repeat is what a cache
#             is for
#   parallel  every file once with eight readers, the only row that can see
#             the server answering one request at a time
#   write     a burst of small files, where the write mode shows
#
# Columns: fetched is what the cache channel carried for that share, from
# `remote status`, and amp is that over the bytes the workload read.

PREFETCH=${BENCH_PREFETCH:-"off eager tree"}
WORKLOADS=${BENCH_WORKLOADS:-"dense subtree sparse dense3 parallel write"}

# restart_client brings the session back under a prefetch setting, which is
# read at start.
restart_client() {
    local policy=$1
    kill "$CLIENT_PID" 2>/dev/null
    wait "$CLIENT_PID" 2>/dev/null
    REMOTE_DOCKER_WATCH=partial REMOTE_DOCKER_PREFETCH="$policy" \
        "$WORK/remote-docker" remote start --foreground >"$WORK/up-$policy.log" 2>&1 &
    CLIENT_PID=$!
    if ! wait_endpoint "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID"; then
        bad "the client did not come back under prefetch=$policy"
        sed 's/^/        /' "$WORK/up-$policy.log" | tail -20
        return 1
    fi
    # The first container after a restart carries the reconnect; a throwaway
    # takes it so no row does.
    dockert run --rm -v "$PROJECT:/w" alpine:3 true >/dev/null 2>&1
    return 0
}

# workload_files prints the host paths a workload reads, one per line, over a
# tree built by build_tree: pkg<d>/file<f>.go. `write` reads nothing.
workload_files() {
    local w=$1 tree=$2
    case $w in
    dense | dense3 | parallel) find "$tree" -name "*.go" | sort ;;
    subtree) printf '%s\n' "$tree"/pkg1/*.go "$tree"/pkg2/*.go ;;
    sparse) find "$tree" -name "*.go" | sort | awk 'NR % 50 == 0' ;;
    write) ;;
    esac
}

# workload_cmd is the shell the container runs for a workload, over the files
# workload_files names, spelled as the container sees them.
workload_cmd() {
    local w=$1 tree=$2 list
    list=$(workload_files "$w" "$tree" | sed "s#^$tree#/w#" | tr '\n' ' ')
    # The dense rows walk the tree in the container, as a build does, so the
    # directory walk is in the measurement and the rows stay comparable with
    # the recorded ones (ADR 0045).
    case $w in
    dense) echo 'find /w -name "*.go" -exec cat {} +' ;;
    dense3) echo 'for i in 1 2 3; do find /w -name "*.go" -exec cat {} +; done' ;;
    parallel) echo 'find /w -name "*.go" -print0 | xargs -0 -P8 -n 20 cat' ;;
    subtree | sparse) echo "cat $list" ;;
    write) echo "for i in \$(seq 1 $WRITES); do echo built >/w/out/\$i; done" ;;
    esac
}

# workload_bytes is how much a workload reads, from the host's copy of the
# tree, for the amplification column.
workload_bytes() {
    local w=$1 tree=$2 n
    n=$(workload_files "$w" "$tree" | xargs -r cat | wc -c)
    case $w in
    dense3) echo $((n * 3)) ;;
    *) echo "$n" ;;
    esac
}

# fetched_bytes reads what `remote status` says the cache channel carried for
# a share, and turns humanBytes back into a number.
fetched_bytes() {
    local tree=$1
    "$WORK/remote-docker" remote status 2>/dev/null |
        grep -F "$tree" | sed -nE 's/.* ([0-9.]+)([KMGT]?)B sent.*/\1 \2/p' | head -1 |
        awk '{ n = $1; u = $2; m = 1; if (u == "K") m = 1024; if (u == "M") m = 1048576; if (u == "G") m = 1073741824; printf "%d", n * m }'
}

printf '\n%-8s %-12s %-32s %-9s %-7s %-8s %-8s %-10s %-6s %s\n' \
    prefetch shape mode workload rtt_ms start_s time_s fetched amp nfs_ops
printf '%s\n' "$(printf '=%.0s' $(seq 1 110))"

first_policy=${PREFETCH%% *}
row=0
for policy in $PREFETCH; do
    restart_client "$policy" || continue

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
            # A mode with no union does not depend on the setting: once.
            case $mode in
            *write=through) [ "$policy" = "$first_policy" ] || continue ;;
            esac

            for w in $WORKLOADS; do
                row=$((row + 1))
                tree="$WORK/bench-$row"
                rm -rf "$tree"
                build_tree "$tree" "$FILES"
                cmd=$(workload_cmd "$w" "$tree")
                want=$(workload_bytes "$w" "$tree")

                dockert rm -f "$PIN" >/dev/null 2>&1
                start=$(elapsed dockert run -d --name "$PIN" -v "$tree:/w:$mode" alpine:3 sleep 3600)
                if ! outputs true dockert inspect -f '{{.State.Running}}' "$PIN"; then
                    bad "no workload container for $policy $spec $mode $w: $LAST_OUTPUT"
                    rm -rf "$tree"
                    continue
                fi

                before=$(nfsops)
                took=$(elapsed dockert exec "$PIN" sh -c "$cmd")
                after=$(nfsops)
                # What the cache channel carried, once the sender has had a
                # moment to drain what the reads triggered.
                sleep 3
                fetched=$(fetched_bytes "$tree")
                [ -n "$fetched" ] || fetched=0
                amp=$(awk -v f="$fetched" -v r="$want" 'BEGIN { if (r > 0) printf "%.2f", f / r; else print "-" }')
                dockert rm -f "$PIN" >/dev/null 2>&1
                rm -rf "$tree"

                printf '%-8s %-12s %-32s %-9s %-7s %-8s %-8s %-10s %-6s %s\n' \
                    "$policy" "$delay/$rate" "$mode" "$w" "$measured" "$start" "$took" \
                    "$fetched" "$amp" "$(delta "$before" "$after")"
            done
        done
    done
done

unshape
echo
echo "Pass criteria: ADR 0045, docs/adr/0045-prefetch-follows-the-reads.md."
