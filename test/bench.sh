#!/usr/bin/env bash
# How fast is a mounted directory, and where does the time go?
#
# CI tunnels over loopback, ~0 RTT and unbounded throughput, so netem shapes
# both; without that the numbers are a lie. It shapes the WORKSPACE container's
# lo, where NFS runs in shared mode (per-account mode mounts inside each dind).
#
# Not a gate: it reports numbers. It runs from the `bench` pull-request label or
# workflow_dispatch (.github/workflows/bench.yml).
#
# Needs docker, a kernel with NFS client support, and iproute2, installed into
# the RUNNING workspace because the product does not ship it.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
# shellcheck disable=SC2034  # IMAGE is read by lib.sh's build_image.
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-bench
PIN=bench-workload
SSH_PORT=22224
ACCOUNT=bench

DOCKER_TIMEOUT=600

# Sized for CI: at 80ms a tree ten times this size is an hour.
FILES=${BENCH_FILES:-300}

DIRS=${BENCH_DIRS:-20}
WRITES=${BENCH_WRITES:-100}

# A shape is delay:rate for netem, rate 0 meaning unshaped; a mode is written
# on the mount as a person would (ADR 0042).
SHAPES=${BENCH_SHAPES:-"0ms:0 10ms:0 20ms:0 80ms:0 0ms:10mbit"}
MODES=${BENCH_MODES:-"read=direct,write=through read=cached,write=through read=cached,write=back read=cached,write=ephemeral"}

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

cleanup() {
    # Before the client goes: the workload container is reachable only through
    # the session, which on an abort may already be gone.
    timeout 30 docker rm -f "$PIN" >/dev/null 2>&1
    cleanup_suite "${CLIENT_PID:-}"
}
trap cleanup EXIT

# rtt is measured: on loopback netem delays both directions, so a 20ms delay is
# not a 20ms round trip. The average comes from the value list, which busybox
# and iputils label differently.
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

# nfsops reads the workspace's NFS per-operation counters: metadata-bound
# (GETATTR/LOOKUP/ACCESS) or data-bound (READ/WRITE).
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

delta() {
    awk -v before="$1" -v after="$2" 'BEGIN {
        split(before, b, " "); split(after, a, " ")
        for (i in b) { split(b[i], p, "="); was[p[1]] = p[2] }
        for (i in a) { split(a[i], p, "="); if (p[1] != "") printf "%s=%d ", p[1], p[2] - was[p[1]] }
    }'
}

# elapsed prints the seconds a command took, to hundredths.
#
# Workloads run through exec in a container already up. `run --rm` would time
# creating a container too, and would unmount the REFCOUNTED volume, leaving
# mountstats (and every nfs_ops column) empty.
elapsed() {
    local start end
    start=$(date +%s.%N)
    "$@" >/dev/null 2>&1
    end=$(date +%s.%N)
    awk -v s="$start" -v e="$end" 'BEGIN { printf "%.2f", e - s }'
}

echo "== build =="
build_all

export REMOTE_DOCKER_STATE_DIR="$WORK/state"
export REMOTE_DOCKER_HOST=127.0.0.1
export REMOTE_DOCKER_PORT=$SSH_PORT
export REMOTE_DOCKER_USER=$ACCOUNT
export REMOTE_DOCKER_ENDPOINT="$WORK/docker.sock"

echo
echo "== workspace =="
enrol_machine "$ACCOUNT" "$REMOTE_DOCKER_STATE_DIR"
workspace_up false "$ACCOUNT" || exit 1
if hostdocker exec "$CONTAINER" apk add --no-cache iproute2 >/dev/null 2>&1; then
    ok "workspace up, with shaping available"
else
    bad "iproute2 did not install; only the unshaped row is meaningful"
fi

echo
echo "== the session =="
# Watching, because read=cached refuses to run without it (ADR 0042).
REMOTE_DOCKER_WATCH=partial "$WORK/remote-docker" remote start --foreground >"$WORK/up.log" 2>&1 &
CLIENT_PID=$!
endpoint_up "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID" "$WORK/up.log" || exit 1
export DOCKER_HOST="unix://$REMOTE_DOCKER_ENDPOINT"

echo
echo "== a project-shaped tree: $FILES files across $DIRS directories =="
PROJECT="$WORK/project"
filler=$(head -c 400 /dev/zero | tr '\0' 'x')
# Every row of the table gets a fresh tree; PROJECT is the warm-up's.
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

# So the first row does not carry a pull.
dockert run --rm -v "$PROJECT:/w" alpine:3 true >/dev/null 2>&1
ok "warm"

# --- the table --------------------------------------------------------------
#
# One row per (prefetch, shape, mode, workload), each in a FRESH container over
# a FRESH tree: a share id comes from its path, so a reused directory reuses a
# full cache. Prefetch is read at client start, so the client restarts per
# value, all on ONE runner so the numbers compare.
#
# The workloads are the simulator's (dircache/sim_test.go), so the two tables
# lie side by side:
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

restart_client() {
    local policy=$1
    stop_pid "$CLIENT_PID"
    REMOTE_DOCKER_WATCH=partial REMOTE_DOCKER_PREFETCH="$policy" \
        "$WORK/remote-docker" remote start --foreground >"$WORK/up-$policy.log" 2>&1 &
    CLIENT_PID=$!
    if ! wait_endpoint "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID"; then
        bad "the client did not come back under prefetch=$policy"
        sed 's/^/        /' "$WORK/up-$policy.log" | tail -20
        return 1
    fi
    # A throwaway carries the reconnect so no row does.
    dockert run --rm -v "$PROJECT:/w" alpine:3 true >/dev/null 2>&1
    return 0
}

# workload_files prints the host paths a workload reads. `write` reads nothing.
workload_files() {
    local w=$1 tree=$2
    case $w in
    dense | dense3 | parallel) find "$tree" -name "*.go" | sort ;;
    subtree) printf '%s\n' "$tree"/pkg1/*.go "$tree"/pkg2/*.go ;;
    sparse) find "$tree" -name "*.go" | sort | awk 'NR % 50 == 0' ;;
    write) ;;
    esac
}

workload_cmd() {
    local w=$1 tree=$2 list
    list=$(workload_files "$w" "$tree" | sed "s#^$tree#/w#" | tr '\n' ' ')
    # The dense rows walk the tree in the container, as a build does, and as
    # the rows recorded in ADR 0045 did.
    case $w in
    dense) echo 'find /w -name "*.go" -exec cat {} +' ;;
    dense3) echo 'for i in 1 2 3; do find /w -name "*.go" -exec cat {} +; done' ;;
    parallel) echo 'find /w -name "*.go" -print0 | xargs -0 -P8 -n 20 cat' ;;
    subtree | sparse) echo "cat $list" ;;
    write) echo "for i in \$(seq 1 $WRITES); do echo built >/w/out/\$i; done" ;;
    esac
}

# workload_bytes is how much a workload reads, for the amp column.
workload_bytes() {
    local w=$1 tree=$2 n
    n=$(workload_files "$w" "$tree" | xargs -r cat | wc -c)
    case $w in
    dense3) echo $((n * 3)) ;;
    *) echo "$n" ;;
    esac
}

# fetched_bytes turns `remote status`'s human-readable "sent" back into bytes.
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
                # Let the sender drain what the reads triggered.
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
