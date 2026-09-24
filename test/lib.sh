# Shared MECHANICS for the test/*.sh suites, and only those: the assertions
# stay in the suites, each of which states the setup it tests (daemon mode,
# clients, workspace image).
#
# Sourced, not executed. The counters and `outputs` need nothing; the rest
# needs the caller's REPO, WORK, IMAGE, CONTAINER and SSH_PORT.

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); echo "  PASS  $*"; }
bad()  { FAIL=$((FAIL + 1)); echo "  FAIL  $*"; }
info() { echo "  ....  $*"; }

# outputs runs a command and reports whether its combined output matches an
# extended regex. The output is left in LAST_OUTPUT for the failure message.
#
#   outputs <regex> <cmd...>
#
# Never `cmd | grep -q` on a live command. grep -q exits at the first match,
# the producer's next write gets EPIPE, and Go turns EPIPE on fd 1 or 2 into a
# fatal SIGPIPE (exit 141), so under `set -o pipefail` the assertion fails
# BECAUSE it matched, depending only on scheduling. The command substitution
# below reads to EOF, so there is no reader to close early.
#
# LAST_OUTPUT is empty rather than unset: the suites run under `set -u` and a
# failure message may name it on a path where outputs never ran.
#
# shellcheck disable=SC2034  # read by the suites that source this, not here.
LAST_OUTPUT=""

# LAST_STATUS is the command's exit status, for a probe that numbers its steps.
outputs() {
    local re=$1
    shift
    LAST_OUTPUT=$("$@" 2>&1)
    # shellcheck disable=SC2034  # read by the suites
    LAST_STATUS=$?
    grep -qE "$re" <<<"$LAST_OUTPUT"
}

# Every docker command that crosses the proxy has a timeout: a volume mount
# that never completes otherwise blocks until CI kills the job, saying nothing.
# A suite sets DOCKER_TIMEOUT before sourcing this to change the budget.
DOCKER_TIMEOUT=${DOCKER_TIMEOUT:-120}
dockert() { timeout "$DOCKER_TIMEOUT" docker "$@"; }

#   dockerat <socket> <args...>
dockerat() {
    local sock=$1
    shift
    timeout "$DOCKER_TIMEOUT" docker -H "unix://$sock" "$@"
}

# The workspace container lives on the RUNNER's daemon, and once DOCKER_HOST
# points at the workspace a plain `docker exec` silently looks in the wrong one.
hostdocker() { env -u DOCKER_HOST docker "$@"; }

# build_image builds the workspace image, agent included, from the repo root.
# Not -q: that reports the failing Dockerfile line and nothing from the compiler.
build_image() {
    if docker build -t "$IMAGE" -f "$REPO/image/Dockerfile" "$REPO" \
        >"$WORK/image-build.log" 2>&1; then
        return 0
    fi
    echo "--- image build output ---"
    tail -40 "$WORK/image-build.log" | sed 's/^/        /'
    return 1
}

build_client() {
    (cd "$REPO/client" && CGO_ENABLED=0 go build -o "$WORK/remote-docker" ./cmd/remote-docker)
}

# build_probe builds one of test/probes into <dest>: static and for Linux, so
# it runs under plain alpine straight off a share, with no image build.
#
#   build_probe <name> <dest>
build_probe() {
    local name=$1 dest=$2
    (cd "$REPO/test/probes" && CGO_ENABLED=0 GOOS=linux go build -o "$dest" "./$name")
}

# cleanup_suite is the EXIT trap of a suite that runs one workspace container.
# Empty pids are skipped.
#
#   cleanup_suite <pid...>
cleanup_suite() {
    local pid
    echo
    echo "== cleanup =="
    for pid in "$@"; do
        [ -n "$pid" ] || continue
        kill "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
    done
    hostdocker rm -f "$CONTAINER" >/dev/null 2>&1
    rm -rf "$WORK"
}

# genkey is separate from enrol because test/two-clients.sh stages two
# machines' keys in ONE account's file itself.
genkey() {
    local statedir=$1
    REMOTE_DOCKER_STATE_DIR="$statedir" "$WORK/remote-docker" remote enroll >/dev/null 2>&1
    [ -f "$statedir/id_ed25519.pub" ]
}

# enrol stages a new key for <account>. The FILENAME is the account name a
# client logs in as; the unix user behind it is `rd-<account>` (ADR 0025).
enrol() {
    local account=$1 statedir=$2
    genkey "$statedir" || return 1
    cp "$statedir/id_ed25519.pub" "$WORK/keys/$account.pub"
}

# ssh_account runs one command as an enrolled account with a STOCK ssh: the
# agent replaces sshd (ADR 0010) and an ordinary client must still get a
# session. stderr is left alone, so a caller that wants it captured says so.
#
#   ssh_account <keyfile> <account> <timeout-seconds> <command> [ssh-option...]
ssh_account() {
    local key=$1 account=$2 secs=$3 command=$4
    shift 4
    timeout "$secs" ssh -i "$key" \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o BatchMode=yes -p "$SSH_PORT" "$@" \
        "$account@127.0.0.1" "$command" </dev/null
}

# start_workspace runs the workspace container. The WORKSPACE_PER_USER_DIND
# value has no default, so every suite states the daemon mode it tests.
start_workspace() {
    local per_user_dind=$1
    shift
    if [ -z "$per_user_dind" ]; then
        bad "start_workspace needs a WORKSPACE_PER_USER_DIND value"
        return 1
    fi

    hostdocker rm -f "$CONTAINER" >/dev/null 2>&1
    hostdocker run -d --name "$CONTAINER" --privileged \
        -p "$SSH_PORT:2222" \
        -v "$WORK/keys:/etc/workspace/authorized_keys.d:ro" \
        -v "$WORK/wsstate:/etc/workspace" \
        -e DOCKER_TLS_CERTDIR= \
        -e "WORKSPACE_PER_USER_DIND=$per_user_dind" \
        "$@" \
        "$IMAGE" >/dev/null
}

# wait_provisioned asks for the UNIX user, `rd-<account>`; asking for the
# account name waits the full timeout on a correctly provisioned workspace.
wait_provisioned() {
    local seconds=${WAIT_PROVISION:-90} account
    for _ in $(seq 1 "$seconds"); do
        local all=true
        for account in "$@"; do
            hostdocker exec "$CONTAINER" id "rd-$account" >/dev/null 2>&1 || all=false
        done
        [ "$all" = true ] && return 0
        sleep 1
    done
    return 1
}

# load_image_into_workspace copies an image from the RUNNER's daemon into the
# workspace's own, which starts each per-account daemon (ADR 0019) from it.
# Without it: `pull access denied for remote-docker-workspace`.
load_image_into_workspace() {
    local image=$1
    hostdocker save "$image" | hostdocker exec -i "$CONTAINER" docker load >/dev/null 2>&1
}

# wait_parent_dockerd REPORTS a daemon that never arrives, or the next section
# fails for a reason nothing on screen explains.
wait_parent_dockerd() {
    for _ in $(seq 1 90); do
        hostdocker exec "$CONTAINER" docker info >/dev/null 2>&1 && return 0
        sleep 1
    done
    bad "the workspace's own dockerd never came up"
    return 1
}

# start_session runs a client session in the background from inside <dir> and
# prints its pid. Watching is on because read=cached refuses to run without it.
#
#   start_session <statedir> <user> <endpoint> <log> <dir> [VAR=value...]
#
# exec, so $! is the client and not a subshell whose death leaves the client
# holding the endpoint.
start_session() {
    local statedir=$1 user=$2 endpoint=$3 log=$4 dir=$5
    shift 5
    (
        cd "$dir" || exit 1
        exec env \
            REMOTE_DOCKER_STATE_DIR="$statedir" \
            REMOTE_DOCKER_HOST=127.0.0.1 \
            REMOTE_DOCKER_PORT="$SSH_PORT" \
            REMOTE_DOCKER_USER="$user" \
            REMOTE_DOCKER_ENDPOINT="$endpoint" \
            REMOTE_DOCKER_WATCH=partial \
            "$@" \
            "$WORK/remote-docker" remote start --foreground
    ) >"$log" 2>&1 &
    echo $!
}

# wait_endpoint waits for a client endpoint to answer, and gives up at once
# when the optional client <pid> dies rather than reporting a timeout.
wait_endpoint() {
    local sock=$1 pid=${2:-}
    for _ in $(seq 1 120); do
        if [ -S "$sock" ] && docker -H "unix://$sock" info >/dev/null 2>&1; then
            return 0
        fi
        if [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null; then
            return 1
        fi
        sleep 2
    done
    return 1
}

# wait_ready waits for watchprobe's READY line: a change made before its watch
# is registered proves nothing either way.
#
#   wait_ready <container> <secs>
wait_ready() {
    local container=$1 secs=$2 _
    for _ in $(seq 1 "$secs"); do
        outputs '^READY' docker logs "$container" && return 0
        sleep 1
    done
    return 1
}

#   wait_url <url> <regex> <secs>
wait_url() {
    local url=$1 re=$2 secs=$3 _
    for _ in $(seq 1 "$secs"); do
        outputs "$re" curl -fsS --max-time 3 "$url" && return 0
        sleep 1
    done
    return 1
}

dump_workspace_log() {
    echo "== workspace log =="
    hostdocker logs "$CONTAINER" 2>&1 | tail -"${1:-60}" | sed 's/^/        /'
}

# union_is_fuse asks whether a container's /w is a fuse mount (ADR 0044); the
# caller reports, and LAST_OUTPUT holds what /proc/mounts said.
union_is_fuse() {
    local exec_fn=$1 container=$2
    outputs ' /w fuse' "$exec_fn" exec "$container" sh -c 'grep " /w " /proc/mounts'
}

# wait_for_content prints what it last saw (empty for no file).
wait_for_content() {
    local path=$1 want=$2 secs=$3 seen="" _
    for _ in $(seq 1 "$secs"); do
        [ -f "$path" ] && seen=$(cat "$path") && [ "$seen" = "$want" ] && break
        sleep 1
    done
    printf '%s' "$seen"
    [ "$seen" = "$want" ]
}

# wait_gone: the directory must still list, so a failed exec or a dead mount
# is not read as a deletion. LAST_OUTPUT holds the last answer.
wait_gone() {
    local exec_fn=$1 container=$2 path=$3 secs=$4 _
    for _ in $(seq 1 "$secs"); do
        # shellcheck disable=SC2016  # expanded by the container's sh
        outputs '^GONE$' "$exec_fn" exec "$container" sh -c \
            'if [ -e "$1" ]; then echo PRESENT; else ls "$(dirname "$1")" >/dev/null && echo GONE; fi' \
            _ "$path" && return 0
        sleep 1
    done
    return 1
}

union_diagnostics() {
    hostdocker logs "$CONTAINER" 2>&1 | grep -iE "union|fuse" | tail -8 | sed 's/^/        /'
}

summary() {
    echo
    echo "=================================="
    echo "  passed: $PASS   failed: $FAIL"
    echo "=================================="
    [ "$FAIL" -eq 0 ]
}
