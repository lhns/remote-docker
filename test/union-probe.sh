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
#   hot reload regresses (ADR 0014)                   5, 6d
#   a container cannot read the lower at all          6, 6b, 6c   THE VERDICT
#   the union costs more than it saves                6d
#   a userspace union brings a daemon that can die    6d
#   the per-account dind cannot run one               6e
#   the union walks slower than the mount it beats    7
#   the cache lands somewhere the kernel refuses      8
#   a mount going away leaves a container half-fed    9
#   a daemon per account cannot be reached at all     10
#   a dead union cannot be told from its leftover dir  12
#   an agent restart cannot adopt a live mount         12
#
# THE VERDICT, measured 2026-09-01 on ubuntu-latest:
#
#   The KERNEL union is out. An overlay whose lower is NFS is readable only from
#   the mount namespace that created it -- a container gets EOPNOTSUPP on any
#   lower-backed file while upper-backed files work, and so does the HOST in a
#   plain `unshare --mount` with no container involved. Binding the lower in
#   beside it does not help, so it is namespace identity rather than visibility,
#   and a volume of type overlay fails the same way whoever mounts it. docker's
#   own overlay2 is unaffected because its lower is ext4.
#
#   fuse-overlayfs is in, and the workspace image already carries it. A
#   container reads both layers through it; a write through it fires IN_MODIFY,
#   IN_CLOSE_WRITE and IN_DELETE inside the container; a deletion leaves the
#   same char-device whiteout write-back needs; and 200 cached reads cost 0.01s
#   against 0.00s for the kernel union, which is nothing beside one 160ms round
#   trip. Its new failure mode is loud: kill the daemon and the container gets
#   ENOTCONN rather than silence.
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
# start_dind runs a dind and waits for its daemon, which three sections need and
# each used to spell out. The second argument is any extra `docker run` flag,
# which in practice is --network host.
start_dind() {
    local name=$1 extra=${2:-}
    docker rm -f "$name" >/dev/null 2>&1
    # shellcheck disable=SC2086  # extra is a flag list, and word splitting is the point
    docker run -d --name "$name" --privileged $extra -e DOCKER_TLS_CERTDIR=         docker:28-dind >/dev/null 2>&1 || return 1
    for _ in $(seq 1 30); do
        docker exec "$name" docker info >/dev/null 2>&1 && return 0
        sleep 2
    done
    return 1
}

DIND=union-probe-dind

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

cleanup() {
    echo
    echo "== cleanup =="
    docker rm -f "$HOLDER" "$DIND" union-probe-dind2 union-probe-fusehold union-probe-fusewatch >/dev/null 2>&1
    docker volume rm union-probe-vol >/dev/null 2>&1
    sudo umount "$WORK/fuse-merged" 2>/dev/null
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
# Never touched by anything below, so section 6 can read them as a control.
echo "pristine at the root" >"$EXPORT_DIR/pristine-root.txt"
echo "pristine and nested" >"$EXPORT_DIR/pkg/pristine-nested.txt"

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
if (cd "$REPO/test/probes" && CGO_ENABLED=0 GOOS=linux go build -o "$WORK/watchprobe" ./watchprobe); then
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
echo "== 6. reading the lower, which is the whole point of falling through =="
# Deliberately an experiment rather than an assertion. The first run of this
# probe read a lower file from the CONTAINER and got EOPNOTSUPP where the same
# read from the HOST had worked, and one failure cannot say which of the two
# variables mattered -- host against container, or a file at the root against
# one in a subdirectory. So all four are read, pristine, before anything is
# modified underneath.
report "host, merged, at the root" cat "$MERGED/pristine-root.txt"
report "host, merged, nested" cat "$MERGED/pkg/pristine-nested.txt"
report "container, at the root" docker exec "$HOLDER" cat /w/pristine-root.txt
report "container, nested" docker exec "$HOLDER" cat /w/pkg/pristine-nested.txt
report "container, listing a lower directory" docker exec "$HOLDER" ls -la /w/pkg
report "container, listing the root" docker exec "$HOLDER" ls -la /w

# MEASURED, 2026-09-01, and the reason the kernel union is not what ships: this
# fails with EOPNOTSUPP. Not a container problem -- the same open fails from the
# host in a plain `unshare --mount` and succeeds in the original namespace, so an
# overlay whose lower is NFS is readable only from the mount namespace that
# created it. docker's own overlay2 is unaffected because its lower is ext4.
#
# Recorded rather than asserted, because a job that is permanently red is a job
# nobody reads. If a kernel ever fixes this, this line starts passing and the
# cheaper union becomes available again.
if outputs '^pristine and nested$' docker exec "$HOLDER" cat /w/pkg/pristine-nested.txt; then
    ok "the KERNEL union can now be read from a container; it could not on 2026-09-01"
else
    info "as expected: a container cannot read an NFS lower through the kernel union"
    info "  [$LAST_OUTPUT]"
fi

# The controls that say WHICH of the three ingredients is at fault, because the
# failure above is EOPNOTSUPP on open while stat and readdir both work, and that
# narrows nothing on its own.
#
# The first is the one that matters most: this project already has containers
# reading NFS mounts in every integration run, so if a plain bind of the same
# mount works, NFS in a container is fine and the fault is specific to reading
# an NFS LOWER through an overlay from another mount namespace.
report "a container reading the NFS mount directly, no overlay"     docker run --rm -v "$LOWER:/n" alpine:3 cat /n/pristine-root.txt
report "a container reading the merged mount, privileged"     docker run --rm --privileged -v "$MERGED:/w" alpine:3 cat /w/pristine-root.txt
report "a container reading the merged mount, host network and pid"     docker run --rm --network host --pid host -v "$MERGED:/w" alpine:3 cat /w/pristine-root.txt
report "the same open from the host, in a private mount namespace"     sudo unshare --mount sh -c "cat $MERGED/pristine-root.txt"
report "what the kernel said while that was happening"     sh -c "sudo dmesg | tail -15"

# Only now, the coherence question: the client edits a file the cache does not
# hold, which is the case the invalidation channel does NOT cover because there
# is nothing cached to invalidate.
echo "changed underneath" >"$EXPORT_DIR/pkg/edited.txt"
sleep 2
report "container, a lower file changed underneath" docker exec "$HOLDER" cat /w/pkg/edited.txt

echo "appeared underneath" >"$EXPORT_DIR/pkg/new-below.txt"
sleep 2
report "container, a lower file created underneath" docker exec "$HOLDER" cat /w/pkg/new-below.txt

# If the cause is that the lower is not present in the container's mount
# namespace, then putting it there should repair it -- and that is a fix we can
# actually ship, because the rewriter decides what a container mounts. Cheap to
# ask, and the answer is either a repair or a rule out.
report "the container with the LOWER bound in beside the merged mount"     docker run --rm -v "$MERGED:/w" -v "$LOWER:/rd-lower:ro" alpine:3 cat /w/pristine-root.txt


echo "== 6b. the shape the design actually ships =="
# Everything above binds a path this script mounted. What the design ships is a
# managed VOLUME of type overlay, where the DAEMON performs the overlay mount
# itself -- moby's local driver has no type whitelist, so this is expressible --
# and that is a different code path. If the daemon's own mount behaves
# differently from this script's, the daemon's is the one that matters.
if docker volume create --name union-probe-vol --opt type=overlay --opt device=overlay     --opt "o=lowerdir=$LOWER,upperdir=$UPPER,workdir=$WORKDIR" >/dev/null 2>"$WORK/vol.err"; then
    ok "the local driver accepted a volume of type overlay"
    report "a container reading the LOWER through that volume"         docker run --rm -v union-probe-vol:/w alpine:3 cat /w/pristine-root.txt
    report "a container reading the CACHE through that volume"         docker run --rm -v union-probe-vol:/w alpine:3 cat /w/late.txt
    docker volume rm union-probe-vol >/dev/null 2>&1
else
    bad "the local driver refused a volume of type overlay: $(cat "$WORK/vol.err")"
fi


echo "== 6c. fuse-overlayfs, where the union lives in userspace =="
# The kernel union is readable only from the mount namespace that made it when
# the lower is NFS -- measured above, from a plain `unshare --mount` with no
# container anywhere near it. fuse-overlayfs is the other implementation of the
# same idea, and the reason to try it is that the lower reads happen in the FUSE
# daemon's own namespace rather than the caller's.
#
# It is already in the workspace image, for the graph driver on Ceph-backed
# storage, so this costs nothing to have if it works. What it would cost is a
# userspace round trip per operation, INCLUDING cache hits, which is exactly
# what the benchmark would then have to settle.
if command -v fuse-overlayfs >/dev/null 2>&1 ||
    sudo apt-get install -y -qq fuse-overlayfs >/dev/null 2>&1; then
    mkdir -p "$WORK/fuse-merged"
    if sudo fuse-overlayfs -o "lowerdir=$LOWER,upperdir=$UPPER,workdir=$WORKDIR"         "$WORK/fuse-merged" 2>"$WORK/fuse.err"; then
        ok "fuse-overlayfs mounted over an NFS lower"
        report "the host reading the lower through it"             cat "$WORK/fuse-merged/pristine-root.txt"

        # THE assertion this script exists for, now that the kernel union is
        # ruled out: a container reading a file that is only in the lower.
        if outputs '^pristine at the root$'             docker run --rm -v "$WORK/fuse-merged:/w" alpine:3 cat /w/pristine-root.txt; then
            ok "a container reads the LOWER through the userspace union"
        else
            bad "THE DESIGN IS DEAD: no union is readable from a container: [$LAST_OUTPUT]"
        fi
        if outputs '^arrived through the union$'             docker run --rm -v "$WORK/fuse-merged:/w" alpine:3 cat /w/late.txt; then
            ok "a container reads the CACHE through the userspace union"
        else
            bad "a container could not read the cache: [$LAST_OUTPUT]"
        fi
        sudo umount "$WORK/fuse-merged" 2>/dev/null
    else
        bad "fuse-overlayfs refused the mount: $(cat "$WORK/fuse.err")"
    fi
else
    info "fuse-overlayfs is not available here"
fi


echo "== 6d. what a userspace union costs and whether it keeps the promises =="
# fuse-overlayfs answered the question the kernel union failed, so everything
# the design rests on has to be asked again of IT: the events, the overhead, the
# shape write-back reads, and what happens when the daemon behind it dies.
FUSE_MERGED=$WORK/fuse-merged
mkdir -p "$FUSE_MERGED"
if sudo fuse-overlayfs -o "lowerdir=$LOWER,upperdir=$UPPER,workdir=$WORKDIR" "$FUSE_MERGED" 2>/dev/null; then
    # What it costs. A FUSE round trip is microseconds against a 160ms RTT, so
    # the cache should still win by orders of magnitude -- but "should" is what
    # this script exists to replace.
    for i in $(seq 1 200); do echo "cached body $i" | sudo tee "$FUSE_MERGED/c$i" >/dev/null; done
    fuse_read=$( { time -p sh -c "cat $FUSE_MERGED/c* >/dev/null"; } 2>&1 | awk '/^real/ {print $2}')
    kern_read=$( { time -p sh -c "cat $MERGED/c* >/dev/null"; } 2>&1 | awk '/^real/ {print $2}')
    direct_read=$( { time -p sh -c "cat $UPPER/c* >/dev/null"; } 2>&1 | awk '/^real/ {print $2}')
    info "200 cached files: fuse ${fuse_read}s, kernel overlay ${kern_read}s, straight off disk ${direct_read}s"
    lower_via_fuse=$( { time -p sh -c "cat $FUSE_MERGED/pkg/f1 >/dev/null"; } 2>&1 | awk '/^real/ {print $2}')
    info "one file that misses and falls through to NFS: ${lower_via_fuse}s"

    # The floor for landing a batch: files applied THROUGH the union, with no
    # network in it, against the same files straight to disk. Reported, not
    # asserted.
    APPLY_SRC=$WORK/apply-src
    rm -rf "$APPLY_SRC"
    for d in $(seq 1 30); do
        mkdir -p "$APPLY_SRC/pkg$d"
        for f in $(seq 1 100); do
            head -c 2048 /dev/zero >"$APPLY_SRC/pkg$d/file$f.go"
        done
    done
    tar -cf "$WORK/apply.tar" -C "$APPLY_SRC" .
    sudo mkdir -p "$FUSE_MERGED/applied"
    apply_t=$( { time -p sudo tar -xf "$WORK/apply.tar" -C "$FUSE_MERGED/applied"; } 2>&1 | awk '/^real/ {print $2}')
    mkdir -p "$WORK/apply-disk-$$"
    disk_t=$( { time -p tar -xf "$WORK/apply.tar" -C "$WORK/apply-disk-$$"; } 2>&1 | awk '/^real/ {print $2}')
    rate=$(awk -v t="$apply_t" 'BEGIN { if (t > 0) printf "%.0f", 3000 / t; else print "?" }')
    info "3,000 files applied through the union: ${apply_t}s ($rate files/s); straight to disk: ${disk_t}s"
    rm -rf "$WORK/apply-disk-$$"

    # The ADR 0014 claim, asked of the union we would actually ship.
    if (cd "$REPO/test/probes" && CGO_ENABLED=0 GOOS=linux go build -o "$WORK/watchprobe" ./watchprobe); then
        sudo mkdir -p "$FUSE_MERGED/fusewatch"
        echo "before" | sudo tee "$FUSE_MERGED/fusewatch/reloaded.txt" >/dev/null
        if docker run -d --name union-probe-fusewatch -v "$FUSE_MERGED:/w"             -v "$WORK/watchprobe:/watchprobe" alpine:3             /watchprobe -timeout 25s /w/fusewatch >/dev/null 2>&1; then
            for _ in $(seq 1 15); do
                outputs '^READY' docker logs union-probe-fusewatch && break
                sleep 1
            done
            sleep 1
            echo "edited through the userspace union" | sudo tee "$FUSE_MERGED/fusewatch/reloaded.txt" >/dev/null
            sleep 1
            sudo rm -f "$FUSE_MERGED/fusewatch/reloaded.txt"
            timeout 60 docker wait union-probe-fusewatch >/dev/null 2>&1
            fuse_watch=$(docker logs union-probe-fusewatch 2>&1)
            docker rm -f union-probe-fusewatch >/dev/null 2>&1
            echo "$fuse_watch" | grep -E '^(RESULT|INOTIFY)' | sed 's/^/          /'
            if echo "$fuse_watch" | grep -qE '^INOTIFY '; then
                ok "a write through the userspace union reaches the container's watcher"
            else
                bad "no inotify event survived fuse-overlayfs; hot reload would need the poke after all"
            fi
        fi
    fi

    # What a container-side deletion leaves behind, which is what write-back
    # reads to learn about it.
    sudo rm -f "$FUSE_MERGED/pristine-root.txt"
    report "what a delete through the userspace union left in the upper"         sudo sh -c "ls -l $UPPER/pristine-root.txt 2>&1; getfattr -d -m - $UPPER/pristine-root.txt 2>&1 | head -5"

    # The new failure mode a userspace union brings: a daemon that can die.
    if docker run -d --name union-probe-fusehold -v "$FUSE_MERGED:/w" alpine:3 sleep 60 >/dev/null 2>&1; then
        sudo pkill -f "fuse-overlayfs.*$FUSE_MERGED"
        sleep 2
        report "a container reading after the fuse daemon was killed"             docker exec union-probe-fusehold sh -c "cat /w/c1 2>&1; echo exit=\$?"
        docker rm -f union-probe-fusehold >/dev/null 2>&1
    fi
    sudo umount "$FUSE_MERGED" 2>/dev/null
else
    bad "fuse-overlayfs would not mount for the second round of questions"
fi

echo
echo "== 6e. can a per-account dind run a userspace union at all? =="
# With a daemon per account (ADR 0019) the union has to live inside that dind's
# mount namespace, and the dind is docker:28-dind -- which is not the workspace
# image and may not carry the binary at all. That decides whether this mode can
# work in the default daemon mode or only in the shared one.
if docker run --rm docker:28-dind sh -c "command -v fuse-overlayfs" >/dev/null 2>&1; then
    ok "the per-account dind image already carries fuse-overlayfs"
else
    info "docker:28-dind does NOT carry fuse-overlayfs; the agent would have to"
    info "supply it, or the per-account dind would have to be a different image"
fi
report "what the dind has under /dev/fuse"     docker run --rm --privileged docker:28-dind sh -c "ls -l /dev/fuse 2>&1"


echo "== 7. does the lower cost a d_type fallback? =="
# overlayfs needs the file type from READDIR. If the lower cannot supply it the
# kernel stats every entry, which would make a directory walk through the union
# SLOWER than the plain mount it is meant to beat.
for i in $(seq 1 200); do echo x >"$EXPORT_DIR/pkg/f$i"; done
sleep 1
lower_walk=$( { time -p find "$LOWER" -type f >"$WORK/lower.list"; } 2>&1 | awk '/^real/ {print $2}')
merged_walk=$( { time -p find "$MERGED" -type f >"$WORK/merged.list"; } 2>&1 | awk '/^real/ {print $2}')
# The counts matter more than the seconds: a walk that found nothing is the
# fastest walk there is.
info "lower: $(wc -l <"$WORK/lower.list") files in ${lower_walk}s"
info "merged: $(wc -l <"$WORK/merged.list") files in ${merged_walk}s"
report "the same walk from inside the container"     docker exec "$HOLDER" sh -c "find /w -type f | wc -l"
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
if start_dind "$DIND"; then
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
echo "== 11. the shape that actually ships, end to end =="
# Sections 6c and 10 each work; this is the two together, which is the only
# combination Stage 1 would be built on: a union mounted INSIDE a per-account
# dind's namespace, read by a container that dind starts.
#
# Its own dind, on the HOST network, because the export here listens on the
# host's loopback while a dind has a netns of its own. The real system does not
# need that: the reverse tunnel is bound inside the account's dind precisely so
# 127.0.0.1 there is the client's NFS server (ADR 0019).
DIND2=union-probe-dind2
if start_dind "$DIND2" "--network host"; then
    # The workspace's own image carries fuse-overlayfs; docker:28-dind does not,
    # which is why a real deployment runs the workspace image for a per-account
    # daemon (agent/internal/daemons/plan.go:38). Installed here rather than
    # building that image, to keep this probe cheap and independent.
    if docker exec "$DIND2" apk add --no-cache fuse-overlayfs >/dev/null 2>&1; then
        pid2=$(docker inspect -f '{{.State.Pid}}' "$DIND2" 2>/dev/null)
        # The upper and the work directory go on the daemon's DATA ROOT, which
        # is a real filesystem, and not on /rd -- a dind's own root is
        # overlayfs, and section 8 established the kernel refuses an upper
        # there. The first run of this section did exactly that and got
        # "cannot read upper dir", which is the design's own constraint
        # arriving as a probe bug.
        sudo nsenter -t "$pid2" -m -- mkdir -p /rd/lower /rd/merged             /var/lib/docker/rd-union/upper /var/lib/docker/rd-union/work
        if sudo nsenter -t "$pid2" -m -- mount -t nfs 127.0.0.1:"$EXPORT_DIR" /rd/lower             -o nfsvers=3,nolock,noacl,soft,timeo=30,retrans=2 2>"$WORK/dindnfs.err"; then
            ok "the lower mounts inside the dind"
            # The first two attempts both failed with "cannot read upper dir"
            # for a directory that stat had just answered for, which cannot
            # both be true. So both views are printed: what the namespace we
            # mount from sees, and what the container itself sees.
            report "the upper, as the mount namespace sees it"                 sudo nsenter -t "$pid2" -m -- sh -c "stat -f -c %T /var/lib/docker/rd-union/upper; ls -lad /var/lib/docker/rd-union /var/lib/docker/rd-union/upper /var/lib/docker/rd-union/work"
            report "the upper, as the dind itself sees it"                 docker exec "$DIND2" sh -c "ls -lad /var/lib/docker/rd-union /var/lib/docker/rd-union/upper 2>&1"
            report "which fuse-overlayfs, and which version"                 sudo nsenter -t "$pid2" -m -- sh -c "command -v fuse-overlayfs; fuse-overlayfs --version 2>&1 | head -3"
            # ls and fuse-overlayfs disagreed about a directory that both of
            # them were shown -- but in SEPARATE nsenter invocations, which
            # leaves the process and the namespace entry confounded. One shell,
            # both commands, settles which.
            report "ls and the mount, in one namespace entry"                 sudo nsenter -t "$pid2" -m -- sh -c                 "ls -la /var/lib/docker/rd-union/upper && echo ---- && fuse-overlayfs -o lowerdir=/rd/lower,upperdir=/var/lib/docker/rd-union/upper,workdir=/var/lib/docker/rd-union/work /rd/merged; echo exit=\$?"

            # The suspect. nsenter -m enters the MOUNT namespace only, so the
            # process sees the dind's /proc -- a procfs tied to the dind's PID
            # namespace -- while carrying a pid from the host's. /proc/self
            # then resolves to nothing, and libfuse uses /proc/self/fd heavily.
            # ENOENT is exactly what that would produce, and exactly what
            # fuse-overlayfs reports.
            report "what /proc/self is, entering the mount namespace only"                 sudo nsenter -t "$pid2" -m -- sh -c "readlink /proc/self; ls /proc/self/fd 2>&1 | head -3"
            # nsenter here cannot fork into the pid namespace -- it has neither
            # -f nor --fork -- so the invocation this project actually uses
            # cannot be spelled with it. Our own child does it directly:
            # setns(CLONE_NEWPID), setns(CLONE_NEWNS), then run fuse-overlayfs
            # as a CHILD, which is the same set of namespaces the dind gets when
            # it runs the binary itself. That is what the assertion below uses.
            report "whether this nsenter can enter the pid namespace at all"                 sudo nsenter -t "$pid2" -m -p --fork -- true

            # And the same mount asked for by the dind ITSELF, which enters all
            # of its own namespaces the way docker does rather than the way
            # nsenter does.
            report "the same mount, run by the dind itself"                 docker exec "$DIND2" sh -c                 "mkdir -p /rd2 && fuse-overlayfs -o lowerdir=/rd/lower,upperdir=/var/lib/docker/rd-union/upper,workdir=/var/lib/docker/rd-union/work /rd2 2>&1; echo exit=\$?"

            # THE assertion, in the namespaces the union really runs in. The
            # agent's child enters the dind's pid AND mount namespaces and then
            # runs fuse-overlayfs as a child of its own, which is exactly the
            # position a process the dind started is in.
            #
            # Recorded above and not asserted: the same mount with the MOUNT
            # namespace alone, which fails with ENOENT about a directory that
            # is plainly there, because /proc/self resolves to nothing when the
            # pid namespace was left behind and libfuse leans on /proc/self/fd.
            if docker exec "$DIND2" sh -c                 "fuse-overlayfs -o lowerdir=/rd/lower,upperdir=/var/lib/docker/rd-union/upper,workdir=/var/lib/docker/rd-union/work /rd/merged"                 2>"$WORK/dindfuse.err"; then
                ok "fuse-overlayfs mounts inside the dind"
                if outputs 'pristine and nested' docker exec "$DIND2"                     docker run --rm -v /rd/merged:/w alpine:3 cat /w/pkg/pristine-nested.txt; then
                    ok "a container on the account own daemon reads the lower through the union"
                else
                    bad "the shipping shape does not work: [$LAST_OUTPUT]"
                fi
            else
                bad "fuse-overlayfs would not mount in the dind: $(cat "$WORK/dindfuse.err")"
            fi
        else
            bad "the lower would not mount in the dind: $(cat "$WORK/dindnfs.err")"
        fi
    else
        info "could not install fuse-overlayfs in the dind; section 11 is unanswered"
    fi
    docker rm -f "$DIND2" >/dev/null 2>&1
else
    info "could not start a dind on the host network; section 11 is unanswered"
fi


echo
echo "== 12. telling a live union from the directory it leaves behind =="
# The agent supervises a union it cannot enter, and it reads the mount through
# /proc/<pid>/root. Two things rest on that read being able to say "mounted"
# rather than merely "the path is there":
#
#   - a union whose server died leaves its mountpoint behind as an ordinary
#     empty directory. Read as alive, a container binds it and sees nothing,
#     with nothing to say the share is empty.
#   - after an agent restart the child is an orphan whose mount is still
#     serving. Mounting over that would strand every container bound to it, so
#     the supervisor has to recognise it and adopt it instead.
#
# The test is st_dev against the parent, which is what the child already uses
# from INSIDE the namespace. What is unmeasured, and is the whole question here,
# is whether it still answers from OUTSIDE, through /proc/<pid>/root -- so this
# section asks it rather than assuming it.
DIND3=union-probe-dind3
if start_dind "$DIND3" "--network host"; then
    pid=$(docker inspect -f '{{.State.Pid}}' "$DIND3" 2>/dev/null)
    docker exec "$DIND3" sh -c 'mkdir -p /rd/probe12/lower /rd/probe12/merged' >/dev/null 2>&1

    # An ordinary directory first, so what a dead union leaves behind has a
    # measured answer of its own rather than being inferred from the mounted one.
    # sudo, because traversing another process's root needs it. The agent has
    # that already: it runs as root in the workspace container.
    if sudo test -d "/proc/$pid/root/rd/probe12/merged"; then
        ok "the agent can see the daemon's directories through /proc/<pid>/root"

        bare=$(sudo stat -c %d "/proc/$pid/root/rd/probe12/merged" 2>/dev/null)
        up=$(sudo stat -c %d "/proc/$pid/root/rd/probe12" 2>/dev/null)
        info "an unmounted directory: dev=$bare parent=$up"
        if [ -n "$bare" ] && [ "$bare" = "$up" ]; then
            ok "an unmounted directory is on its parent's device, so it reads as NOT mounted"
        else
            bad "an unmounted directory already differs from its parent: [$bare] vs [$up]"
        fi

        # And now with something actually mounted there. tmpfs rather than a
        # union: what is under test is whether crossing a mount is visible from
        # out here, which is a property of the mount and not of its filesystem.
        if docker exec "$DIND3" mount -t tmpfs none /rd/probe12/merged >/dev/null 2>&1; then
            mounted_dev=$(sudo stat -c %d "/proc/$pid/root/rd/probe12/merged" 2>/dev/null)
            info "a mounted directory: dev=$mounted_dev parent=$up"
            if [ -n "$mounted_dev" ] && [ "$mounted_dev" != "$up" ]; then
                ok "a mount in another namespace IS visible as a device change from outside it"
            else
                bad "a mount is indistinguishable from a directory out here: [$mounted_dev] vs [$up]"
            fi
        else
            info "could not mount a tmpfs in the dind; section 12 is half answered"
        fi
    else
        bad "the daemon's directories are not reachable through /proc/$pid/root"
    fi
    docker rm -f "$DIND3" >/dev/null 2>&1
else
    info "could not start a third dind; section 12 is unanswered"
fi


echo "The answers above decide the design, not the pass count: section 4 says"
echo "whether a mounted cache can be filled at all, and section 5 says whether"
echo "hot reload survives it."
summary
