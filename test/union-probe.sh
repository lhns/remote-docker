#!/usr/bin/env bash
# Can a cache be a union mount, and does it behave the way the design needs?
#
# Every risk in the cache mode that is a question about the KERNEL rather than
# about our code: plain docker and mount commands, and of this project only the
# watch probe. Everything learned is printed, failures included.
#
# The design under test (ADR 0044): per share the workspace mounts
#
#     lower  = NFS, live and correct
#     upper  = a local directory, fast, and where writes land
#     merged = the overlay a container binds
#
# One section per risk:
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
#   write-back's times are not what parity rests on    14    recorded
#
# THE VERDICT (ADR 0044): the kernel union is out, because an overlay whose
# lower is NFS is readable only from the mount namespace that created it;
# fuse-overlayfs is in, and the workspace image carries it.
#
# Needs: a Linux host with docker, sudo, and a kernel with NFS client support.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
EXPORT_DIR=$WORK/export
LOWER=$WORK/lower
UPPER=$WORK/cache/upper
WORKDIR=$WORK/cache/work
MERGED=$WORK/merged
HOLDER=union-probe-holder
# Named here so cleanup takes them all, whichever path a section left by.
DIND=union-probe-dind
DIND2=union-probe-dind2
DIND3=union-probe-dind3
# start_dind <name> [extra docker run flags] runs a dind and waits for its daemon.
start_dind() {
    local name=$1 extra=${2:-}
    docker rm -f "$name" >/dev/null 2>&1
    # shellcheck disable=SC2086  # extra is a flag list, and word splitting is the point
    docker run -d --name "$name" --privileged $extra -e DOCKER_TLS_CERTDIR= \
        docker:28-dind >/dev/null 2>&1 || return 1
    for _ in $(seq 1 30); do
        docker exec "$name" docker info >/dev/null 2>&1 && return 0
        sleep 2
    done
    return 1
}

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

cleanup() {
    echo
    echo "== cleanup =="
    docker rm -f "$HOLDER" "$DIND" "$DIND2" "$DIND3" \
        union-probe-watch union-probe-fusehold union-probe-fusewatch >/dev/null 2>&1
    docker volume rm union-probe-vol >/dev/null 2>&1
    sudo umount "$WORK/fuse-merged" 2>/dev/null
    sudo umount "$MERGED" 2>/dev/null
    sudo umount "$LOWER" 2>/dev/null
    sudo exportfs -u "127.0.0.1:$EXPORT_DIR" 2>/dev/null
    sudo rm -rf "$WORK"
}
trap cleanup EXIT

# report prints what a command said, whatever it said.
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

# Modelled on the options shares are mounted with (core/workspace/export.go).
if sudo mount -t nfs 127.0.0.1:"$EXPORT_DIR" "$LOWER" \
    -o nfsvers=3,nolock,noacl,soft,timeo=30,retrans=2,actimeo=1,noatime; then
    ok "the lower is an NFS mount"
else
    bad "could not mount the export; nothing below can run"
    exit 1
fi

echo
echo "== 2. does overlayfs accept an NFS lower? =="
# index, metacopy and redirect_dir are pinned: the defaults vary by kernel
# build, and each assumes a stability a remote lower does not offer.
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
# The whiteout is how write-back learns of a delete: a 0:0 character device, or
# an xattr on a newer kernel, so its shape is reported rather than asserted.
report "what the delete left in the upper layer" sudo ls -l "$UPPER"

echo
echo "== 4. THE DENTRY QUESTION =="
# overlayfs forbids changing the layers under a mounted overlay, so the cache
# can only be filled THROUGH the merged mount. If a container that already
# missed on a file cannot then see it, a mounted cache cannot be filled at all.
if ! docker run -d --name "$HOLDER" -v "$MERGED:/w" alpine:3 sleep 300 >/dev/null 2>&1; then
    bad "could not start a container on the union"
    exit 1
fi
ok "a container binds the merged mount"

# It has to MISS first: a negative lookup is what gets cached.
docker exec "$HOLDER" cat /w/late.txt >/dev/null 2>&1
docker exec "$HOLDER" cat /w/sneaked.txt >/dev/null 2>&1
info "the container looked for two files that do not exist yet"

echo "arrived through the union" | sudo tee "$MERGED/late.txt" >/dev/null
if outputs '^arrived through the union$' docker exec "$HOLDER" cat /w/late.txt; then
    ok "a file written THROUGH the merged mount is visible after a missed lookup"
else
    bad "THE DESIGN IS DEAD: a file written through the union stayed invisible: [$LAST_OUTPUT]"
fi

# The anti-case: writing into the upper directory while the overlay is mounted.
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
# If it does, ADR 0014 closes for these shares: the write IS the event rather
# than a poke that approximates one.
if build_probe watchprobe "$WORK/watchprobe"; then
    # Through the merged mount, so this does not depend on section 6's answer.
    sudo mkdir -p "$MERGED/watched"
    echo "before" | sudo tee "$MERGED/watched/reloaded.txt" >/dev/null
    sleep 1

    if docker run -d --name union-probe-watch -v "$MERGED:/w" -v "$WORK/watchprobe:/watchprobe" \
        alpine:3 /watchprobe -timeout 30s /w/watched >/dev/null 2>&1; then
        wait_ready union-probe-watch 20

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
# Host against container, root against nested: all four read, pristine, before
# anything is modified underneath.
report "host, merged, at the root" cat "$MERGED/pristine-root.txt"
report "host, merged, nested" cat "$MERGED/pkg/pristine-nested.txt"
report "container, at the root" docker exec "$HOLDER" cat /w/pristine-root.txt
report "container, nested" docker exec "$HOLDER" cat /w/pkg/pristine-nested.txt
report "container, listing a lower directory" docker exec "$HOLDER" ls -la /w/pkg
report "container, listing the root" docker exec "$HOLDER" ls -la /w

# Fails with EOPNOTSUPP, which is why the kernel union does not ship (ADR
# 0044). Recorded, not asserted: if a kernel ever fixes it, this line passes.
if outputs '^pristine and nested$' docker exec "$HOLDER" cat /w/pkg/pristine-nested.txt; then
    ok "the KERNEL union can now be read from a container, which ADR 0044 says it cannot"
else
    info "as expected: a container cannot read an NFS lower through the kernel union"
    info "  [$LAST_OUTPUT]"
fi

# Controls for WHICH ingredient is at fault (open fails while stat and readdir
# work). A plain bind of the NFS mount working puts it on the overlay.
report "a container reading the NFS mount directly, no overlay" \
    docker run --rm -v "$LOWER:/n" alpine:3 cat /n/pristine-root.txt
report "a container reading the merged mount, privileged" \
    docker run --rm --privileged -v "$MERGED:/w" alpine:3 cat /w/pristine-root.txt
report "a container reading the merged mount, host network and pid" \
    docker run --rm --network host --pid host -v "$MERGED:/w" alpine:3 cat /w/pristine-root.txt
report "the same open from the host, in a private mount namespace" \
    sudo unshare --mount sh -c "cat $MERGED/pristine-root.txt"
report "what the kernel said while that was happening" \
    sh -c "sudo dmesg | tail -15"

# Coherence: an edit to a file the cache does not hold, which invalidation does
# not cover because there is nothing to invalidate.
echo "changed underneath" >"$EXPORT_DIR/pkg/edited.txt"
sleep 2
report "container, a lower file changed underneath" docker exec "$HOLDER" cat /w/pkg/edited.txt

echo "appeared underneath" >"$EXPORT_DIR/pkg/new-below.txt"
sleep 2
report "container, a lower file created underneath" docker exec "$HOLDER" cat /w/pkg/new-below.txt

# If the cause is the lower missing from the container's mount namespace,
# binding it in beside the merged mount repairs it.
report "the container with the LOWER bound in beside the merged mount" \
    docker run --rm -v "$MERGED:/w" -v "$LOWER:/rd-lower:ro" alpine:3 cat /w/pristine-root.txt


echo "== 6b. the shape the design actually ships =="
# A volume of type overlay, where the DAEMON performs the mount: a different
# code path from binding a path this script mounted.
if docker volume create --name union-probe-vol --opt type=overlay --opt device=overlay \
    --opt "o=lowerdir=$LOWER,upperdir=$UPPER,workdir=$WORKDIR" >/dev/null 2>"$WORK/vol.err"; then
    ok "the local driver accepted a volume of type overlay"
    report "a container reading the LOWER through that volume" \
        docker run --rm -v union-probe-vol:/w alpine:3 cat /w/pristine-root.txt
    report "a container reading the CACHE through that volume" \
        docker run --rm -v union-probe-vol:/w alpine:3 cat /w/late.txt
    docker volume rm union-probe-vol >/dev/null 2>&1
else
    bad "the local driver refused a volume of type overlay: $(cat "$WORK/vol.err")"
fi


echo "== 6c. fuse-overlayfs, where the union lives in userspace =="
# With fuse-overlayfs the lower reads happen in the FUSE daemon's namespace
# rather than the caller's. Its cost is a userspace round trip per operation,
# cache hits included (section 6d).
if command -v fuse-overlayfs >/dev/null 2>&1 ||
    sudo apt-get install -y -qq fuse-overlayfs >/dev/null 2>&1; then
    mkdir -p "$WORK/fuse-merged"
    if sudo fuse-overlayfs -o "lowerdir=$LOWER,upperdir=$UPPER,workdir=$WORKDIR" \
        "$WORK/fuse-merged" 2>"$WORK/fuse.err"; then
        ok "fuse-overlayfs mounted over an NFS lower"
        report "the host reading the lower through it" \
            cat "$WORK/fuse-merged/pristine-root.txt"

        if outputs '^pristine at the root$' \
            docker run --rm -v "$WORK/fuse-merged:/w" alpine:3 cat /w/pristine-root.txt; then
            ok "a container reads the LOWER through the userspace union"
        else
            bad "THE DESIGN IS DEAD: no union is readable from a container: [$LAST_OUTPUT]"
        fi
        if outputs '^arrived through the union$' \
            docker run --rm -v "$WORK/fuse-merged:/w" alpine:3 cat /w/late.txt; then
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
# Sections 3 to 5 asked again of the union that ships: the overhead, the
# events, the shape write-back reads, and the daemon behind it dying.
FUSE_MERGED=$WORK/fuse-merged
mkdir -p "$FUSE_MERGED"
if sudo fuse-overlayfs -o "lowerdir=$LOWER,upperdir=$UPPER,workdir=$WORKDIR" "$FUSE_MERGED" 2>/dev/null; then
    for i in $(seq 1 200); do echo "cached body $i" | sudo tee "$FUSE_MERGED/c$i" >/dev/null; done
    fuse_read=$( { time -p sh -c "cat $FUSE_MERGED/c* >/dev/null"; } 2>&1 | awk '/^real/ {print $2}')
    kern_read=$( { time -p sh -c "cat $MERGED/c* >/dev/null"; } 2>&1 | awk '/^real/ {print $2}')
    direct_read=$( { time -p sh -c "cat $UPPER/c* >/dev/null"; } 2>&1 | awk '/^real/ {print $2}')
    info "200 cached files: fuse ${fuse_read}s, kernel overlay ${kern_read}s, straight off disk ${direct_read}s"
    # pristine-nested is only ever in the lower, so this read falls through.
    lower_via_fuse=$( { time -p sh -c "cat $FUSE_MERGED/pkg/pristine-nested.txt >/dev/null"; } 2>&1 | awk '/^real/ {print $2}')
    info "one file that misses and falls through to NFS: ${lower_via_fuse}s"

    # The floor for landing a batch: THROUGH the union, no network, against
    # straight to disk.
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

    # Landing a file whose name the LOWER has, in place (what the agent does)
    # against a temporary renamed over it (what it does not, ADR 0044). The
    # rows below record why: a file landed by rename comes back after an rm.
    # Recorded, not asserted. The lower is a local NFS server, so a link's
    # latency, where a copy-up's data read costs most, is absent from the timings.
    sudo mkdir -p "$LOWER/landing"
    for i in $(seq 1 200); do head -c 2048 /dev/zero | sudo tee "$LOWER/landing/f$i" >/dev/null; done
    sleep 1
    inplace_t=$( { time -p sudo sh -c "for i in \$(seq 1 100); do head -c 2048 /dev/zero >$FUSE_MERGED/landing/f\$i; done"; } 2>&1 | awk '/^real/ {print $2}')
    rename_t=$( { time -p sudo sh -c "for i in \$(seq 101 200); do head -c 2048 /dev/zero >$FUSE_MERGED/landing/.rd-fill-\$i && mv $FUSE_MERGED/landing/.rd-fill-\$i $FUSE_MERGED/landing/f\$i; done"; } 2>&1 | awk '/^real/ {print $2}')
    info "100 files the lower has, landed in place: ${inplace_t}s; landed by rename: ${rename_t}s"
    # Deleting what was landed. The lower keeps these files here, so a file that
    # is still present afterwards means the delete left no whiteout.
    sudo rm "$FUSE_MERGED/landing/f1" "$FUSE_MERGED/landing/f101"
    report "after rm of a file landed in place (f1): visible? and what the upper holds" \
        sudo sh -c "test -e $FUSE_MERGED/landing/f1 && echo VISIBLE || echo GONE; ls -l $UPPER/landing/f1 2>&1"
    report "after rm of a file landed by rename (f101): visible? and what the upper holds" \
        sudo sh -c "test -e $FUSE_MERGED/landing/f101 && echo VISIBLE || echo GONE; ls -l $UPPER/landing/f101 2>&1"

    # Ways to land that might keep the whiteout. Each ends in an rm, and a
    # VISIBLE answer is the lower showing through.
    M=$FUSE_MERGED/landing
    land() { sudo sh -c "printf new >$M/.rd-fill-x && mv $M/.rd-fill-x $M/$1"; }
    sudo chmod 666 "$M/f110"
    land f110
    sudo rm "$M/f110"
    land f111
    sudo stat "$M/f111" >/dev/null
    sudo rm "$M/f111"
    land f112
    sudo rm "$M/f112"
    sudo rm -f "$M/f112"
    sudo rm "$M/f113"
    land f113
    sudo rm "$M/f113"
    sudo cat "$M/f114" >/dev/null
    land f114
    sudo rm "$M/f114"
    for pair in "f110 copy-up first (chmod), then rename" "f111 rename, then stat, then rm" \
        "f112 rename, rm, rm again" "f113 rm first, rename, rm" "f114 read first, rename, rm"; do
        set -- $pair
        n=$1
        shift
        report "landing variant: $*" sudo sh -c "test -e $M/$n && echo VISIBLE || echo GONE"
    done

    if build_probe watchprobe "$WORK/watchprobe"; then
        sudo mkdir -p "$FUSE_MERGED/fusewatch"
        echo "before" | sudo tee "$FUSE_MERGED/fusewatch/reloaded.txt" >/dev/null
        if docker run -d --name union-probe-fusewatch -v "$FUSE_MERGED:/w" \
            -v "$WORK/watchprobe:/watchprobe" alpine:3 \
            /watchprobe -timeout 25s /w/fusewatch >/dev/null 2>&1; then
            wait_ready union-probe-fusewatch 15
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

    sudo rm -f "$FUSE_MERGED/pristine-root.txt"
    report "what a delete through the userspace union left in the upper" \
        sudo sh -c "ls -l $UPPER/pristine-root.txt 2>&1; getfattr -d -m - $UPPER/pristine-root.txt 2>&1 | head -5"

    if docker run -d --name union-probe-fusehold -v "$FUSE_MERGED:/w" alpine:3 sleep 60 >/dev/null 2>&1; then
        sudo pkill -f "fuse-overlayfs.*$FUSE_MERGED"
        sleep 2
        report "a container reading after the fuse daemon was killed" \
            docker exec union-probe-fusehold sh -c "cat /w/c1 2>&1; echo exit=\$?"
        docker rm -f union-probe-fusehold >/dev/null 2>&1
    fi
    sudo umount "$FUSE_MERGED" 2>/dev/null
else
    bad "fuse-overlayfs would not mount for the second round of questions"
fi

echo
echo "== 6e. can a per-account dind run a userspace union at all? =="
# With a daemon per account (ADR 0019) the union lives inside that dind, whose
# fallback image is stock docker:28-dind (daemons.DefaultImage).
if docker run --rm docker:28-dind sh -c "command -v fuse-overlayfs" >/dev/null 2>&1; then
    ok "the per-account dind image already carries fuse-overlayfs"
else
    info "docker:28-dind does NOT carry fuse-overlayfs; the agent would have to"
    info "supply it, or the per-account dind would have to be a different image"
fi
report "what the dind has under /dev/fuse" \
    docker run --rm --privileged docker:28-dind sh -c "ls -l /dev/fuse 2>&1"


echo "== 7. does the lower cost a d_type fallback? =="
# Without d_type from the lower's READDIR the kernel stats every entry, making
# a walk through the union SLOWER than the plain mount.
for i in $(seq 1 200); do echo x >"$EXPORT_DIR/pkg/f$i"; done
sleep 1
lower_walk=$( { time -p find "$LOWER" -type f >"$WORK/lower.list"; } 2>&1 | awk '/^real/ {print $2}')
merged_walk=$( { time -p find "$MERGED" -type f >"$WORK/merged.list"; } 2>&1 | awk '/^real/ {print $2}')
# The counts matter: a walk that found nothing is the fastest there is.
info "lower: $(wc -l <"$WORK/lower.list") files in ${lower_walk}s"
info "merged: $(wc -l <"$WORK/merged.list") files in ${merged_walk}s"
report "the same walk from inside the container" \
    docker exec "$HOLDER" sh -c "find /w -type f | wc -l"
report "what the kernel said about the lower" sh -c "sudo dmesg 2>/dev/null | grep -i 'overlayfs' | tail -5"

echo
echo "== 8. where the upper may live =="
# The kernel refuses an upper on overlayfs, so the cache is a volume on the
# daemon's data root and not a directory in the dind's own root.
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
# An agent restart, a dind restart, an idle disconnect (ADR 0015): the lower
# goes from under a running container. "It fails" and "it silently shows
# nothing" are very different answers.
sudo umount -l "$LOWER" 2>/dev/null
sleep 1
report "reading a lower-only file with the lower unmounted" \
    docker exec "$HOLDER" sh -c "cat /w/pkg/edited.txt 2>&1; echo exit=\$?"
report "reading a cached file with the lower unmounted" \
    docker exec "$HOLDER" sh -c "cat /w/late.txt 2>&1; echo exit=\$?"
docker rm -f "$HOLDER" >/dev/null 2>&1

echo
echo "== 10. mounting inside another daemon's namespace =="
# With a daemon per account (ADR 0019) the mounts happen inside that dind's
# mount namespace, which core-agent/union/serve_linux.go enters (with its pid
# and net ones).
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
# Sections 6c and 10 together: a union mounted INSIDE a per-account dind's
# namespace, read by a container that dind starts.
#
# On the HOST network because the export here listens on the host's loopback;
# the real reverse tunnel is bound inside the account's dind (ADR 0019).
if start_dind "$DIND2" "--network host"; then
    # Stock docker:28-dind lacks fuse-overlayfs; a real per-account daemon runs
    # the workspace image, which carries it (daemons.DefaultImage).
    if docker exec "$DIND2" apk add --no-cache fuse-overlayfs >/dev/null 2>&1; then
        pid2=$(docker inspect -f '{{.State.Pid}}' "$DIND2" 2>/dev/null)
        # The upper goes on the daemon's DATA ROOT, not /rd: a dind's own root
        # is overlayfs (section 8), and fuse-overlayfs then says "cannot read
        # upper dir".
        sudo nsenter -t "$pid2" -m -- mkdir -p /rd/lower /rd/merged \
            /var/lib/docker/rd-union/upper /var/lib/docker/rd-union/work
        if sudo nsenter -t "$pid2" -m -- mount -t nfs 127.0.0.1:"$EXPORT_DIR" /rd/lower \
            -o nfsvers=3,nolock,noacl,soft,timeo=30,retrans=2 2>"$WORK/dindnfs.err"; then
            ok "the lower mounts inside the dind"
            report "the upper, as the mount namespace sees it" \
                sudo nsenter -t "$pid2" -m -- sh -c "stat -f -c %T /var/lib/docker/rd-union/upper; ls -lad /var/lib/docker/rd-union /var/lib/docker/rd-union/upper /var/lib/docker/rd-union/work"
            report "the upper, as the dind itself sees it" \
                docker exec "$DIND2" sh -c "ls -lad /var/lib/docker/rd-union /var/lib/docker/rd-union/upper 2>&1"
            report "which fuse-overlayfs, and which version" \
                sudo nsenter -t "$pid2" -m -- sh -c "command -v fuse-overlayfs; fuse-overlayfs --version 2>&1 | head -3"
            report "ls and the mount, in one namespace entry" \
                sudo nsenter -t "$pid2" -m -- sh -c \
                "ls -la /var/lib/docker/rd-union/upper && echo ---- && fuse-overlayfs -o lowerdir=/rd/lower,upperdir=/var/lib/docker/rd-union/upper,workdir=/var/lib/docker/rd-union/work /rd/merged; echo exit=\$?"

            # Entering the MOUNT namespace alone leaves /proc/self resolving to
            # nothing (the dind's procfs, a host pid), and libfuse leans on
            # /proc/self/fd: fuse-overlayfs fails with ENOENT about a directory
            # that is there. Hence the agent's child also enters the pid one.
            report "what /proc/self is, entering the mount namespace only" \
                sudo nsenter -t "$pid2" -m -- sh -c "readlink /proc/self; ls /proc/self/fd 2>&1 | head -3"
            report "whether this nsenter can enter the pid namespace at all" \
                sudo nsenter -t "$pid2" -m -p --fork -- true

            report "the same mount, run by the dind itself" \
                docker exec "$DIND2" sh -c \
                "mkdir -p /rd2 && fuse-overlayfs -o lowerdir=/rd/lower,upperdir=/var/lib/docker/rd-union/upper,workdir=/var/lib/docker/rd-union/work /rd2 2>&1; echo exit=\$?"

            # Run by the dind itself: the position the agent's child is in
            # after entering the dind's namespaces.
            if docker exec "$DIND2" sh -c \
                "fuse-overlayfs -o lowerdir=/rd/lower,upperdir=/var/lib/docker/rd-union/upper,workdir=/var/lib/docker/rd-union/work /rd/merged" \
                2>"$WORK/dindfuse.err"; then
                ok "fuse-overlayfs mounts inside the dind"
                if outputs 'pristine and nested' docker exec "$DIND2" \
                    docker run --rm -v /rd/merged:/w alpine:3 cat /w/pkg/pristine-nested.txt; then
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
# union.Alive reads st_dev against the parent through /proc/<pid>/root, from
# OUTSIDE the daemon's namespace. A dead union leaves an empty directory that
# must not read as alive, and a live orphan after an agent restart must, so it
# is adopted rather than mounted over (ADR 0044).
if start_dind "$DIND3" "--network host"; then
    pid=$(docker inspect -f '{{.State.Pid}}' "$DIND3" 2>/dev/null)
    docker exec "$DIND3" sh -c 'mkdir -p /rd/probe12/lower /rd/probe12/merged' >/dev/null 2>&1

    # An ordinary directory first: what a dead union leaves behind.
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

        # tmpfs rather than a union: crossing a mount is a property of the
        # mount, not of its filesystem.
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


echo
echo "== 13. what fuse-overlayfs writes into the cache layer for a deletion =="
# Write-back reads the layer and has to tell a deletion and a replaced directory
# from a file the container made (ADR 0044). The kernel overlay records them as
# a character device 0:0 and an xattr; fuse-overlayfs is documented to use its
# own marks, and an agent that knows only the kernel's reports the others as the
# container's files. RECORDED here and not asserted: what the runner prints is
# the answer, and a change in it should be read rather than turn this red.
if command -v fuse-overlayfs >/dev/null 2>&1; then
    WH=$WORK/wh
    mkdir -p "$WH/lower/tools" "$WH/lower/dir" "$WH/upper" "$WH/work" "$WH/merged"
    echo old >"$WH/lower/tools/old"
    echo gone >"$WH/lower/dir/victim"
    echo kept >"$WH/lower/dir/kept"
    if sudo fuse-overlayfs -o "lowerdir=$WH/lower,upperdir=$WH/upper,workdir=$WH/work" \
        "$WH/merged" 2>"$WORK/wh.err"; then
        # A replaced directory and a deleted file, through the union.
        sudo sh -c "cd $WH/merged && rm -rf tools && mkdir tools && echo x >tools/new && rm dir/victim"
        sudo umount "$WH/merged" 2>/dev/null

        report "fuse-overlayfs version" sh -c "fuse-overlayfs --version 2>&1 | head -2"
        report "the layer, with file types and device numbers" \
            sudo find "$WH/upper" -printf '%y %m %p rdev=%D size=%s\n'
        report "character devices in the layer (the kernel's whiteout)" \
            sudo find "$WH/upper" -type c -ls
        report "names starting .wh. in the layer (fuse-overlayfs's own marker files)" \
            sudo find "$WH/upper" -name '.wh.*' -ls
        report "extended attributes in the layer" \
            sudo sh -c "command -v getfattr >/dev/null || apt-get install -y -qq attr >/dev/null 2>&1; getfattr -R -d -m - $WH/upper 2>&1"
        if [ -n "$(sudo find "$WH/upper" -name '.wh.*')" ]; then
            info "RECORDED: fuse-overlayfs writes .wh. marker files on this runner"
        else
            info "RECORDED: fuse-overlayfs writes no .wh. marker files on this runner (device or xattr instead)"
        fi
    else
        info "fuse-overlayfs would not mount for section 13: $(cat "$WORK/wh.err")"
    fi
    sudo umount "$WH/merged" 2>/dev/null
else
    info "fuse-overlayfs is not available; section 13 is unanswered"
fi

echo
echo "== 14. what write-back reads when it behaves like a bind mount =="
# Write-back decides a file the cache never held by its times (ADR 0044): a
# copy is not a change when size and mtime match this machine's, and otherwise
# the upper entry's ctime against this machine's says who wrote last. Each of
# those rests on something fuse-overlayfs does over an NFS lower, RECORDED here
# and not asserted: copy-up keeps the lower's mtime, a chown -R copies every
# file up, and a whiteout and an opaque marker keep the ctime of the deletion
# rather than of later writes beside them.
if command -v fuse-overlayfs >/dev/null 2>&1; then
    PB=$WORK/parity
    mkdir -p "$EXPORT_DIR/parity/tree/sub" "$EXPORT_DIR/parity/repl" "$PB/upper" "$PB/work" "$PB/merged"
    echo kept >"$EXPORT_DIR/parity/old.txt"
    touch -d '2020-01-02 03:04:05.123456789' "$EXPORT_DIR/parity/old.txt"
    echo victim >"$EXPORT_DIR/parity/victim.txt"
    printf '#!/bin/sh\n' >"$EXPORT_DIR/parity/script.sh"
    chmod 0644 "$EXPORT_DIR/parity/script.sh"
    for f in a b c; do
        echo "$f" >"$EXPORT_DIR/parity/tree/$f"
        echo "$f" >"$EXPORT_DIR/parity/tree/sub/$f"
    done
    echo old >"$EXPORT_DIR/parity/repl/old"
    # A lower of its own: section 9 unmounted the shared one. Mounted as
    # section 1 mounts it, so copy-up reads through NFS as a share does.
    PLOWER=$WORK/parity-lower
    mkdir -p "$PLOWER"
    if ! sudo mount -t nfs 127.0.0.1:"$EXPORT_DIR" "$PLOWER" \
        -o nfsvers=3,nolock,noacl,soft,timeo=30,retrans=2,actimeo=1,noatime 2>"$WORK/parity-nfs.err"; then
        info "could not mount the export again for section 14: $(cat "$WORK/parity-nfs.err")"
    fi
    LOWER=$PLOWER
    report "the lower as fuse-overlayfs will see it" ls -la "$LOWER/parity"
    if sudo fuse-overlayfs -o "lowerdir=$LOWER/parity,upperdir=$PB/upper,workdir=$PB/work" \
        "$PB/merged" 2>"$WORK/parity.err"; then
        sudo chown 1000:1000 "$PB/merged/old.txt"
        report "copy-up by chown: mtime, mode and ctime, lower then upper" \
            sudo stat -c '%n mtime=%y mode=%a ctime=%z' "$LOWER/parity/old.txt" "$PB/upper/old.txt"
        if [ "$(sudo stat -c %y "$LOWER/parity/old.txt")" = "$(sudo stat -c %y "$PB/upper/old.txt" 2>/dev/null)" ]; then
            info "RECORDED: copy-up keeps the lower's mtime to the nanosecond"
        else
            info "RECORDED: copy-up does NOT keep the lower's mtime exactly"
        fi

        sudo chown -R 1000:1000 "$PB/merged/tree"
        report "the upper after chown -R of tree" sudo find "$PB/upper/tree" -printf '%y %m %p\n'
        info "RECORDED: chown -R copied up $(sudo find "$PB/upper/tree" -type f | wc -l) of $(find "$EXPORT_DIR/parity/tree" -type f | wc -l) files"

        sudo chmod +x "$PB/merged/script.sh"
        report "chmod +x copies up with the new mode, lower then upper" \
            sudo stat -c '%n mode=%a mtime=%y' "$LOWER/parity/script.sh" "$PB/upper/script.sh"

        sudo mkdir "$PB/merged/emptydir"
        report "a directory made through the union" sudo find "$PB/upper/emptydir" -printf '%y %m %p\n'
        report "its extended attributes (opaque or not)" \
            sudo sh -c "command -v getfattr >/dev/null || apt-get install -y -qq attr >/dev/null 2>&1; getfattr -d -m - $PB/upper/emptydir 2>&1"

        # The deletion and the replace first, then writes beside them later:
        # the times write-back reads must be the first ones.
        sudo rm "$PB/merged/victim.txt"
        sudo sh -c "rm -rf $PB/merged/repl && mkdir $PB/merged/repl"
        sleep 2
        sudo sh -c "echo later >$PB/merged/repl/new && echo later >$PB/merged/written-later.txt"
        report "the whiteout, the opaque directory and its markers, and a later write" \
            sudo sh -c "cd $PB/upper && stat -c '%n %F ctime=%z' .wh.victim.txt victim.txt repl repl/.wh..wh..opq repl/.wh..opq repl/new written-later.txt 2>&1"
        marker=$(sudo sh -c "stat -c %Z $PB/upper/repl/.wh..wh..opq $PB/upper/repl/.wh..opq 2>/dev/null | sort -n | head -1")
        later=$(sudo stat -c %Z "$PB/upper/repl/new" 2>/dev/null)
        if [ -n "$marker" ] && [ -n "$later" ] && [ "$marker" -lt "$later" ]; then
            info "RECORDED: an opaque marker keeps the ctime of the replace ($marker), before a later write in it ($later)"
        else
            info "RECORDED: no opaque marker ctime earlier than a later write (marker [$marker], later [$later])"
        fi
        sudo umount "$PB/merged" 2>/dev/null
    else
        info "fuse-overlayfs would not mount for section 14: $(cat "$WORK/parity.err")"
    fi
else
    info "fuse-overlayfs is not available; section 14 is unanswered"
fi

echo "The answers above decide the design, not the pass count: section 4 says"
echo "whether a mounted cache can be filled at all, and section 5 says whether"
echo "hot reload survives it."
summary
