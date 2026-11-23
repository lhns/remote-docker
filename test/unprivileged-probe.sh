#!/usr/bin/env bash
# Can the workspace run on a cluster that refuses privileged pods?
#
# A measurement, not a gate. It RECORDS what each environment answers and never
# exits non-zero because a row failed: a refusal is the data. Only a setup
# error (a missing tool, an image that will not build) is fatal.
#
# Today's workspace is a privileged pod running dockerd, a nested privileged
# dind per account, kernel NFS mounts of the shares through the docker local
# volume driver, fuse-overlayfs unions, and setns into the daemons' namespaces.
# The suspected blocker is that the kernel refuses an NFS mount from any
# non-initial user namespace (nfs has no FS_USERNS_MOUNT); the alternative is a
# userspace client over /dev/fuse. Each question below is one row:
#
#   question                                                        row
#   does the pod run at all (admission, scheduling)                 0
#   can a user namespace be created; the sysctls that gate it       1
#   can a uid map hold more than one uid (newuidmap, subuid)        2
#   does a kernel NFS mount work, and from a user namespace         3a 3b 3c
#   what does a root-made NFS mount in a private netns look like
#     to a rootless container (runner only)                         4
#   is /dev/fuse there and usable; fuse-overlayfs in a userns       5a 5b 5c
#   which engine starts: rootless dockerd, podman, stock dockerd    6a 6b 6c 6d
#   do images work (storage driver, pull, run)                      7
#   can a mount made in the daemon's namespace reach a container    8
#   does an inotify poke on a FUSE mount reach a container (11d)    9
#   what does fuse-overlayfs write without trusted.* (union 13)     10
#   where does a published port land; can a container reach the pod 11a 11b 11c
#   are cgroup v2 controllers delegated; do --memory/--cpus work    12a 12b
#
# Where it runs (modes):
#   runner   the bare GitHub runner: as root, then as an unprivileged user, then
#            as that user again with kernel.apparmor_restrict_unprivileged_userns
#            off, labelled as such and never flipped silently
#   kind     one pod per Pod Security tier on a kind cluster, each again with
#            hostUsers: false, and a privileged control pod on the same node
#   report   the markdown table from what the two recorded
#   all      runner, kind, report
#   render   print one pod manifest, for kubeconform
#   rows     the rows themselves, in whatever environment this runs in
#
# Tiers in the kind cluster (manifests in test/unprivileged-probe/):
#   perm-nonroot, perm-root  namespace enforcing `privileged`, pod not privileged:
#                            Unconfined seccomp and AppArmor, host /dev/fuse,
#                            uid 1000 / root with SYS_ADMIN
#   baseline                 `baseline` level, root, default seccomp
#   restricted               `restricted` level, non-root, no_new_privs
#   <tier>-uns               the same with hostUsers: false (the permissive ones
#                            also ask for procMount: Unmasked, which needs it)
#   control                  privileged, the workspace as it is today
# A tier whose pod the cluster refuses is itself an answer (row 0).
#
# Environment: PROBE_OUT (results directory), KIND_CLUSTER (default probe).
#
# $DOCKER, $ENG, $NS_PRE and $NS_ARGS are command prefixes split on purpose; the
# redirects after sudo are the runner's own log files; the sed indent reads a
# variable. None of that is a mistake here.
# shellcheck disable=SC2086,SC2024,SC2001,SC2181,SC1091,SC2016
set -uo pipefail

SELF=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")
HERE=$(dirname "$SELF")
TIER=${PROBE_TIER:-local}
NFS_OPTS="vers=3,proto=tcp,port=2049,mountport=2049,nolock"

# ---- recording --------------------------------------------------------------

# One-line, pipe-free, bounded: results are parsed back from the log.
clean() { tr '\n\r|\t' '    ' | sed 's/  */ /g' | cut -c1-300; }
firstline() { awk 'NF{print; exit}' <<<"$1"; }
emit() {
    local d=$3
    case "$1" in 4 | 7 | 8 | 9 | 11* | 12b) [ -n "${ENGINE:-}" ] && d="[engine: $ENGINE${RUN_NET:+ $RUN_NET}] $d" ;; esac
    printf 'RESULT|%s|%s|%s|%s\n' "$TIER" "$1" "$2" "$(clean <<<"$d")"
}
fact() { printf 'FACT|%s|%s|%s\n' "${3:-$TIER}" "$1" "$(clean <<<"$2")"; }
say() { echo "  ....  $*"; }

# attempt <row> <seconds> <cmd...>: ok on exit 0, else refused with the first
# line of what it said. The whole output goes to the log either way.
attempt() {
    local row=$1 t=$2 out rc
    shift 2
    out=$(timeout "$t" "$@" 2>&1)
    rc=$?
    echo "---- row $row: $* (exit $rc)"
    sed 's/^/        /' <<<"$out"
    if [ "$rc" -eq 0 ]; then
        emit "$row" ok "$(firstline "$out")"
    else
        emit "$row" refused "$(firstline "$out") [exit $rc]"
    fi
    return "$rc"
}

# firsterr <file>: the first line that looks like the reason, else the first line.
firsterr() {
    grep -m1 -iE 'error|fail|must|cannot|permission|denied|not permitted|refus|unable|invalid' "$1" 2>/dev/null ||
        awk 'NF{print; exit}' "$1" 2>/dev/null
}

# ---- the rows ---------------------------------------------------------------

ENGINE_OK=0
USERNS_OK=0
ENGMODE=self
ENG_UID=1000
NS_PRE=""
NS_ARGS=""
NS_PID=""

context() {
    echo "== context: $TIER =="
    fact id "$(id)"
    fact kernel "$(uname -r)"
    fact uid_map "$(tr -s ' ' </proc/self/uid_map | tr '\n' ';')"
    if grep -qE '^ *0 +0 +4294967295' /proc/self/uid_map; then
        fact userns "none: this is the host user namespace (hostUsers false not honoured, or not asked for)"
    else
        fact userns "own user namespace (hostUsers: false honoured, or the runner's)"
    fi
    fact CapEff "$(awk '/^CapEff/{print $2}' /proc/self/status)"
    fact CapBnd "$(awk '/^CapBnd/{print $2}' /proc/self/status)"
    fact NoNewPrivs "$(awk '/^NoNewPrivs/{print $2}' /proc/self/status)"
    fact Seccomp "$(awk '/^Seccomp:/{print $2}' /proc/self/status) (0 none, 2 filter)"
    fact apparmor "$(cat /proc/self/attr/current 2>&1)"
    fact dev_fuse "$(ls -l /dev/fuse 2>&1)"
    for t in nfs fuse overlay; do
        fact "fs_$t" "$(grep -cw "$t" /proc/filesystems)"
    done
    emit 0 ok "ran as $(id -u):$(id -g), $(firstline "$(tr -s ' ' </proc/self/uid_map | tr '\n' ';')")"
}

row1() {
    echo "== 1. user namespace creation =="
    local k v
    for k in kernel.apparmor_restrict_unprivileged_userns user.max_user_namespaces \
        kernel.unprivileged_userns_clone kernel.unprivileged_userns_apparmor_policy; do
        v=$(cat "/proc/sys/${k//.//}" 2>/dev/null) || v=absent
        fact "sysctl $k" "${v:-absent}"
    done
    if attempt 1 20 unshare -Ur true; then USERNS_OK=1; fi
}

# subrange <file>: "start count" of this user's first range, by name or uid.
subrange() {
    awk -F: -v n="$(id -un 2>/dev/null)" -v u="$(id -u)" '$1==n||$1==u{print $2" "$3; exit}' "$1" 2>/dev/null
}

row2() {
    echo "== 2. a uid map of more than one uid =="
    if [ "$USERNS_OK" -ne 1 ]; then
        emit 2 notrun "no user namespace (row 1)"
        return
    fi
    local uid gid sub gsub p out out2 map
    uid=$(id -u)
    gid=$(id -g)
    if [ "$uid" -eq 0 ]; then
        # Root has no subuid range by default; the file is the image's own.
        grep -q '^root:' /etc/subuid 2>/dev/null || echo 'root:100000:65536' >>/etc/subuid 2>/dev/null
        grep -q '^root:' /etc/subgid 2>/dev/null || echo 'root:100000:65536' >>/etc/subgid 2>/dev/null
    fi
    sub=$(subrange /etc/subuid)
    gsub=$(subrange /etc/subgid)
    say "NoNewPrivs=$(awk '/^NoNewPrivs/{print $2}' /proc/self/status) subuid=[$sub] subgid=[$gsub]"
    if [ -z "$sub" ] || [ -z "$gsub" ]; then
        emit 2 refused "no subuid/subgid range for this user [NoNewPrivs=$(awk '/^NoNewPrivs/{print $2}' /proc/self/status)]"
        return
    fi
    if ! command -v newuidmap >/dev/null 2>&1; then
        emit 2 refused "newuidmap is not installed"
        return
    fi
    unshare -U sleep 30 &
    p=$!
    sleep 0.5
    # shellcheck disable=SC2086  # sub is "start count"
    out=$(timeout 10 newuidmap "$p" 0 "$uid" 1 1 $sub 2>&1)
    local rc1=$?
    # shellcheck disable=SC2086
    out2=$(timeout 10 newgidmap "$p" 0 "$gid" 1 1 $gsub 2>&1)
    local rc2=$?
    map=$(tr -s ' ' </proc/$p/uid_map 2>&1 | tr '\n' ';')
    kill "$p" 2>/dev/null
    wait "$p" 2>/dev/null
    echo "        newuidmap rc=$rc1: $out"
    echo "        newgidmap rc=$rc2: $out2"
    echo "        uid_map: $map"
    local nnp
    nnp=$(awk '/^NoNewPrivs/{print $2}' /proc/self/status)
    if [ "$rc1" -eq 0 ] && [ "$rc2" -eq 0 ]; then
        emit 2 ok "uid_map $map [NoNewPrivs=$nnp]"
    else
        emit 2 refused "$(firstline "$out$out2") [newuidmap=$rc1 newgidmap=$rc2 NoNewPrivs=$nnp]"
    fi
}

NFS_PID=""
nfs_server() {
    mkdir -p "$W/mnt"
    if [ -n "${PROBE_NFS_EXPORT:-}" ]; then
        # A server the driver started (the pod image's unfsd, in a container).
        NFS_EXPORT=$PROBE_NFS_EXPORT
        return 0
    fi
    NFS_EXPORT=$W/export
    mkdir -p "$W/export"
    echo "from the server" >"$W/export/hello"
    echo "$W/export 127.0.0.1(rw,no_root_squash,insecure)" >"$W/exports"
    if ! command -v unfsd >/dev/null 2>&1; then
        NFS_ERR="unfsd (unfs3) is not installed"
        return 1
    fi
    unfsd -u -d -p -n 2049 -m 2049 -e "$W/exports" >"$W/unfsd.log" 2>&1 &
    NFS_PID=$!
    local _
    for _ in $(seq 1 20); do
        (exec 3<>/dev/tcp/127.0.0.1/2049) 2>/dev/null && return 0
        kill -0 "$NFS_PID" 2>/dev/null || break
        sleep 0.5
    done
    NFS_ERR="unfsd did not listen on 2049: $(firsterr "$W/unfsd.log")"
    unfsd -h >>"$W/unfsd.log" 2>&1
    sed 's/^/        /' "$W/unfsd.log"
    return 1
}

row3() {
    echo "== 3. kernel NFS mounts =="
    attempt 3c 5 grep -w nfs /proc/filesystems
    if ! nfs_server; then
        for r in 3a 3b; do emit "$r" notrun "no NFS server: $NFS_ERR"; done
        return
    fi
    attempt 3a 30 sh -c "mount -t nfs -o $NFS_OPTS 127.0.0.1:$NFS_EXPORT $W/mnt && cat $W/mnt/hello"
    umount "$W/mnt" 2>/dev/null
    if [ "$USERNS_OK" -ne 1 ]; then
        emit 3b notrun "no user namespace (row 1)"
        return
    fi
    attempt 3b 30 unshare -Urm sh -c "mount -t nfs -o $NFS_OPTS 127.0.0.1:$NFS_EXPORT $W/mnt && cat $W/mnt/hello"
}

# The script both fuse rows run: build a union, make a replaced directory and a
# deleted file through it, and print what the layer looks like (union-probe.sh
# section 13, without the trusted.* xattrs a user namespace cannot write).
write_fuse_script() {
    cat >"$W/fuse_union.sh" <<'EOS'
B=$1
mkdir -p "$B/lower/tools" "$B/lower/dir" "$B/upper" "$B/work" "$B/merged"
echo old >"$B/lower/tools/old"
echo gone >"$B/lower/dir/victim"
echo kept >"$B/lower/dir/kept"
fuse-overlayfs -o "lowerdir=$B/lower,upperdir=$B/upper,workdir=$B/work" "$B/merged" || { echo "MOUNT-FAILED"; exit 3; }
fst=$(awk -v m="$B/merged" '$5==m{for(i=6;i<=NF;i++) if($i=="-"){print $(i+1); exit}}' /proc/self/mountinfo)
echo "FSTYPE=$fst"
(cd "$B/merged" && rm -rf tools && mkdir tools && echo x >tools/new && rm dir/victim && echo hi >created) || echo "WRITE-FAILED"
echo "--- layer"
find "$B/upper" -printf '%y %m %p\n'
echo "--- names starting .wh."
find "$B/upper" -name '.wh.*'
echo "--- character devices"
find "$B/upper" -type c
echo "--- xattrs"
getfattr -R -d -m - "$B/upper" 2>&1
fusermount3 -u "$B/merged" 2>/dev/null || umount "$B/merged" 2>/dev/null
exit 0
EOS
}

fuse_summary() { # <output>
    local o=$1 wh cd xa
    wh=$(awk '/^--- names starting/{f=1;next}/^---/{f=0}f&&NF' <<<"$o" | wc -l)
    cd=$(awk '/^--- character devices/{f=1;next}/^---/{f=0}f&&NF' <<<"$o" | wc -l)
    xa=$(awk '/^--- xattrs/{f=1;next}/^---/{f=0}f&&/=/' <<<"$o" | cut -d= -f1 | sort -u | tr '\n' ',')
    echo "$(grep -m1 '^FSTYPE=' <<<"$o"); .wh. names: $wh; char devices: $cd; xattrs: ${xa:-none}"
}

row5() {
    echo "== 5. FUSE =="
    attempt 5a 5 bash -c 'exec 3</dev/fuse && echo "opened /dev/fuse: $(ls -l /dev/fuse)"'
    if ! command -v fuse-overlayfs >/dev/null 2>&1; then
        for r in 5b 5c 10; do emit "$r" notrun "fuse-overlayfs is not installed"; done
        return
    fi
    write_fuse_script
    local out rc plain_sum
    out=$(timeout 60 bash "$W/fuse_union.sh" "$W/f5c" 2>&1)
    rc=$?
    echo "---- row 5c: fuse-overlayfs, no user namespace (exit $rc)"
    sed 's/^/        /' <<<"$out"
    if [ "$rc" -eq 0 ] && grep -q '^FSTYPE=fuse' <<<"$out"; then
        emit 5c ok "$(fuse_summary "$out")"
        plain_sum=$(fuse_summary "$out")
    else
        emit 5c refused "$(firstline "$(grep -v '^FSTYPE=$' <<<"$out")") [exit $rc]"
        plain_sum=""
    fi
    if [ "$USERNS_OK" -ne 1 ]; then
        emit 5b notrun "no user namespace (row 1)"
        emit 10 notrun "no user namespace (row 1)${plain_sum:+; without one: $plain_sum}"
        return
    fi
    out=$(timeout 60 unshare -Urm bash "$W/fuse_union.sh" "$W/f5b" 2>&1)
    rc=$?
    echo "---- row 5b: fuse-overlayfs under unshare -Urm (exit $rc)"
    sed 's/^/        /' <<<"$out"
    if [ "$rc" -eq 0 ] && grep -q '^FSTYPE=fuse' <<<"$out"; then
        emit 5b ok "$(fuse_summary "$out")"
        emit 10 ok "RECORDED in a user namespace: $(fuse_summary "$out")"
    else
        emit 5b refused "$(firstline "$(grep -v '^FSTYPE=$' <<<"$out")") [exit $rc]"
        emit 10 notrun "fuse-overlayfs would not mount in a user namespace (row 5b)${plain_sum:+; without one: $plain_sum}"
    fi
}

# ---- engines ----------------------------------------------------------------

IMG=docker.io/library/alpine:3
ENGINE=""
RUN_NET=""

make_engine_wrapper() {
    mkdir -p "$W/home" "$W/run" "$W/engine"
    chmod 700 "$W/run"
    local envs="HOME=$W/home XDG_RUNTIME_DIR=$W/run XDG_CONFIG_HOME=$W/home/.config XDG_DATA_HOME=$W/home/.local/share"
    if [ "$ENGMODE" = setpriv ]; then
        chown -R "$ENG_UID:$ENG_UID" "$W/home" "$W/run" "$W/engine" 2>/dev/null
        printf '#!/bin/sh\nexec setpriv --reuid=%s --regid=%s --clear-groups env %s "$@"\n' \
            "$ENG_UID" "$ENG_UID" "$envs" >"$W/eng"
    else
        printf '#!/bin/sh\nexec env %s "$@"\n' "$envs" >"$W/eng"
    fi
    chmod 755 "$W/eng"
    ENG=$W/eng
}

row6() {
    echo "== 6. which engine starts =="
    # 6c: the stock rootful dockerd, as docker:dind runs it (without the
    # entrypoint, whose default is a TCP listener nobody here wants).
    mkdir -p "$W/stock"
    timeout 60 dockerd --host="unix://$W/stock/docker.sock" --data-root="$W/stock/data" \
        --exec-root="$W/stock/exec" --pidfile="$W/stock/pid" >"$W/stock.log" 2>&1 &
    local spid=$! ok=0
    for _ in $(seq 1 40); do
        if timeout 5 docker -H "unix://$W/stock/docker.sock" info >/dev/null 2>&1; then ok=1; break; fi
        kill -0 "$spid" 2>/dev/null || break
        sleep 1
    done
    echo "---- row 6c: stock dockerd (log tail)"
    tail -n 12 "$W/stock.log" | sed 's/^/        /'
    if [ "$ok" -eq 1 ]; then
        emit 6c ok "$(timeout 5 docker -H "unix://$W/stock/docker.sock" info --format 'driver={{.Driver}}' 2>&1)"
    else
        emit 6c refused "$(firsterr "$W/stock.log") [dockerd exit: $(kill -0 "$spid" 2>/dev/null && echo "still running, not ready" || echo "exited")]"
    fi
    kill "$spid" 2>/dev/null
    pkill -f "$W/stock" 2>/dev/null
    wait "$spid" 2>/dev/null

    if [ "$ENGMODE" = none ]; then
        for r in 6a 6b 6d; do emit "$r" notrun "this tier runs the kernel rows only"; done
        return
    fi
    make_engine_wrapper

    # 6a: rootless dockerd, the way docker:dind-rootless starts it. The image
    # carries rootlesskit and an entrypoint but no dockerd-rootless.sh, so the
    # candidates are tried in turn and each one's first error is kept.
    local drl rk ep cand dpid dsock clog errs=""
    drl=$(command -v dockerd-rootless.sh || find /usr /opt /bin -name 'dockerd-rootless.sh' 2>/dev/null | head -n1)
    rk=$(command -v rootlesskit || true)
    ep=$(command -v dockerd-entrypoint.sh || true)
    say "dockerd-rootless.sh: [${drl:-not found}] rootlesskit: [${rk:-not found}] entrypoint: [${ep:-not found}] dockerd: [$(command -v dockerd || true)]"
    say "running as uid $(id -u), engine uid $ENG_UID ($ENGMODE), NoNewPrivs=$(awk '/^NoNewPrivs/{print $2}' /proc/self/status)"
    say "sysctls: unprivileged_userns_clone=$(cat /proc/sys/kernel/unprivileged_userns_clone 2>/dev/null || echo absent) apparmor_restrict_unprivileged_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo absent) max_user_namespaces=$(cat /proc/sys/user/max_user_namespaces 2>/dev/null || echo absent)"
    if [ -n "$ep" ]; then
        echo "---- $ep (the image's own start-up)"
        sed 's/^/        /' "$ep"
    fi
    dsock="$ENG docker -H unix://$W/run/docker.sock"
    for cand in entrypoint script rootlesskit; do
        clog=$W/dockerd-rootless-$cand.log
        case "$cand" in
            entrypoint)
                [ -n "$ep" ] || continue
                set -- "$ENG" env DOCKER_TLS_CERTDIR= "$ep" dockerd --host="unix://$W/run/docker.sock" --data-root="$W/home/docker"
                ;;
            script)
                [ -n "$drl" ] || continue
                set -- "$ENG" "$drl" --host="unix://$W/run/docker.sock" --data-root="$W/home/docker"
                ;;
            rootlesskit)
                [ -n "$rk" ] || continue
                set -- "$ENG" "$rk" --net=slirp4netns --mtu=65520 --disable-host-loopback --port-driver=builtin \
                    --copy-up=/etc --copy-up=/run dockerd --host="unix://$W/run/docker.sock" --data-root="$W/home/docker"
                ;;
        esac
        "$@" >"$clog" 2>&1 &
        dpid=$!
        for _ in $(seq 1 60); do
            if timeout 5 $dsock info >/dev/null 2>&1; then ENGINE_OK=1; break; fi
            kill -0 "$dpid" 2>/dev/null || break
            sleep 1
        done
        echo "---- row 6a via $cand (log tail)"
        tail -n 25 "$clog" | sed 's/^/        /'
        [ "$ENGINE_OK" -eq 1 ] && break
        errs="$errs [$cand: $(firsterr "$clog")]"
        pkill -f "$W/run/docker.sock" 2>/dev/null
        wait "$dpid" 2>/dev/null
    done
    if [ "$ENGINE_OK" -eq 1 ]; then
        ENGINE=docker-rootless
        DOCKER=$dsock
        emit 6a ok "via $cand: $(timeout 10 $DOCKER info --format 'driver={{.Driver}} cgroup={{.CgroupVersion}}/{{.CgroupDriver}} security={{.SecurityOptions}}' 2>&1)"
    else
        emit 6a refused "${errs:-nothing to start: no entrypoint, script or rootlesskit} [uid $(id -u) engine uid $ENG_UID NoNewPrivs=$(awk '/^NoNewPrivs/{print $2}' /proc/self/status) apparmor_restrict=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo absent)]"
    fi

    # 6b and 6d: rootless podman, its API service and a container. pasta, the
    # default network, may be refused, so the run is retried with slirp4netns
    # and then no network, and the first that works is used by later rows.
    $ENG podman system service --time=0 "unix://$W/run/podman.sock" >"$W/podman.log" 2>&1 &
    local ppid=$!
    for _ in $(seq 1 30); do
        [ -S "$W/run/podman.sock" ] && break
        kill -0 "$ppid" 2>/dev/null || break
        sleep 1
    done
    echo "---- row 6b: podman system service (log)"
    sed 's/^/        /' "$W/podman.log"
    local svc=0
    if [ -S "$W/run/podman.sock" ]; then
        svc=1
        emit 6b ok "service listening on $W/run/podman.sock"
    else
        emit 6b refused "$(firsterr "$W/podman.log")"
    fi
    local net out seq="" worked=""
    for net in "" "--network=slirp4netns" "--network=none"; do
        out=$(timeout 180 $ENG podman run $net --rm $IMG true 2>&1)
        local rc=$?
        echo "---- row 6d: podman run ${net:-(default network)} (exit $rc)"
        sed 's/^/        /' <<<"$out"
        if [ "$rc" -eq 0 ]; then
            seq="$seq [${net:-default}: ok]"
            worked=${worked:-$net}
            [ -n "$worked" ] || worked=default
            break
        fi
        seq="$seq [${net:-default}: $(grep -m1 -iE 'error|fail|denied|refus|unable' <<<"$out" || firstline "$out")]"
    done
    if [ -n "$worked" ]; then
        emit 6d ok "$seq"
        [ "$worked" = default ] || RUN_NET=$worked
    else
        emit 6d refused "$seq"
    fi
    if [ "$ENGINE_OK" -ne 1 ] && [ "$svc" -eq 1 ]; then
        ENGINE_OK=1
        ENGINE=podman
        DOCKER="$ENG podman"
        say "no rootless dockerd: rows 7 to 12 use rootless podman${RUN_NET:+ with $RUN_NET}"
    fi
}

need_engine() { # <row...>: returns 1 after recording, if no engine started
    [ "$ENGINE_OK" -eq 1 ] && return 0
    local r
    for r in "$@"; do emit "$r" notrun "no engine (rows 6a, 6b)"; done
    return 1
}

row7() {
    echo "== 7. image storage =="
    need_engine 7 || return
    if [ "$ENGINE" = podman ]; then
        say "$(timeout 10 $DOCKER info --format 'storage driver: {{.Store.GraphDriverName}}' 2>&1)"
    else
        say "$(timeout 10 $DOCKER info --format 'storage driver: {{.Driver}} {{.DriverStatus}}' 2>&1)"
    fi
    attempt 7 240 sh -c "$DOCKER pull $IMG && $DOCKER run $RUN_NET --rm $IMG echo ran-in-container"
}

# inns <shell command>: run where the container runtime's mounts live: the
# rootless daemon's mount namespace (row 8), or for podman the engine user's own.
inns() {
    if [ "$ENGINE" = podman ]; then
        $ENG sh -c "$1"
    else
        $NS_PRE nsenter -t "$NS_PID" $NS_ARGS sh -c "$1"
    fi
}

row8() {
    echo "== 8. enter the daemon's mount namespace =="
    need_engine 8 || return
    if [ "$ENGINE" = podman ]; then
        emit 8 notrun "the engine is rootless podman, which has no daemon namespace to enter"
        return
    fi
    NS_PID=$(cat "$W/run/docker.pid" 2>/dev/null)
    [ -n "$NS_PID" ] && [ -d "/proc/$NS_PID" ] || NS_PID=$($ENG pgrep -x dockerd | head -n1)
    say "daemon pid $NS_PID"
    if [ -z "$NS_PID" ]; then
        emit 8 refused "cannot find the rootless dockerd pid"
        return
    fi
    local v who args pre out seen=""
    local variants=("eng|-U --preserve-credentials -m" "eng|-U -m" "eng|-m")
    if [ "$ENGMODE" = setpriv ]; then
        variants=("self|-m" "self|-U -m" "${variants[@]}")
    fi
    for v in "${variants[@]}"; do
        who=${v%%|*}
        args=${v#*|}
        pre=""
        [ "$who" = eng ] && pre=$ENG
        out=$(timeout 30 $pre nsenter -t "$NS_PID" $args sh -c \
            "mkdir -p $W/nsmount && mount -t tmpfs tmpfs $W/nsmount && echo hello >$W/nsmount/marker" 2>&1)
        local rc=$?
        echo "---- row 8 variant [$who nsenter $args] exit $rc: $out"
        if [ "$rc" -ne 0 ]; then
            seen="$seen [$who nsenter $args: $(firstline "$out")]"
            continue
        fi
        NS_PRE=$pre
        NS_ARGS=$args
        out=$(timeout 60 $DOCKER run $RUN_NET --rm -v "$W/nsmount:/w" $IMG ls /w 2>&1)
        echo "        docker run -v: $out"
        if grep -qw marker <<<"$out"; then
            emit 8 ok "[$who nsenter $args] made a mount, a container bound from it saw: $(clean <<<"$out")"
        else
            emit 8 refused "entered with [$who nsenter $args] but the container did not see the mount: $(firstline "$out")"
        fi
        return
    done
    NS_PID=""
    emit 8 refused "no way into the namespace:$seen"
}

# watch_matrix <container> <dir> <direct|inns>: a watcher in a container on
# <dir>, poked from outside; MATRIX gets the masks seen per primitive and WLOG
# the container's log. The files poke-<primitive>.txt must exist in <dir>.
watch_matrix() {
    local c=$1 dir=$2 how=$3 p out seen
    MATRIX=""
    WLOG=""
    if ! timeout 60 $DOCKER run $RUN_NET -d --name "$c" -v "$dir:/data" \
        -v /usr/local/bin/watchprobe:/probe/watchprobe:ro \
        $IMG /probe/watchprobe -timeout 40s /data >"$W/$c.out" 2>&1; then
        MATRIX="the watch container would not start: $(firstline "$(cat "$W/$c.out")")"
        return 1
    fi
    for _ in $(seq 1 30); do
        $DOCKER logs "$c" 2>&1 | grep -q '^READY' && break
        sleep 1
    done
    for p in openclose mtime touch; do
        if [ "$how" = inns ]; then
            out=$(inns "/usr/local/bin/pokeprobe $p $dir/poke-$p.txt" 2>&1)
        else
            out=$($ENG /usr/local/bin/pokeprobe "$p" "$dir/poke-$p.txt" 2>&1)
        fi
        say "pokeprobe $p ($how): $out"
    done
    timeout 90 $DOCKER wait "$c" >/dev/null 2>&1
    WLOG=$($DOCKER logs "$c" 2>&1)
    $DOCKER rm -f "$c" >/dev/null 2>&1
    echo "$WLOG" | grep -v 'IN_ISDIR data/$' | sed 's/^/        /'
    for p in openclose mtime touch; do
        seen=$(grep "^INOTIFY .* poke-$p\.txt$" <<<"$WLOG" | awk '{print $2}' | sort -u | tr '\n' ',' | sed 's/,$//')
        MATRIX="$MATRIX $p=${seen:-<nothing>}"
    done
    return 0
}

row9() {
    echo "== 9. inotify on a FUSE mount bound into a container =="
    need_engine 9 || return
    if [ "$ENGINE" != podman ] && [ -z "$NS_PID" ]; then
        emit 9 notrun "could not enter the daemon's namespace (row 8)"
        return
    fi
    if ! command -v fuse-overlayfs >/dev/null 2>&1; then emit 9 notrun "fuse-overlayfs is not installed"; return; fi
    # The positive control: the same watcher and pokes on a plain bind of a
    # local directory. If even that shows nothing, the harness is at fault and
    # the FUSE answer below means nothing.
    local C=$W/f9ctl B=$W/f9 out p ctl
    mkdir -p "$C"
    for p in openclose mtime touch; do echo before >"$C/poke-$p.txt"; done
    [ "$ENGMODE" = setpriv ] && chown -R "$ENG_UID:$ENG_UID" "$C" 2>/dev/null
    if ! watch_matrix wp9c "$C" direct; then
        emit 9 refused "positive control (plain bind) did not run: $MATRIX"
        return
    fi
    ctl=$MATRIX
    if ! grep -q "^INOTIFY .* poke-" <<<"$WLOG"; then
        emit 9 refused "HARNESS FAULT: the positive control (plain bind, same watcher and pokes) saw no event for any poke:$ctl"
        return
    fi
    local cmd="mkdir -p $B/lower $B/upper $B/work $B/merged && for p in openclose mtime touch; do echo before >$B/lower/poke-\$p.txt; done && fuse-overlayfs -o lowerdir=$B/lower,upperdir=$B/upper,workdir=$B/work $B/merged"
    out=$(inns "$cmd" 2>&1)
    if [ $? -ne 0 ]; then
        emit 9 refused "fuse-overlayfs would not mount where the runtime sees it: $(firstline "$out") [positive control ok:$ctl]"
        return
    fi
    if watch_matrix wp9 "$B/merged" inns; then
        emit 9 ok "RECORDED masks on the FUSE mount:$MATRIX ; positive control on a plain bind:$ctl"
    else
        emit 9 refused "$MATRIX [positive control ok:$ctl]"
    fi
    inns "fusermount3 -u $B/merged 2>/dev/null || umount $B/merged" >/dev/null 2>&1
}

row4() {
    echo "== 4. root NFS mount in a private netns, into a rootless container =="
    if [ -z "${PROBE_NFS_PRESET:-}" ]; then
        emit 4 notrun "${PROBE_NFS_REASON:-runner only: needs a root-made mount before the daemon starts}"
        return
    fi
    need_engine 4 || return
    attempt 4 60 $DOCKER run $RUN_NET --rm -v "$PROBE_NFS_PRESET:/w" $IMG sh -c \
        'stat -c "%n uid=%u gid=%g" /w /w/hello /w/owned-by-1234; touch /w/written && echo write-ok'
}

row11() {
    echo "== 11. ports =="
    need_engine 11a 11b 11c || return
    say "tun device: $(ls -l /dev/net/tun 2>&1); slirp4netns: $(command -v slirp4netns || echo absent); rootlessport: $(find /usr -name rootlessport 2>/dev/null | head -n1)"
    timeout 60 $DOCKER run $RUN_NET -d --name web11 -p 18080:80 $IMG httpd -f -p 80 -h /etc >"$W/web11.out" 2>&1
    sleep 3
    say "web11 start: $(cat "$W/web11.out")"
    say "containers: $($DOCKER ps -a 2>&1 | tr '\n' ';')"
    say "web11 log: $($DOCKER logs web11 2>&1 | head -n3 | tr '\n' ';')"
    say "listeners: $(ss -ltnp 2>&1 | tr '\n' ';')"
    attempt 11a 15 wget -qO- -T5 http://127.0.0.1:18080/hostname
    $DOCKER rm -f web11 >/dev/null 2>&1
    # Where the container's own network points: the gateway rows depend on it.
    say "in a container: $(timeout 60 $DOCKER run $RUN_NET --rm $IMG sh -c 'ip -4 addr; ip route; cat /etc/resolv.conf' 2>&1 | tr '\n' ';')"
    busybox httpd -f -p 18081 -h /etc >"$W/pod-httpd.log" 2>&1 &
    local hpid=$! ip net=$RUN_NET
    sleep 1
    # slirp4netns refuses the host's loopback unless told otherwise.
    [ "$ENGINE" = podman ] && net=${RUN_NET/slirp4netns/slirp4netns:allow_host_loopback=true}
    ip=$(ip -4 -o addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -n1)
    attempt 11b 40 $DOCKER run $net --rm $IMG wget -qO- -T5 http://10.0.2.2:18081/hostname
    if [ -n "$ip" ]; then
        attempt 11c 40 $DOCKER run $RUN_NET --rm $IMG wget -qO- -T5 "http://$ip:18081/hostname"
    else
        emit 11c notrun "the pod has no global IPv4 address"
    fi
    kill "$hpid" 2>/dev/null
}

row12() {
    echo "== 12. cgroups =="
    local c s me
    c=$(cat /sys/fs/cgroup/cgroup.controllers 2>&1)
    s=$(cat /sys/fs/cgroup/cgroup.subtree_control 2>&1)
    me=$(cat /proc/self/cgroup 2>&1)
    say "controllers=[$c] subtree_control=[$s] self=[$me]"
    if [ -n "$c" ] && ! grep -q 'No such\|denied' <<<"$c"; then
        emit 12a ok "controllers=[$c] subtree_control=[$s] self=[$me]"
    else
        emit 12a refused "controllers: ${c:-empty}"
    fi
    need_engine 12b || return
    attempt 12b 90 $DOCKER run $RUN_NET --rm --memory 64m --cpus 0.5 $IMG true
}

rows() {
    W=${PROBE_WORK:-$(mktemp -d)}
    mkdir -p "$W"
    if [ "$(id -u)" -eq 0 ]; then
        ENGMODE=setpriv
        [ "${PROBE_ENGINE:-}" = none ] && ENGMODE=none
    fi
    context
    row1
    row2
    row3
    row5
    row6
    row7
    row8
    row9
    row4
    row11
    row12
    [ -n "$NFS_PID" ] && kill "$NFS_PID" 2>/dev/null
    echo "== done: $TIER =="
}

# ---- the runner -------------------------------------------------------------

collect() { # <log>: what a log recorded, into the results
    grep -E '^(RESULT|FACT)\|' "$1" >>"$PROBE_OUT/results.txt"
}

build_probes() {
    BIN=$PROBE_OUT/bin
    mkdir -p "$BIN"
    [ -x "$BIN/watchprobe" ] && return 0
    local p
    for p in watchprobe pokeprobe; do
        (cd "$HERE/probes/$p" && CGO_ENABLED=0 GOOS=linux go build -o "$BIN/$p" .) || {
            echo "setup error: cannot build $p" >&2
            exit 2
        }
    done
}

# build_image: the probe image, for the pods and for the runner's NFS servers.
build_image() {
    local stage=$PROBE_OUT/stage
    build_probes
    mkdir -p "$stage"
    cp "$HERE/unprivileged-probe/Dockerfile" "$SELF" "$BIN/watchprobe" "$BIN/pokeprobe" "$stage/"
    docker build -t unprivileged-probe:kind "$stage" || { echo "setup error: image build" >&2; exit 2; }
}

# The runner has no unfs3 package, so the pod image's server runs in containers:
# one on the host network (rows 3a, 3b), one in a private netns of its own whose
# client mount a root process makes (row 4). The mount is made before any
# rootless daemon starts, so that daemon's mount namespace inherits it.
prep_nfs_servers() {
    local d=/srv/rdprobe pid out
    sudo mkdir -p $d/export $d/mnt
    echo "from the server" | sudo tee $d/export/hello >/dev/null
    echo "owned by 1234" | sudo tee $d/export/owned-by-1234 >/dev/null
    sudo chown 1234:1234 $d/export/owned-by-1234
    sudo chmod -R a+rX /srv $d
    echo "$d/export 127.0.0.1(rw,no_root_squash,insecure)" | sudo tee $d/exports >/dev/null
    docker rm -f rd-unfsd rd-unfsd-ns >/dev/null 2>&1
    docker run -d --name rd-unfsd --network host --entrypoint unfsd -v $d:$d unprivileged-probe:kind \
        -u -d -p -n 2049 -m 2049 -e $d/exports >/dev/null 2>&1 || echo "note: the host-network NFS server did not start"
    docker run -d --name rd-unfsd-ns --network none --entrypoint unfsd -v $d:$d unprivileged-probe:kind \
        -u -d -p -n 2049 -m 2049 -e $d/exports >/dev/null 2>&1 || echo "note: the private-netns NFS server did not start"
    sleep 3
    PROBE_NFS_EXPORT=$d/export
    pid=$(docker inspect -f '{{.State.Pid}}' rd-unfsd-ns 2>/dev/null)
    if [ -z "$pid" ] || [ "$pid" = 0 ]; then
        PROBE_NFS_PRESET=""
        PROBE_NFS_REASON="the private-netns NFS server container is not running: $(docker logs rd-unfsd-ns 2>&1 | head -n2)"
        return
    fi
    if out=$(sudo nsenter --net="/proc/$pid/ns/net" mount -t nfs -o "$NFS_OPTS" 127.0.0.1:$d/export $d/mnt 2>&1); then
        PROBE_NFS_PRESET=$d/mnt
        PROBE_NFS_REASON=""
    else
        PROBE_NFS_PRESET=""
        PROBE_NFS_REASON="the root mount in the private netns failed: $(firstline "$out")"
    fi
}

run_tier() { # <tier> <who: root|user> [extra env...]
    local tier=$1 who=$2 log w
    shift 2
    log=$PROBE_OUT/log-$tier.txt
    if [ "$who" = root ]; then
        w=$(sudo mktemp -d)
        (cd /tmp && sudo env PROBE_TIER="$tier" PROBE_WORK="$w" PROBE_ENGINE=none "$@" \
            bash /usr/local/bin/unprivileged-probe.sh rows >"$log" 2>&1)
    else
        w=$(sudo -u probe mktemp -d)
        (cd /tmp && sudo -H -u probe env PROBE_TIER="$tier" PROBE_WORK="$w" "$@" \
            bash /usr/local/bin/unprivileged-probe.sh rows >"$log" 2>&1)
        sudo pkill -u probe 2>/dev/null
        sleep 2
    fi
    sed 's/^/  | /' "$log"
    collect "$log"
}

runner_mode() {
    sudo apt-get update -qq
    # One at a time: a package that does not exist must not take the rest down.
    local pkg
    for pkg in uidmap fuse-overlayfs fuse3 nfs-common unfs3 slirp4netns passt \
        docker-ce-rootless-extras podman util-linux attr psmisc iproute2 busybox; do
        sudo apt-get install -y -qq "$pkg" >/dev/null 2>&1 || echo "note: runner package $pkg did not install"
    done
    sudo modprobe nfs 2>/dev/null
    build_image
    sudo cp "$BIN"/watchprobe "$BIN"/pokeprobe /usr/local/bin/
    sudo cp "$SELF" /usr/local/bin/unprivileged-probe.sh
    sudo useradd -m -s /bin/bash probe 2>/dev/null
    grep -q '^probe:' /etc/subuid || echo 'probe:100000:65536' | sudo tee -a /etc/subuid >/dev/null
    grep -q '^probe:' /etc/subgid || echo 'probe:100000:65536' | sudo tee -a /etc/subgid >/dev/null
    {
        fact kernel "$(uname -r)" runner
        fact os "$(. /etc/os-release && echo "$PRETTY_NAME")" runner
        fact apparmor_restrict_unprivileged_userns "$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo absent)" runner
    } >>"$PROBE_OUT/results.txt"
    prep_nfs_servers
    run_tier runner-root root PROBE_NFS_EXPORT="$PROBE_NFS_EXPORT"
    run_tier runner-user user PROBE_NFS_EXPORT="$PROBE_NFS_EXPORT" PROBE_NFS_PRESET="$PROBE_NFS_PRESET" PROBE_NFS_REASON="$PROBE_NFS_REASON"
    # The same user with the Ubuntu restriction lifted, clearly labelled: what
    # the rows say once the one sysctl that blocks them is off.
    local was
    was=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null)
    if [ "${was:-0}" = 1 ]; then
        sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 >/dev/null
        run_tier runner-user-unrestricted user PROBE_NFS_EXPORT="$PROBE_NFS_EXPORT" PROBE_NFS_PRESET="$PROBE_NFS_PRESET" PROBE_NFS_REASON="$PROBE_NFS_REASON"
        sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=1 >/dev/null
    else
        say "the runner does not restrict unprivileged user namespaces; no second user pass"
    fi
    docker rm -f rd-unfsd rd-unfsd-ns >/dev/null 2>&1
}

# ---- the cluster ------------------------------------------------------------

render() { # <tier> <name> <namespace> <hostusers: default|false>
    local file=$HERE/unprivileged-probe/$1.yaml proc="" hu="#__HOSTUSERS__"
    [ "$4" = false ] && hu="hostUsers: false" && proc="procMount: Unmasked"
    sed -e "s/__NAME__/$2/g" -e "s/__NS__/$3/g" -e "s/#__HOSTUSERS__/$hu/" \
        -e "s/#__PROCMOUNT__/${proc:-#}/" "$file"
}

kind_setup() {
    local cluster=${KIND_CLUSTER:-probe}
    kubectl cluster-info >/dev/null 2>&1 || { echo "setup error: no cluster to talk to" >&2; exit 2; }
    build_image
    kind load docker-image unprivileged-probe:kind --name "$cluster" || { echo "setup error: kind load" >&2; exit 2; }

    {
        fact kubernetes "$(kubectl version -o json 2>/dev/null | tr -d '\n ' | sed 's/.*"serverVersion":{[^}]*"gitVersion":"\([^"]*\)".*/\1/')" cluster
        fact node "$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.containerRuntimeVersion} kernel {.items[0].status.nodeInfo.kernelVersion} {.items[0].status.nodeInfo.osImage}')" cluster
        fact kind "$(kind version 2>&1)" cluster
    } >>"$PROBE_OUT/results.txt"

    local ns lvl msg
    for ns in privileged baseline restricted; do
        kubectl create namespace "probe-$ns" >/dev/null 2>&1
        kubectl label --overwrite namespace "probe-$ns" "pod-security.kubernetes.io/enforce=$ns" >/dev/null
    done

    # The permissive spec offered to the stricter levels: what the admission says.
    for lvl in baseline restricted; do
        msg=$(render permissive-root adm-probe "probe-$lvl" default | kubectl apply --dry-run=server -f - 2>&1)
        printf 'FACT|cluster|permissive-root spec offered to %s|%s\n' "$lvl" "$(clean <<<"$msg")" >>"$PROBE_OUT/results.txt"
    done
}

# kind_mode: the pods run on the runner's kernel, so the runner's AppArmor
# restriction on user namespaces applies to them. Run the tiers with it as it
# is (pods named plainly), then with it off (pods suffixed -ur0).
kind_mode() {
    kind_setup
    local was
    was=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo absent)
    printf 'FACT|cluster|apparmor_restrict_unprivileged_userns, pods without a suffix|%s\n' "$was" >>"$PROBE_OUT/results.txt"
    kind_pass "" yes
    if [ "$was" = 1 ]; then
        sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 >/dev/null
        printf 'FACT|cluster|apparmor_restrict_unprivileged_userns, pods suffixed -ur0|0\n' >>"$PROBE_OUT/results.txt"
        kind_pass -ur0 no
        sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=1 >/dev/null
    fi
}

# kind_pass <suffix> <yes|no>: every tier once; the second argument says whether
# the hostUsers: false variants run too.
kind_pass() {
    local sfx=$1 withuns=$2
    # name|manifest|namespace
    local specs=(
        "perm-nonroot|permissive-nonroot|probe-privileged"
        "perm-root|permissive-root|probe-privileged"
        "baseline|baseline|probe-baseline"
        "restricted|restricted|probe-restricted"
        "baseline-localhost|baseline-localhost|probe-baseline"
        "restricted-localhost|restricted-localhost|probe-restricted"
        "control|control|probe-privileged"
    )
    local s name file hu pod pods=() out rc ns
    for s in "${specs[@]}"; do
        IFS='|' read -r name file ns <<<"$s"
        for hu in default false; do
            [ "$hu" = false ] && { [ "$name" = control ] || [ "$withuns" = no ]; } && continue
            case "$name" in *-localhost) [ "$hu" = false ] && continue ;; esac
            pod=$name
            [ "$hu" = false ] && pod=$name-uns
            pod=$pod$sfx
            kubectl delete pod "$pod" -n "$ns" --ignore-not-found >/dev/null 2>&1
            out=$(render "$file" "$pod" "$ns" "$hu" | kubectl apply -f - 2>&1)
            rc=$?
            if [ "$rc" -ne 0 ] && [ "$hu" = false ] && grep -qi procmount <<<"$out"; then
                say "$pod: procMount: Unmasked refused ($(firstline "$out")); retrying without it"
                out=$(render "$file" "$pod" "$ns" "$hu" | sed '/procMount: Unmasked/d' | kubectl apply -f - 2>&1)
                rc=$?
            fi
            if [ "$rc" -ne 0 ]; then
                echo "$pod: admission said: $out"
                printf 'RESULT|%s|0|refused|admission: %s\n' "$pod" "$(clean <<<"$out")" >>"$PROBE_OUT/results.txt"
            else
                pods+=("$pod|$ns")
            fi
        done
    done

    # Wait for every admitted pod: finished, or stuck Pending past 4 minutes.
    local deadline=$((SECONDS + 1800)) pending=("${pods[@]}") still p phase n ev started
    declare -A seen_at
    while [ ${#pending[@]} -gt 0 ] && [ $SECONDS -lt $deadline ]; do
        still=()
        for p in "${pending[@]}"; do
            pod=${p%%|*}
            n=${p#*|}
            phase=$(kubectl get pod "$pod" -n "$n" -o jsonpath='{.status.phase}' 2>/dev/null)
            started=${seen_at[$pod]:-$SECONDS}
            seen_at[$pod]=$started
            case "$phase" in
                Succeeded | Failed) finish_pod "$pod" "$n" ;;
                Pending)
                    if [ $((SECONDS - started)) -gt 240 ]; then
                        ev=$(kubectl get events -n "$n" --field-selector "involvedObject.name=$pod" \
                            -o jsonpath='{range .items[*]}{.type}: {.reason}: {.message}{"\n"}{end}' 2>&1 | grep -v '^Normal' | tail -n1)
                        echo "$pod: never started: $ev"
                        case "$pod:$ev" in *-uns*:*sysfs*) ev="$ev [kind limitation (nested node container), needs a real cluster]" ;; esac
                        printf 'RESULT|%s|0|refused|admitted but never started: %s\n' "$pod" "$(clean <<<"$ev")" >>"$PROBE_OUT/results.txt"
                        kubectl describe pod "$pod" -n "$n" >"$PROBE_OUT/describe-$pod.txt" 2>&1
                    else
                        still+=("$p")
                    fi
                    ;;
                *) still+=("$p") ;;
            esac
        done
        pending=("${still[@]}")
        [ ${#pending[@]} -gt 0 ] && sleep 10
    done
    for p in "${pending[@]}"; do
        pod=${p%%|*}
        n=${p#*|}
        echo "$pod: still running at the deadline; taking its log as it stands"
        finish_pod "$pod" "$n"
    done
}

finish_pod() { # <pod> <namespace>
    local log=$PROBE_OUT/log-$1.txt
    kubectl logs "$1" -n "$2" >"$log" 2>&1
    echo "---- $1 ----"
    sed 's/^/  | /' "$log"
    collect "$log"
    grep -q "^RESULT|$1|0|" "$log" || printf 'RESULT|%s|0|refused|ran but recorded nothing: %s\n' "$1" "$(clean <<<"$(firstline "$(cat "$log")")")" >>"$PROBE_OUT/results.txt"
}

# ---- the table --------------------------------------------------------------

ROWS=(
    "0|the pod was admitted and ran"
    "1|user namespace: unshare -Ur true"
    "2|nested uid map of more than one uid (newuidmap)"
    "3a|kernel NFS mount, initial user namespace"
    "3b|kernel NFS mount under unshare -Urm"
    "3c|nfs in /proc/filesystems"
    "4|root NFS mount in a private netns, bound into a rootless container"
    "5a|/dev/fuse can be opened"
    "5b|fuse-overlayfs under unshare -Urm"
    "5c|fuse-overlayfs, no user namespace"
    "6a|rootless dockerd starts"
    "6b|rootless podman system service starts"
    "6c|stock rootful dockerd starts"
    "6d|rootless podman runs a container"
    "7|image pull and docker run"
    "8|enter the daemon's mount namespace; a container sees a mount made there"
    "9|inotify on a FUSE mount, container bound to it"
    "10|fuse-overlayfs marks in a user namespace"
    "11a|published port reachable from the pod"
    "11b|container reaches a pod listener via 10.0.2.2"
    "11c|container reaches a pod listener via the pod IP"
    "12a|cgroup v2 controllers delegated"
    "12b|docker run --memory 64m --cpus 0.5"
)

# ref_for <tier>: the tier whose pass makes a refusal in this one a measurement.
ref_for() {
    case "$1" in
        runner-root | control | control-ur0) echo "" ;;
        runner-user*) echo runner-root ;;
        *-ur0) echo control-ur0 ;;
        *) echo control ;;
    esac
}

report() {
    local f=$PROBE_OUT/results.txt out=$PROBE_OUT/table.md
    [ -s "$f" ] || { echo "no results recorded in $f"; return 0; }
    declare -A res
    local tiers=() line t r st d k v ref rs
    while IFS='|' read -r k t r st d; do
        [ "$k" = RESULT ] || continue
        res[$t|$r]="$st|$d"
        case " ${tiers[*]:-} " in *" $t "*) ;; *) tiers+=("$t") ;; esac
    done <"$f"
    {
        echo "# Unprivileged workspace probe"
        echo
        echo "Recorded answers, never assertions. A refusal counts as measured only where the reference tier (the privileged control pod for the cluster, the root pass for the runner) passed the same row; otherwise it is not measured."
        echo
        echo "## Environment"
        echo
        echo "| scope | fact | value |"
        echo "|---|---|---|"
        while IFS='|' read -r k t r st; do
            [ "$k" = FACT ] || continue
            case "$t" in cluster | runner) echo "| $t | $r | ${st//|/ } |" ;; esac
        done <"$f"
        for t in "${tiers[@]}"; do
            echo
            echo "## $t"
            echo
            while IFS='|' read -r k tt kk vv; do
                [ "$k" = FACT ] && [ "$tt" = "$t" ] && echo "- $kk: ${vv:-}"
            done <"$f"
            echo
            echo "| row | question | answer |"
            echo "|---|---|---|"
            for line in "${ROWS[@]}"; do
                r=${line%%|*}
                v=${res[$t|$r]:-}
                if [ -z "$v" ]; then
                    if [[ "${res[$t|0]:-}" == ok* ]]; then
                        v="notrun|no result recorded (the rows stopped early)"
                    else
                        v="notrun|the pod never ran (row 0)"
                    fi
                fi
                st=${v%%|*}
                d=${v#*|}
                ref=$(ref_for "$t")
                rs=""
                [ -n "$ref" ] && rs=${res[$ref|$r]%%|*}
                case "$st" in
                    ok) a="measured ok${d:+: $d}" ;;
                    refused)
                        if [ -z "$ref" ]; then
                            a="not measured (the reference itself failed): $d"
                        elif [ "$rs" = ok ]; then
                            a="measured refused (the reference passed): $d"
                        else
                            a="not measured (the reference ${rs:-has no result}, so a harness or kernel limit): $d"
                        fi
                        ;;
                    *) a="not measured: $d" ;;
                esac
                echo "| $r | ${line#*|} | ${a//|/ } |"
            done
        done
    } | tee "$out"
    [ -n "${GITHUB_STEP_SUMMARY:-}" ] && cat "$out" >>"$GITHUB_STEP_SUMMARY"
    return 0
}

# ---- main -------------------------------------------------------------------

mode=${1:-all}
case "$mode" in
    rows)
        rows
        exit 0
        ;;
    render)
        render "${2:?manifest}" "${3:-name}" "${4:-ns}" "${5:-default}"
        exit 0
        ;;
esac
PROBE_OUT=${PROBE_OUT:-$(mktemp -d)}
mkdir -p "$PROBE_OUT"
export PROBE_OUT
case "$mode" in
    runner) runner_mode ;;
    kind) kind_mode ;;
    report) report ;;
    all)
        runner_mode
        kind_mode
        report
        ;;
    *)
        echo "usage: $0 [runner|kind|report|all|render <manifest> [name ns hostusers]|rows]" >&2
        exit 2
        ;;
esac
exit 0
