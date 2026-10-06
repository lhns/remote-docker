#!/usr/bin/env bash
# Ephemeral clients (ADR 0050), end to end: many runs of ONE key at once, what
# their ending leaves behind, and an ordinary machine account beside them.
#
#   bash test/ephemeral.sh shared|per-account
#
# The argument is the daemon mode (ADR 0012 or ADR 0019). Cleanup goes through
# `daemons.Targets`, so both are run.
#
# Every run sees the SAME checkout path with different files in it, which is
# what an autoscaled CI runner looks like: each client process runs in a mount
# namespace of its own with its own directory bound over $CHECKOUT. Without
# that, four exports of one directory would serve the same marker and a
# container reading another run's export would pass.
#
# Docker commands for a run go to its own endpoint. What the WORKSPACE holds is
# asked of it directly (`wsdocker`, `remote-dockerd ephemeral ls`), so a run that
# has ended is never reconnected by the question.
#
# Sections 1 to 7 share one workspace with a 20s grace. Section 8 replaces it
# with one whose grace outlasts the agent's ~60s dead-peer detection.
#
# Requires: docker, sudo, util-linux (unshare, setpriv), curl, and a kernel with
# NFS client support.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-ephemeral
SSH_PORT=22225

EPH=ci        # the ephemeral account: every run shares its one key
MACHINE=alice # an ordinary machine account on the same workspace (ADR 0029)

# Short, so each wait for a cleanup is half a minute; the default is 2m.
GRACE=20
MAX_CLIENTS=4

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

CLIENT_LABEL=com.github.lhns.remote-docker.client
OWNER_LABEL=com.github.lhns.remote-docker.owner
PROJECT_LABEL=com.docker.compose.project

CHECKOUT="$WORK/checkout"

cleanup() {
    # The runs were started through sudo, so their pids are root's to signal.
    # Each is matched by the binary's path, which is this suite's alone. The
    # bracket keeps the pattern from matching the sudo that carries it.
    sudo pkill -f "$WORK/[r]emote-docker" 2>/dev/null
    cleanup_suite "${MACHINE_PID:-}"
}
trap cleanup EXIT

# wsdocker runs docker against <account>'s daemon on the workspace: the shared
# one, or the account's own dind.
#
#   wsdocker <account> <args...>
wsdocker() {
    local account=$1
    shift
    if [ "$PER_USER_DIND" = true ]; then
        hostdocker exec "$CONTAINER" docker exec "rd-dind-$account" docker "$@"
    else
        hostdocker exec "$CONTAINER" docker "$@"
    fi
}

# runs prints `remote-dockerd ephemeral ls` for the ephemeral account only, as
# `client state port` lines.
runs() {
    hostdocker exec "$CONTAINER" remote-dockerd ephemeral ls 2>&1 |
        awk -v a="$EPH" '$1 == a {print $2, $3, $4}'
}

# run_field prints one field of a client's row in `runs`: 2 is the state, 3
# the port. Empty for a client the workspace does not list.
run_field() { runs | awk -v c="$1" -v f="$2" '$1 == c {print $f}'; }

# leftovers prints every object on the workspace carrying <client>'s id:
# containers, compose networks and volumes. Empty means the run is gone.
leftovers() {
    local client=$1
    wsdocker "$EPH" ps -a --filter "label=$CLIENT_LABEL=$client" --format 'container {{.Names}}'
    wsdocker "$EPH" volume ls --filter "label=$CLIENT_LABEL=$client" --format 'volume {{.Name}}'
    wsdocker "$EPH" network ls --filter "label=$PROJECT_LABEL" \
        --format "{{.Name}} {{.Label \"$PROJECT_LABEL\"}}" | awk -v c="-$client" \
        'substr($2, length($2) - length(c) + 1) == c {print "network", $1}'
    run_field "$client" 0 | sed 's/^/record /'
}

# wait_run_gone waits for a run to leave the record and the workspace, which
# takes one grace and a sweep, plus the cleanup itself.
wait_run_gone() {
    local client=$1 _
    for _ in $(seq 1 $((GRACE * 6))); do
        LAST_OUTPUT=$(leftovers "$client" 2>&1)
        [ -z "$LAST_OUTPUT" ] && return 0
        sleep 1
    done
    return 1
}

# ----------------------------------------------------------------------------
# A run: one client process, its own state directory holding the shared key,
# its own endpoint, and its own directory at $CHECKOUT.

run_sock() { echo "$WORK/run-$1.sock"; }

# run_env prints the environment a command for run <n> needs.
run_env() {
    local n=$1
    echo "REMOTE_DOCKER_STATE_DIR=$WORK/state-$n" \
        "REMOTE_DOCKER_HOST=127.0.0.1" \
        "REMOTE_DOCKER_PORT=$SSH_PORT" \
        "REMOTE_DOCKER_USER=$EPH" \
        "REMOTE_DOCKER_ENDPOINT=$(run_sock "$n")" \
        "REMOTE_DOCKER_WATCH=partial"
}

# start_run starts run <n> in the background and prints the pid of the sudo
# in front of it; run_pid asks the run itself for the client's.
#
# sudo for the mount namespace only: setpriv drops back to this user before
# the client starts, so the client is as unprivileged as anywhere else.
start_run() {
    local n=$1
    mkdir -p "$WORK/src-$n" "$WORK/state-$n"
    cp "$WORK/key/id_ed25519" "$WORK/key/id_ed25519.pub" "$WORK/state-$n/"
    echo "run $n" >"$WORK/src-$n/marker"
    cp "$CHECKOUT/compose.yaml" "$WORK/src-$n/"

    # shellcheck disable=SC2016,SC2024,SC2046  # expanded by the inner sh; the log is ours to own; run_env splits on purpose
    sudo unshare --mount --propagation private -- sh -c '
        mount --bind "$1" "$2" || exit 1
        cd "$2" || exit 1
        shift 2
        exec "$@"' _ "$WORK/src-$n" "$CHECKOUT" \
        setpriv --reuid="$(id -u)" --regid="$(id -g)" --init-groups -- \
        env HOME="$HOME" PATH="$PATH" $(run_env "$n") \
        "$WORK/remote-docker" remote start --foreground \
        >"$WORK/run-$n.log" 2>&1 &
    echo $!
}

# wait_run waits for run <n>'s endpoint to answer, giving up at once when the
# sudo in front of it has exited. `ps`, not `kill -0`: that pid is root's.
wait_run() {
    local n=$1 pid=$2 sock
    sock=$(run_sock "$n")
    for _ in $(seq 1 120); do
        if [ -S "$sock" ] && docker -H "unix://$sock" info >/dev/null 2>&1; then
            return 0
        fi
        ps -p "$pid" >/dev/null 2>&1 || return 1
        sleep 2
    done
    return 1
}

# control asks run <n>'s session one of its own questions (proxy.ControlPrefix).
control() {
    curl -s --max-time 30 --unix-socket "$(run_sock "$1")" "http://session/_remote-docker/$2"
}

# client_of prints the client id the workspace derived for run <n>.
client_of() { control "$1" client | sed -n 's/.*"client":"\([0-9a-f]*\)".*/\1/p'; }

# pid_of prints run <n>'s client process.
pid_of() { control "$1" status | sed -n 's/.*"pid":\([0-9]*\).*/\1/p'; }

drun() {
    local n=$1
    shift
    dockerat "$(run_sock "$n")" "$@"
}

# compose_in runs the EMBEDDED compose (ADR 0009) for run <n>, from $CHECKOUT
# outside every namespace: the file is the same everywhere, and a bind of `.`
# is a path the run's own session resolves.
compose_in() {
    local n=$1
    shift
    # shellcheck disable=SC2046  # run_env splits on purpose
    (cd "$CHECKOUT" && timeout "$DOCKER_TIMEOUT" env $(run_env "$n") "$WORK/remote-docker" compose "$@")
}

# reads_own_file reports whether run <n>'s container reads "run <n>" through a
# bind of $CHECKOUT. The last line: anything before it is docker talking.
reads_own_file() {
    local n=$1 seen
    seen=$(drun "$n" run --rm -v "$CHECKOUT:/w" alpine:3 cat /w/marker 2>&1 | tail -1)
    LAST_OUTPUT=$seen
    [ "$seen" = "run $n" ]
}

# merged_of prints <client>'s union mounts as the namespace of $EPH's daemon
# sees them, one `<fstype> <mountpoint>` line per merged view
# (/run/rd-union/<client>/<share>/merged). A mount, not a directory: a union
# that never mounted leaves the directory there (ADR 0044). Fails, saying so,
# when the mounts cannot be read, so an unreadable table is never "unmounted".
merged_of() {
    local client=$1 table
    if [ "$PER_USER_DIND" = true ]; then
        table=$(hostdocker exec "$CONTAINER" docker exec "rd-dind-$EPH" cat /proc/mounts 2>&1)
    else
        table=$(hostdocker exec "$CONTAINER" cat /proc/mounts 2>&1)
    fi || {
        echo "cannot read the daemon's mounts: $table"
        return 1
    }
    awk -v p="/run/rd-union/$client/" 'index($2, p) == 1 && $2 ~ /\/merged$/ {print $3, $2}' <<<"$table"
}

# union_released asserts that <client>'s run took its union with it: nothing
# mounted under its directory, and (from wait_run_gone) no volume, cache
# included, and no record.
union_released() {
    local n=$1 client=$2 how=$3 start mounts
    start=$(date +%s)
    if wait_run_gone "$client"; then
        ok "run $n ($how): its record, containers and volumes, cache included, are gone $(($(date +%s) - start))s later"
    else
        bad "run $n ($how) left: $LAST_OUTPUT"
        hostdocker logs -t "$CONTAINER" 2>&1 | grep -iE "\[ephemeral\]|union" | tail -12 | sed 's/^/        /'
        # A cleanup still running shows as a process waiting on something.
        hostdocker exec "$CONTAINER" ps -o pid,stat,args 2>&1 |
            awk '/union|unmount|fuse-overlayfs|docker volume/' | sed 's/^/        /'
    fi
    if mounts=$(merged_of "$client"); then
        if [ -z "$mounts" ]; then
            ok "run $n ($how): its union is unmounted"
        else
            bad "run $n ($how): its union is still mounted: [$mounts]"
            union_diagnostics
        fi
    else
        bad "run $n ($how): $mounts"
    fi
}

# ----------------------------------------------------------------------------
# The machine account, which nothing here may change (section 7).

MACHINE_SOCK="$WORK/machine.sock"
dmachine() { dockerat "$MACHINE_SOCK" "$@"; }

machine_port() { hostdocker exec "$CONTAINER" cat /etc/workspace/clientports 2>/dev/null | grep "^$MACHINE:"; }
machine_volumes() { wsdocker "$MACHINE" volume ls -q --filter "label=$OWNER_LABEL=$MACHINE" 2>&1 | sort; }

# machine_unchanged compares the machine account's clientports line and its
# volumes with what section 1 recorded.
machine_unchanged() {
    local when=$1 port vols
    # Its daemon is asked for its volumes; a per-account one stopped by a
    # workspace restart starts again when its account connects.
    wait_output '^CONTAINER' 60 dmachine ps || info "$MACHINE's endpoint did not answer: [$LAST_OUTPUT]"
    port=$(machine_port)
    vols=$(machine_volumes)
    if [ "$port" = "$MACHINE_PORT_BEFORE" ] && [ "$vols" = "$MACHINE_VOLUMES_BEFORE" ]; then
        ok "$when: $MACHINE's clientports entry and volumes are unchanged"
    else
        bad "$when: $MACHINE changed: port [$MACHINE_PORT_BEFORE] -> [$port], volumes [$MACHINE_VOLUMES_BEFORE] -> [$vols]"
    fi
}

echo "== 0. build =="
build_all

echo
echo "== 0b. one key for every run, and a machine account beside it =="
mkdir -p "$WORK/keys" "$WORK/wsstate" "$WORK/key"
if genkey "$WORK/key"; then
    cp "$WORK/key/id_ed25519.pub" "$WORK/keys/$EPH.pub"
    ok "one key staged as $EPH.pub"
else
    bad "no key generated for $EPH"
    exit 1
fi
if enrol "$MACHINE" "$WORK/state-$MACHINE"; then
    ok "a key staged as $MACHINE.pub"
else
    bad "no key generated for $MACHINE"
    exit 1
fi

mode_args=()
if [ "$PER_USER_DIND" = true ]; then
    # The workspace's own image, as the chart sets it (why: per-user-dind.sh).
    mode_args=(-e "WORKSPACE_DIND_IMAGE=$IMAGE")
fi
workspace_up "$PER_USER_DIND" "$EPH $MACHINE" \
    -e "WORKSPACE_EPHEMERAL_ACCOUNTS=$EPH" \
    -e "WORKSPACE_EPHEMERAL_GRACE=${GRACE}s" \
    -e "WORKSPACE_EPHEMERAL_MAX_CLIENTS=$MAX_CLIENTS" \
    -e "WORKSPACE_EPHEMERAL_CLEANUP_CONTAINERS=true" \
    "${mode_args[@]}"

if [ "$PER_USER_DIND" = true ]; then
    # Before any account connects, since the first connection starts its daemon.
    if load_image_into_workspace "$IMAGE"; then
        ok "each account's daemon can start from the workspace image"
    else
        bad "could not load $IMAGE into the workspace's daemon"
    fi
fi

# The checkout every run sees at the same path. What is here OUTSIDE every
# namespace is a marker no run may read, and the compose file the embedded
# compose reads.
mkdir -p "$CHECKOUT"
echo "outside every run" >"$CHECKOUT/marker"
cat >"$CHECKOUT/compose.yaml" <<'COMPOSE'
services:
  reader:
    image: alpine:3
    command: ["sh", "-c", "cat /w/marker; exec sleep 600"]
    volumes:
      - .:/w
COMPOSE

mkdir -p "$WORK/machine-project"
echo "the machine's file" >"$WORK/machine-project/marker"
MACHINE_PID=$(start_session "$WORK/state-$MACHINE" "$MACHINE" "$MACHINE_SOCK" \
    "$WORK/machine.log" "$WORK/machine-project")
endpoint_up "$MACHINE_SOCK" "$MACHINE_PID" "$WORK/machine.log" || exit 1
dmachine pull -q alpine:3 >/dev/null 2>&1 || info "could not pre-pull alpine:3 for $MACHINE"
if outputs "^the machine's file$" dmachine run --rm -v "$WORK/machine-project:/w" alpine:3 cat /w/marker; then
    ok "$MACHINE reads its own file"
else
    bad "$MACHINE could not read its file: [$LAST_OUTPUT]"
fi
MACHINE_PORT_BEFORE=$(machine_port)
MACHINE_VOLUMES_BEFORE=$(machine_volumes)
if [ -n "$MACHINE_PORT_BEFORE" ] && [ -n "$MACHINE_VOLUMES_BEFORE" ]; then
    ok "$MACHINE has a clientports entry [$MACHINE_PORT_BEFORE] and a volume [$MACHINE_VOLUMES_BEFORE]"
else
    bad "$MACHINE has no clientports entry [$MACHINE_PORT_BEFORE] or no volume [$MACHINE_VOLUMES_BEFORE]"
fi

echo
echo "== 1. four runs of one key, at once =="
declare -A SUDO_PID CLIENT
for n in 1 2 3 4; do
    SUDO_PID[$n]=$(start_run "$n")
done
for n in 1 2 3 4; do
    if wait_run "$n" "${SUDO_PID[$n]}"; then
        ok "run $n has a working docker endpoint"
    else
        bad "run $n never came up"
        sed "s/^/        run $n: /" "$WORK/run-$n.log" | tail -20
        dump_workspace_log 40
        exit 1
    fi
done

for n in 1 2 3 4; do
    CLIENT[$n]=$(client_of "$n")
done
ids="${CLIENT[1]} ${CLIENT[2]} ${CLIENT[3]} ${CLIENT[4]}"
if [ "$(tr ' ' '\n' <<<"$ids" | grep -c '^[0-9a-f]\{8\}$')" -eq 4 ] &&
    [ "$(tr ' ' '\n' <<<"$ids" | sort -u | grep -c .)" -eq 4 ]; then
    ok "four distinct client ids from one key: $ids"
else
    bad "the runs do not have four distinct client ids: [$ids]"
    exit 1
fi

info "pulling the test image"
drun 1 pull -q alpine:3 >/dev/null 2>&1 || info "could not pre-pull alpine:3 for $EPH"

for n in 1 2 3 4; do
    if reads_own_file "$n"; then
        ok "run $n's container reads run $n's file at the shared path"
    else
        bad "run $n's container read [$LAST_OUTPUT]"
    fi
done

# Asked after the binds, which every run has now hosted a forward for.
listed=$(runs)
echo "$listed" | sed 's/^/        /'
ports=""
for n in 1 2 3 4; do
    ports="$ports $(run_field "${CLIENT[$n]}" 3)"
done
if [ "$(tr ' ' '\n' <<<"$ports" | grep -c '^[0-9]\+$')" -eq 4 ] &&
    [ "$(tr ' ' '\n' <<<"$ports" | grep . | sort -u | grep -c .)" -eq 4 ]; then
    ok "each run holds a port of its own:$ports"
else
    bad "the runs do not hold four distinct ports: [$ports] from [$listed]"
fi
if outputs "^$EPH:" hostdocker exec "$CONTAINER" cat /etc/workspace/clientports; then
    bad "an ephemeral run was written to clientports: [$LAST_OUTPUT]"
else
    ok "no run of $EPH is in clientports"
fi

# rd-<client>-<share>: the same share for the same path, a different client.
shares=""
for n in 1 2 3 4; do
    vol=$(wsdocker "$EPH" volume ls -q --filter "label=$CLIENT_LABEL=${CLIENT[$n]}" 2>&1)
    if [[ "$vol" =~ ^rd-${CLIENT[$n]}-[^[:space:]]+$ ]]; then
        shares="$shares ${vol#rd-"${CLIENT[$n]}"-}"
    else
        bad "run $n has no single volume named for its client: [$vol]"
    fi
done
if [ "$(tr ' ' '\n' <<<"$shares" | grep -c .)" -eq 4 ] &&
    [ "$(tr ' ' '\n' <<<"$shares" | grep . | sort -u | grep -c .)" -eq 1 ]; then
    ok "one share, four volumes: rd-<client>-${shares##* }"
else
    bad "the runs' volumes do not name one share:$shares"
fi

# All four at once, each its own project because nothing named one (ADR 0050).
for n in 1 2 3 4; do
    compose_in "$n" up -d >"$WORK/compose-$n.log" 2>&1 &
    COMPOSE_PID[n]=$!
done
for n in 1 2 3 4; do
    if wait "${COMPOSE_PID[$n]}"; then
        ok "run $n: compose up succeeded alongside the others"
    else
        bad "run $n: compose up failed"
        sed "s/^/        /" "$WORK/compose-$n.log" | tail -15
    fi
done
for n in 1 2 3 4; do
    project="checkout-${CLIENT[$n]}"
    if ! outputs '[a-z]' drun "$n" ps -q --filter "label=$PROJECT_LABEL=$project"; then
        bad "run $n: no container of project $project: [$LAST_OUTPUT]"
        continue
    fi
    if wait_output "^run $n\$" 30 drun "$n" logs "$project-reader-1"; then
        ok "run $n: its compose project $project reads run $n's file"
    else
        bad "run $n: $project-reader-1 said [$LAST_OUTPUT]"
    fi
done
machine_unchanged "after four runs"

echo
echo "== 2. a clean end frees that run after its grace, and only that run =="
port1=$(run_field "${CLIENT[1]}" 3)
if outputs '"stopping"' curl -s --max-time 30 -X POST --unix-socket "$(run_sock 1)" \
    http://session/_remote-docker/shutdown; then
    ok "run 1 was asked to stop"
else
    bad "run 1 did not take the shutdown: [$LAST_OUTPUT]"
fi
if wait_output '^grace$' 30 run_field "${CLIENT[1]}" 2; then
    ok "run 1 is in its grace period once its connection ended"
else
    bad "run 1 never entered its grace period: [$LAST_OUTPUT] in [$(runs)]"
fi
start=$(date +%s)
if wait_run_gone "${CLIENT[1]}"; then
    ok "run 1 and everything carrying its id are gone $(($(date +%s) - start))s later (grace ${GRACE}s)"
else
    bad "run 1 left: $LAST_OUTPUT"
    hostdocker logs "$CONTAINER" 2>&1 | grep -i ephemeral | tail -10 | sed 's/^/        /'
fi
if [ "$(($(date +%s) - start))" -lt $((GRACE - 2)) ]; then
    bad "run 1 went after $(($(date +%s) - start))s, before its grace (${GRACE}s) was up"
fi
info "run 1's port was $port1"

for n in 2 3 4; do
    if outputs '^live$' run_field "${CLIENT[$n]}" 2; then
        ok "run $n is still live"
    else
        bad "run $n is [$LAST_OUTPUT] after run 1 ended"
    fi
    if outputs "^run $n\$" drun "$n" exec "checkout-${CLIENT[$n]}-reader-1" cat /w/marker; then
        ok "run $n's running container still reads its file"
    else
        bad "run $n's running container cannot read its file: [$LAST_OUTPUT]"
    fi
done

echo
echo "== 3. a client killed with -9 leaves nothing after its grace =="
pid3=$(pid_of 3)
if [ -n "$pid3" ] && kill -9 "$pid3" 2>/dev/null; then
    ok "run 3's client (pid $pid3) was killed"
else
    bad "could not kill run 3's client: pid [$pid3]"
fi
start=$(date +%s)
if wait_run_gone "${CLIENT[3]}"; then
    ok "run 3 and everything carrying its id are gone $(($(date +%s) - start))s later"
else
    bad "run 3 left: $LAST_OUTPUT"
    hostdocker logs "$CONTAINER" 2>&1 | grep -i ephemeral | tail -10 | sed 's/^/        /'
fi
for n in 2 4; do
    if outputs "^run $n\$" drun "$n" exec "checkout-${CLIENT[$n]}-reader-1" cat /w/marker; then
        ok "run $n's running container still reads its file"
    else
        bad "run $n's running container cannot read its file: [$LAST_OUTPUT]"
    fi
done
machine_unchanged "after two runs ended"

echo
echo "== 4. an outage shorter than the grace keeps the run =="
# DROP on the agent's port, as nfs-resilience.sh section 10 does, for less than
# the grace. That is also less than the ~60s the agent takes to declare a peer
# dead (armDeadPeerDetection), so what this proves is that a short outage costs
# a run nothing. A reattach from the grace period is section 5's.
#
# No docker command while it is blocked: the watcher logs to its own stdout.
WATCH_SH='while true; do
    if out=$(cat /w/marker 2>&1); then echo "$(date +%s) OK $out"; else echo "$(date +%s) ERR $out"; fi
    sleep 1
done'
port2=$(run_field "${CLIENT[2]}" 3)
if drun 2 run -d --name eph-watch -v "$CHECKOUT:/w" alpine:3 sh -c "$WATCH_SH" >/dev/null 2>&1 &&
    wait_output "OK run 2" 20 drun 2 logs eph-watch; then
    ok "a watcher of run 2 reads its mount"
else
    bad "the watcher of run 2 could not read its mount: [$LAST_OUTPUT]"
fi

block=$((GRACE - 5))
if blocked=$(hostdocker exec "$CONTAINER" iptables -A INPUT -p tcp --dport 2222 -j DROP 2>&1); then
    info "black-holing the agent's port for ${block}s"
    sleep "$block"
    hostdocker exec "$CONTAINER" iptables -D INPUT -p tcp --dport 2222 -j DROP 2>/dev/null
    mark=$(date +%s)

    if wait_output '^live$' 60 run_field "${CLIENT[2]}" 2 &&
        outputs "^$port2\$" run_field "${CLIENT[2]}" 3; then
        ok "run 2 is live on the same port, $port2"
    else
        bad "run 2 after the outage: [$(runs)], was port $port2"
    fi
    seen=""
    for _ in $(seq 1 60); do
        seen=$(drun 2 logs eph-watch 2>&1 | awk -v t="$mark" '$1 >= t && $2 == "OK"' | tail -1)
        [ -n "$seen" ] && break
        sleep 1
    done
    if [ "$seen" != "${seen%OK run 2}" ]; then
        ok "the running container still reads its mount: [$seen]"
    else
        bad "the running container stopped reading: [$(drun 2 logs eph-watch 2>&1 | tail -3)]"
    fi
else
    bad "could not block the agent's port: [$blocked]"
fi
drun 2 rm -f eph-watch >/dev/null 2>&1

echo
echo "== 5. an agent restart: the live run reattaches, the killed one is cleaned =="
pid4=$(pid_of 4)
port2=$(run_field "${CLIENT[2]}" 3)
if [ -n "$pid4" ] && kill -9 "$pid4" 2>/dev/null; then
    ok "run 4's client (pid $pid4) was killed"
else
    bad "could not kill run 4's client: pid [$pid4]"
fi
# -t 0: the agent is pid 1, so this is the agent dying with no say in it.
if hostdocker restart -t 0 "$CONTAINER" >/dev/null 2>&1; then
    ok "the workspace was killed and started again"
else
    bad "could not restart the workspace"
fi
wait_parent_dockerd

# The record restores run 2 into its grace period, from the agent's start: it
# must reconnect within it, and does on its next command.
reattached=false
for _ in $(seq 1 30); do
    if reads_own_file 2; then
        reattached=true
        break
    fi
    sleep 2
done
if [ "$reattached" = true ]; then
    ok "run 2 reattached and a new container reads its file through its old volume"
else
    bad "run 2 never read its file after the restart: [$LAST_OUTPUT]"
    sed 's/^/        run 2: /' "$WORK/run-2.log" | tail -15
    dump_workspace_log 40
fi
if outputs "^$port2\$" run_field "${CLIENT[2]}" 3; then
    ok "run 2 kept its port across the restart: $port2"
else
    bad "run 2's port after the restart is [$LAST_OUTPUT], was $port2"
fi

start=$(date +%s)
if wait_run_gone "${CLIENT[4]}"; then
    ok "run 4 and everything carrying its id are gone $(($(date +%s) - start))s after the restart"
else
    bad "run 4 left: $LAST_OUTPUT"
    hostdocker logs "$CONTAINER" 2>&1 | grep -i ephemeral | tail -10 | sed 's/^/        /'
fi
if outputs '^live$' run_field "${CLIENT[2]}" 2; then
    ok "run 2 is live after run 4 was cleaned"
else
    bad "run 2 is [$LAST_OUTPUT] after the restart"
fi
machine_unchanged "after the restart"

echo
echo "== 6. the limit: a fifth run is refused at once, by name =="
for n in 5 6 7; do
    SUDO_PID[$n]=$(start_run "$n")
done
for n in 5 6 7; do
    if wait_run "$n" "${SUDO_PID[$n]}"; then
        ok "run $n has a working docker endpoint"
    else
        bad "run $n never came up"
        sed "s/^/        run $n: /" "$WORK/run-$n.log" | tail -20
    fi
done
count=$(runs | grep -c .)
if [ "$count" -eq "$MAX_CLIENTS" ]; then
    ok "$EPH has $MAX_CLIENTS runs, its limit"
else
    bad "$EPH has $count runs, not $MAX_CLIENTS: [$(runs)]"
fi

# The embedded CLI with no session at its endpoint, which is what a CI job's
# first `docker` command is. It must exit, not wait for a slot.
mkdir -p "$WORK/state-8"
cp "$WORK/key/id_ed25519" "$WORK/key/id_ed25519.pub" "$WORK/state-8/"
start=$(date +%s)
# shellcheck disable=SC2046  # run_env splits on purpose
fifth=$(cd "$WORK" && timeout 60 env $(run_env 8) "$WORK/remote-docker" info 2>&1)
rc=$?
took=$(($(date +%s) - start))
said=$(grep -m1 -E "has [0-9]+ clients" <<<"$fifth")
if [ "$rc" -eq 124 ]; then
    bad "a fifth run was still waiting when the suite gave up after ${took}s"
elif [ "$rc" -eq 0 ]; then
    bad "a fifth run was served past the limit"
elif [ -z "$said" ]; then
    bad "a fifth run failed ($rc) without naming the limit: [$(tail -5 <<<"$fifth")]"
elif [ "$took" -gt 20 ]; then
    bad "a fifth run was refused, but took ${took}s: [$said]"
else
    ok "a fifth run was refused in ${took}s: [$said]"
fi
# shellcheck disable=SC2046  # run_env splits on purpose
(cd "$WORK" && timeout 30 env $(run_env 8) "$WORK/remote-docker" remote stop >/dev/null 2>&1)
count=$(runs | grep -c .)
if [ "$count" -eq "$MAX_CLIENTS" ]; then
    ok "the refused run holds no slot"
else
    bad "$EPH has $count runs after the refusal: [$(runs)]"
fi

echo
echo "== 6b. cleanup releases a run's union, and only that run's =="
# write=ephemeral is a union (ADR 0044), bound into the container by PATH, so
# the run's connection ending keeps it while the container runs. Cleanup removes
# the container (CLEANUP_CONTAINERS), releases the union, and only then may
# remove the cache volume (ADR 0050, steps 1, 3 and 4). Runs 5 and 6 have not
# shared $CHECKOUT yet, so the mode is theirs to choose.
for n in 5 6; do
    CLIENT[$n]=$(client_of "$n")
    if out=$(drun "$n" run -d --name "eph-union-$n" -v "$CHECKOUT:/w:write=ephemeral" alpine:3 sleep 600 2>&1); then
        ok "run $n: a container starts against a write=ephemeral union"
    else
        bad "run $n: a container would not start against a union: [$(tail -3 <<<"$out")]"
        union_diagnostics
        continue
    fi
    if outputs ' /w fuse' drun "$n" exec "eph-union-$n" sh -c 'grep " /w " /proc/mounts'; then
        ok "run $n: the container's /w is a fuse mount"
    else
        bad "run $n: /w is not a fuse mount: [$LAST_OUTPUT]"
    fi
    if outputs "^run $n\$" drun "$n" exec "eph-union-$n" cat /w/marker; then
        ok "run $n: the container reads run $n's file through the union"
    else
        bad "run $n: the union served [$LAST_OUTPUT]"
    fi
    if outputs ' /run/rd-union/' merged_of "${CLIENT[$n]}"; then
        ok "run $n: the workspace has its union mounted: [$LAST_OUTPUT]"
    else
        bad "run $n: no union of client ${CLIENT[$n]} is mounted in the daemon's namespace: [$LAST_OUTPUT]"
    fi
    if outputs "^rd-${CLIENT[$n]}-[^[:space:]]+-cache$" \
        wsdocker "$EPH" volume ls -q --filter "label=$CLIENT_LABEL=${CLIENT[$n]}"; then
        ok "run $n: its cache volume exists"
    else
        bad "run $n: no cache volume among [$LAST_OUTPUT]"
    fi
done

if outputs '"stopping"' curl -s --max-time 30 -X POST --unix-socket "$(run_sock 5)" \
    http://session/_remote-docker/shutdown; then
    ok "run 5 was asked to stop with its union container running"
else
    bad "run 5 did not take the shutdown: [$LAST_OUTPUT]"
fi
union_released 5 "${CLIENT[5]}" "a clean end"

if outputs ' /run/rd-union/' merged_of "${CLIENT[6]}"; then
    ok "run 6's union is still mounted after run 5's cleanup"
else
    bad "run 6's union went with run 5's: [$LAST_OUTPUT]"
fi
if outputs '^run 6$' drun 6 exec eph-union-6 cat /w/marker; then
    ok "run 6's container still reads through its union"
else
    bad "run 6's container cannot read through its union: [$LAST_OUTPUT]"
fi

pid6=$(pid_of 6)
if [ -n "$pid6" ] && kill -9 "$pid6" 2>/dev/null; then
    ok "run 6's client (pid $pid6) was killed with its union container running"
else
    bad "could not kill run 6's client: pid [$pid6]"
fi
union_released 6 "${CLIENT[6]}" "kill -9"

echo
echo "== 7. the machine account, after all of it =="
machine_unchanged "at the end"
if outputs "^the machine's file$" dmachine run --rm -v "$WORK/machine-project:/w" alpine:3 cat /w/marker; then
    ok "$MACHINE still reads its own file"
else
    bad "$MACHINE could not read its file: [$LAST_OUTPUT]"
fi
if outputs "^$MACHINE " hostdocker exec "$CONTAINER" remote-dockerd ephemeral ls; then
    bad "$MACHINE appears among the ephemeral runs: [$LAST_OUTPUT]"
else
    ok "$MACHINE is not an ephemeral run"
fi

echo
echo "== 8. a drop the agent noticed: the run reattaches from its grace =="
# Section 4's outage ends before the agent declares the peer dead (~60s:
# sshd.peerTimeout, keepalives and TCP_USER_TIMEOUT), and section 5 reattaches
# after a restart. This is the case between them: a black hole long enough for
# the agent to end the connection and start the run's grace, and a reconnect
# within it. That needs a grace well above 60s, so this phase gets a workspace
# of its own; the sections above keep their 20s. Its state directory starts
# empty, so no run recorded above is restored into it and counted to the limit.
LONG_GRACE=180
stop_pid "$MACHINE_PID"
MACHINE_PID=""
sudo pkill -f "$WORK/[r]emote-docker" 2>/dev/null
hostdocker rm -f "$CONTAINER" >/dev/null 2>&1
sudo rm -rf "$WORK/wsstate" && mkdir -p "$WORK/wsstate"
workspace_up "$PER_USER_DIND" "$EPH" \
    -e "WORKSPACE_EPHEMERAL_ACCOUNTS=$EPH" \
    -e "WORKSPACE_EPHEMERAL_GRACE=${LONG_GRACE}s" \
    -e "WORKSPACE_EPHEMERAL_MAX_CLIENTS=$MAX_CLIENTS" \
    -e "WORKSPACE_EPHEMERAL_CLEANUP_CONTAINERS=true" \
    "${mode_args[@]}"
if [ "$PER_USER_DIND" = true ] && ! load_image_into_workspace "$IMAGE"; then
    bad "could not load $IMAGE into the new workspace's daemon"
fi

SUDO_PID[9]=$(start_run 9)
if wait_run 9 "${SUDO_PID[9]}"; then
    ok "run 9 has a working docker endpoint against a ${LONG_GRACE}s grace"
else
    bad "run 9 never came up"
    sed "s/^/        run 9: /" "$WORK/run-9.log" | tail -20
    dump_workspace_log 40
    exit 1
fi
CLIENT[9]=$(client_of 9)
drun 9 pull -q alpine:3 >/dev/null 2>&1 || info "could not pre-pull alpine:3 for run 9"
if drun 9 run -d --name eph-watch -v "$CHECKOUT:/w" alpine:3 sh -c "$WATCH_SH" >/dev/null 2>&1 &&
    wait_output "OK run 9" 30 drun 9 logs eph-watch; then
    ok "a watcher of run 9 reads its mount"
else
    bad "the watcher of run 9 could not read its mount: [$LAST_OUTPUT]"
fi
port9=$(run_field "${CLIENT[9]}" 3)
if [[ "$port9" =~ ^[0-9]+$ ]] && outputs '^live$' run_field "${CLIENT[9]}" 2; then
    ok "run ${CLIENT[9]} is live on port $port9"
else
    bad "run 9 is not live with a port: [$(runs)]"
fi

# Observed from the workspace only while blocked: any command through run 9's
# endpoint would redial, and repair what it came to observe (nfs-resilience.sh).
if blocked=$(hostdocker exec "$CONTAINER" iptables -A INPUT -p tcp --dport 2222 -j DROP 2>&1); then
    start=$(date +%s)
    info "black-holing the agent's port until the agent ends run 9's connection"
    noticed=""
    while [ $(($(date +%s) - start)) -lt 150 ]; do
        if [ "$(run_field "${CLIENT[9]}" 2)" = grace ]; then
            noticed=$(($(date +%s) - start))
            break
        fi
        sleep 2
    done
    held=$(run_field "${CLIENT[9]}" 3)
    hostdocker exec "$CONTAINER" iptables -D INPUT -p tcp --dport 2222 -j DROP 2>/dev/null
    blocked_for=$(($(date +%s) - start))
    mark=$(date +%s)

    if [ -n "$noticed" ]; then
        ok "the agent noticed the dead peer and started run 9's grace ${noticed}s into the block"
    else
        bad "run 9 never entered its grace in ${blocked_for}s of black hole: [$(runs)]"
        hostdocker logs "$CONTAINER" 2>&1 | grep -iE "ephemeral|run|connection" | tail -10 | sed 's/^/        /'
    fi
    if [ "$held" = "$port9" ]; then
        ok "run 9 kept port $port9 through its grace"
    else
        bad "run 9's port in its grace is [$held], was $port9"
    fi
    if [ "$blocked_for" -ge "$LONG_GRACE" ]; then
        bad "the block lasted ${blocked_for}s, past the ${LONG_GRACE}s grace, so nothing below is a reattach"
    fi

    # The client redials on its next command, as after section 5's restart.
    reattached=false
    for _ in $(seq 1 30); do
        if reads_own_file 9; then
            reattached=true
            break
        fi
        sleep 2
    done
    if [ "$reattached" = true ]; then
        ok "run 9 reattached $(($(date +%s) - mark))s after the block, and a new container reads its file"
    else
        bad "run 9 never read its file after the block: [$LAST_OUTPUT]"
        sed 's/^/        run 9: /' "$WORK/run-9.log" | tail -15
        dump_workspace_log 40
    fi
    if outputs '^live$' run_field "${CLIENT[9]}" 2 && outputs "^$port9\$" run_field "${CLIENT[9]}" 3; then
        ok "run 9 is live again on the same port, $port9"
    else
        bad "run 9 after the reattach: [$(runs)], was port $port9"
    fi

    # Its mount came back with the reverse forward on the same port. Given its
    # own time: a hard NFS mount retries on its own clock.
    seen=""
    for _ in $(seq 1 120); do
        seen=$(drun 9 logs eph-watch 2>&1 | awk -v t="$mark" '$1 >= t && $2 == "OK"' | tail -1)
        [ -n "$seen" ] && break
        sleep 1
    done
    if [ "$seen" != "${seen%OK run 9}" ]; then
        ok "the container started before the block reads its mount again: [$seen]"
    else
        bad "the container started before the block stopped reading: [$(drun 9 logs eph-watch 2>&1 | tail -3)]"
    fi
else
    bad "could not block the agent's port: [$blocked]"
fi
drun 9 rm -f eph-watch >/dev/null 2>&1

if [ "$FAIL" -ne 0 ]; then
    echo
    echo "== the workspace's ephemeral log =="
    hostdocker logs "$CONTAINER" 2>&1 | grep -i ephemeral | tail -40 | sed 's/^/        /'
    for n in 1 2 3 4 5 6 7 9; do
        echo "== run $n =="
        tail -15 "$WORK/run-$n.log" 2>/dev/null | sed 's/^/        /'
    done
fi

summary
