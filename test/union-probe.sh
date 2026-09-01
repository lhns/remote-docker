#!/usr/bin/env bash
# Can a cache be a union mount, and does it behave the way the design needs?
#
# Stage 0 of the cache mode: every risk in that design that is a question about
# the KERNEL rather than about our code, asked here, before any of it is built.
# It runs plain docker and mount commands and none of this project's binaries
# except the watch probe, so a failure here is the kernel's answer and not ours.
#
# The design under test: per share the workspace mounts
#
#     lower  = NFS, live and correct
#     upper  = a local directory, fast, and where writes land
#     merged = the overlay a container binds
#
# so a read hits the cache or falls through, and the cache is filled in the
# background. Two of the questions below decide whether that is possible at all:
#
#   section 4  a file written THROUGH the merged mount, after a container has
#              already looked for it and missed. If the container cannot see it,
#              a mounted cache cannot be filled and the design is dead.
#   section 5  whether a watcher inside the container sees that write. If it
#              does, ADR 0014 closes for these shares; if it does not, hot
#              reload regresses against `cached` and the mode must say so.
#
# Everything it learns is printed, including the answers that are not failures:
# this is a measurement, and a measurement that only says PASS has thrown away
# what it was run for.
#
# One section per risk, so the design's risk list and this script stay in step:
#
#   risk                                              section
#   the union cannot be built on a remote lower       2
#   a read does not fall through / a write is lost    3
#   a filled cache is invisible to the container      4     the foundation
#   hot reload regresses (ADR 0014)                   5
#   the lower changing underneath is not seen         6
#   the union walks slower than the mount it beats    7
#   the cache lands somewhere the kernel refuses      8
#   a mount going away leaves a container half-fed    9
#   a daemon per account cannot be reached at all     10
#
# The risks NOT here are the ones that are about our code rather than the
# kernel -- write-back conflicts, the collector taking a cache volume, the
# backfill budget -- and those are unit and integration tests, not this.
#
# Needs: a Linux host with docker, sudo, and a kernel with NFS client support.
# CI is the only place this runs; there is no Docker on the development machine.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
EXPORT_DIR=$WORK/export
LOWER=$WORK/lower
UPPER=$WORK/cache/upper
WORKDIR=$WORK/cache/work
MERGED=$WORK/merged
HOLDER=union-probe-holder
DIND=union-probe-dind

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

cleanup() {
    echo
    echo "== cleanup =="
    docker rm -f "$HOLDER" "$DIND" >/dev/null 2>&1
    sudo umount "$MERGED" 2>/dev/null
    sudo umount "$LOWER" 2>/dev/null
    sudo exportfs -u "127.0.0.1:$EXPORT_DIR" 2>/dev/null
    sudo rm -rf "$WORK"
}
trap cleanup EXIT

# report prints what a command said, whatever it said. Half of this script's
# value is in the answers that are neither a pass nor a failure.
report() {
    local what=$1
    shift
    local out
    out=$("$@" 2>&1)
    echo "  ....  $what:"
    echo "$out" | sed 's/^/          /'
}

echo "== 1. an NFS export on this machine =="
mkdir -p "$EXPORT_DIR" "$LOWER" "$UPPER" "$WORKDIR" "$MERGED"
mkdir -p "$EXPORT_DIR/pkg"
echo "from the lower" >"$EXPORT_DIR/only-lower.txt"
echo "original" >"$EXPORT_DIR/pkg/edited.txt"

if ! command -v exportfs >/dev/null 2>&1; then
    sudo apt-get update -qq && sudo apt-get install -y -qq nfs-kernel-server
fi
sudo modprobe nfs 2>/dev/null
sudo mkdir -p /etc/exports.d
echo "$EXPORT_DIR 127.0.0.1(rw,sync,no_subtree_check,insecure,no_root_squash)" |
    sudo tee /etc/exports.d/union-probe.exports >/dev/null
sudo exportfs -ra

# The same options the client's shares are mounted with, so what is measured is
# this project's mount rather than a generic one (core/workspace/export.go).
if sudo mount -t nfs 127.0.0.1:"$EXPORT_DIR" "$LOWER" \
    -o nfsvers=3,nolock,noacl,soft,timeo=30,retrans=2,actimeo=1,noatime; then
    ok "the lower is an NFS mount"
else
    bad "could not mount the export; nothing below can run"
    exit 1
fi

echo
echo "== 2. does overlayfs accept an NFS lower? =="
# index, metacopy and redirect_dir are pinned rather than left to the kernel's
# defaults, which vary by build, and each of which assumes a stability a remote
# lower does not offer.
if sudo mount -t overlay overlay \
    -o "lowerdir=$LOWER,upperdir=$UPPER,workdir=$WORKDIR,index=off,metacopy=off,redirect_dir=off" \
    "$MERGED" 2>"$WORK/overlay.err"; then
    ok "overlay mounted over an NFS lower"
else
    bad "overlay refused an NFS lower: $(cat "$WORK/overlay.err")"
    info "the union design cannot work on this kernel; the fallback is a"
    info "parallel prefetcher over the existing cached mount"
    exit 1
fi
report "the mount as the kernel reports it" sh -c "findmnt -no FSTYPE,OPTIONS $MERGED"

echo
echo "== 3. fallthrough, copy-up and whiteouts =="
if outputs '^from the lower$' cat "$MERGED/only-lower.txt"; then
    ok "a read falls through to the lower"
else
    bad "reading through the union: [$LAST_OUTPUT]"
fi

echo "written through the union" | sudo tee "$MERGED/written.txt" >/dev/null
if [ -f "$UPPER/written.txt" ]; then
    ok "a write lands in the cache"
else
    bad "a write did not land in the upper layer"
fi

sudo rm "$MERGED/only-lower.txt"
if [ ! -e "$MERGED/only-lower.txt" ] && [ -e "$LOWER/only-lower.txt" ]; then
    ok "a delete through the union hides a file the lower still has"
else
    bad "deleting through the union did not hide the lower's copy"
fi
# The whiteout is what write-back reads to learn the container deleted
# something. A character device 0:0 is the classic form; a newer kernel may use
# an xattr instead, which is why this reports rather than asserts a shape.
report "what the delete left in the upper layer" sudo ls -l "$UPPER"

echo
echo "== 4. THE DENTRY QUESTION =="
# The foundation of the whole design. overlayfs forbids changing the layers
# underneath a mounted overlay, so the cache cannot be filled by writing into
# the upper directory from somewhere else -- a container that already looked for
# a file and missed may never see it appear. Writing THROUGH the merged mount is
# the supported path, and this asks whether it really is.
if ! docker run -d --name "$HOLDER" -v "$MERGED:/w" alpine:3 sleep 300 >/dev/null 2>&1; then
    bad "could not start a container on the union"
    exit 1
fi
ok "a container binds the merged mount"

# It has to MISS first: a negative lookup is what gets cached, and a lookup that
# never happened proves nothing.
docker exec "$HOLDER" cat /w/late.txt >/dev/null 2>&1
docker exec "$HOLDER" cat /w/sneaked.txt >/dev/null 2>&1
info "the container looked for two files that do not exist yet"

echo "arrived through the union" | sudo tee "$MERGED/late.txt" >/dev/null
if outputs '^arrived through the union$' docker exec "$HOLDER" cat /w/late.txt; then
    ok "a file written THROUGH the merged mount is visible after a missed lookup"
else
    bad "THE DESIGN IS DEAD: a file written through the union stayed invisible: [$LAST_OUTPUT]"
fi

# The anti-case, and the reason population must not take the obvious route:
# writing into the upper directory while the overlay is mounted.
echo "sneaked into the upper" | sudo tee "$UPPER/sneaked.txt" >/dev/null
if outputs '^sneaked into the upper$' docker exec "$HOLDER" cat /w/sneaked.txt; then
    info "a write straight into the upper layer WAS visible here"
    info "(undefined behaviour per the kernel's own documentation; do not rely on it)"
else
    ok "a write straight into the upper layer was NOT visible, as the design assumes"
    info "which is why the agent fills the cache through the merged mount"
fi

echo
echo "== 5. does a watcher inside the container see it? =="
# If it does, ADR 0014 -- open since the beginning, because NFS carries no
# change notification -- closes for these shares, and closes properly: the write
# IS the event rather than a poke that approximates one.
if (cd "$REPO/core" && CGO_ENABLED=0 GOOS=linux go build -o "$WORK/watchprobe" ./probes/watchprobe); then
    # Through the merged mount, not into the lower: whether a directory created
    # in the lower shows up is section 6's question, and this section must not
    # depend on its answer.
    sudo mkdir -p "$MERGED/watched"
    echo "before" | sudo tee "$MERGED/watched/reloaded.txt" >/dev/null
    sleep 1

    if docker run -d --name union-probe-watch -v "$MERGED:/w" -v "$WORK/watchprobe:/watchprobe" \
        alpine:3 /watchprobe -timeout 30s /w/watched >/dev/null 2>&1; then
        for _ in $(seq 1 20); do
            outputs '^READY' docker logs union-probe-watch && break
            sleep 1
        done

        sleep 1
        echo "edited through the union" | sudo tee "$MERGED/watched/reloaded.txt" >/dev/null
        sleep 1
        echo "created" | sudo tee "$MERGED/watched/created.txt" >/dev/null
        sleep 1
        sudo rm "$MERGED/watched/created.txt"

        timeout 60 docker wait union-probe-watch >/dev/null 2>&1
        watch_log=$(docker logs union-probe-watch 2>&1)
        docker rm -f union-probe-watch >/dev/null 2>&1

        echo "$watch_log" | grep -E '^(RESULT|INOTIFY)' | sed 's/^/          /'
        if echo "$watch_log" | grep -qE '^INOTIFY .*(IN_MODIFY|IN_CLOSE_WRITE)'; then
            ok "a write through the union fires inotify inside the container (ADR 0014 closes here)"
        else
            bad "no inotify event reached the container's watcher through the union"
            info "hot reload would regress against cached, and the mode must say so"
        fi
        if echo "$watch_log" | grep -qE '^INOTIFY .*IN_DELETE'; then
            ok "a delete through the union fires IN_DELETE, which no mode has managed"
        else
            info "no IN_DELETE observed; deletions are still only approximable"
        fi
    else
        bad "the watch probe container would not start"
    fi
else
    bad "could not build the watch probe"
fi

echo
echo "== 6. the miss path: a file that is only in the lower =="
# What happens when the client edits a file the cache does not hold. The lower
# is live, so the bytes are right; the question is whether a stale attribute or
# a cached negative gets in the way.
echo "changed underneath" >"$EXPORT_DIR/pkg/edited.txt"
sleep 2
if outputs '^changed underneath$' docker exec "$HOLDER" cat /w/pkg/edited.txt; then
    ok "an edit to a file only in the lower is visible through the union"
else
    bad "the union served a stale lower: [$LAST_OUTPUT]"
fi

echo "appeared underneath" >"$EXPORT_DIR/pkg/new-below.txt"
sleep 2
if outputs '^appeared underneath$' docker exec "$HOLDER" cat /w/pkg/new-below.txt; then
    ok "a file created in the lower appears through the union"
else
    info "a file created in the lower did NOT appear: [$LAST_OUTPUT]"
    info "the cache would have to be told, which is what the invalidation channel is for"
fi

echo
echo "== 7. does the lower cost a d_type fallback? =="
# overlayfs needs the file type from READDIR. If the lower cannot supply it the
# kernel stats every entry, which would make a directory walk through the union
# SLOWER than the plain mount it is meant to beat.
for i in $(seq 1 200); do echo x >"$EXPORT_DIR/pkg/f$i"; done
sleep 1
lower_walk=$( { time -p find "$LOWER" -type f >/dev/null; } 2>&1 | awk '/^real/ {print $2}')
merged_walk=$( { time -p find "$MERGED" -type f >/dev/null; } 2>&1 | awk '/^real/ {print $2}')
info "a walk of 200 files: lower ${lower_walk}s, merged ${merged_walk}s"
report "what the kernel said about the lower" sh -c "sudo dmesg 2>/dev/null | grep -i 'overlayfs' | tail -5"

echo
echo "== 8. where the upper may live =="
# The kernel refuses an upper on overlayfs, which is why the cache has to be a
# volume on the daemon's data root and not a directory in the dind's own root.
report "the filesystem under the upper" sh -c "findmnt -no FSTYPE -T $UPPER"
if sudo mkdir -p /tmp/union-probe-ovl/{u,w,m,l} &&
    sudo mount -t overlay overlay -o lowerdir=/tmp/union-probe-ovl/l,upperdir=/tmp/union-probe-ovl/u,workdir=/tmp/union-probe-ovl/w /tmp/union-probe-ovl/m 2>/dev/null; then
    sudo mkdir -p /tmp/union-probe-ovl/m/{u2,w2}
    if sudo mount -t overlay overlay2 -o "lowerdir=$LOWER,upperdir=/tmp/union-probe-ovl/m/u2,workdir=/tmp/union-probe-ovl/m/w2" /tmp/union-probe-ovl/m 2>/dev/null; then
        info "an upper ON overlayfs was accepted, which the documentation says it is not"
        sudo umount /tmp/union-probe-ovl/m 2>/dev/null
    else
        ok "an upper on overlayfs is refused, as expected"
    fi
    sudo umount /tmp/union-probe-ovl/m 2>/dev/null
    sudo rm -rf /tmp/union-probe-ovl
fi

echo
echo "== 9. what a container sees when the mounts go away =="
# The lifecycle risk: an agent restart, a dind restart, an idle disconnect
# (ADR 0015). A container is holding the union right now; take the lower out
# from under it and report what it does, because "it fails" and "it silently
# shows nothing" are very different answers.
sudo umount -l "$LOWER" 2>/dev/null
sleep 1
report "reading a lower-only file with the lower unmounted" \
    docker exec "$HOLDER" sh -c "cat /w/pkg/edited.txt 2>&1; echo exit=\$?"
report "reading a cached file with the lower unmounted" \
    docker exec "$HOLDER" sh -c "cat /w/late.txt 2>&1; echo exit=\$?"
docker rm -f "$HOLDER" >/dev/null 2>&1

echo
echo "== 10. mounting inside another daemon's namespace =="
# With a daemon per account (ADR 0019) the mounts have to happen inside that
# dind's mount namespace, which the agent has never entered: core-agent/netns
# only does CLONE_NEWNET. This is the shape that work would take.
if docker run -d --name "$DIND" --privileged -e DOCKER_TLS_CERTDIR= docker:28-dind >/dev/null 2>&1; then
    for _ in $(seq 1 30); do
        docker exec "$DIND" docker info >/dev/null 2>&1 && break
        sleep 2
    done
    pid=$(docker inspect -f '{{.State.Pid}}' "$DIND" 2>/dev/null)
    if [ -n "$pid" ] && sudo nsenter -t "$pid" -m -- mkdir -p /union-probe; then
        if sudo nsenter -t "$pid" -m -- mount -t tmpfs probe /union-probe 2>"$WORK/nsenter.err"; then
            sudo nsenter -t "$pid" -m -- sh -c 'echo "from another namespace" >/union-probe/marker'
            if outputs 'from another namespace' \
                docker exec "$DIND" docker run --rm -v /union-probe:/w alpine:3 cat /w/marker; then
                ok "a mount made inside the dind's namespace reaches a container it starts"
            else
                bad "the dind's own container could not see the mount: [$LAST_OUTPUT]"
            fi
        else
            bad "could not mount inside the dind: $(cat "$WORK/nsenter.err")"
        fi
    else
        bad "could not enter the dind's mount namespace"
    fi
else
    bad "could not start a dind to test the per-account shape"
fi

echo
echo "The answers above decide the design, not the pass count: section 4 says"
echo "whether a mounted cache can be filled at all, and section 5 says whether"
echo "hot reload survives it."
summary
