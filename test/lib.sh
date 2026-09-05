# Shared mechanics for the integration suites.
#
# The suites stay separate on purpose -- one per WORKSPACE_PER_USER_DIND mode,
# each stating its own mode -- and this file is deliberately only the
# MECHANICS. Nothing here decides anything a suite exists to decide.
#
# It exists because the same bug kept landing in one suite and not the other.
# One captured stderr on a failed assertion and the other sent it to /dev/null,
# so a real failure printed nothing after the colon. One broke its wait loop
# when the client died and the other waited six minutes. Those are the lines
# worth sharing; the assertions are not.
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
# BECAUSE it matched, depending only on scheduling. Measured 2026-08-13: a
# producer still writing when grep exits gives 141 every time; Windows ignores
# the failed write. It has not been seen to fire here, and section 17's
# intermittent failures are NOT explained by it. The command substitution reads
# to EOF, so there is no reader to close early.
#
# Empty rather than unset, because the suites run under `set -u` and a failure
# message may name it on a path where outputs never ran.
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

# Every docker command that crosses the proxy is wrapped in a timeout. A
# container whose volume mount never completes would otherwise block forever,
# burning the whole CI budget and reporting nothing about where it stopped.
# A suite sets DOCKER_TIMEOUT before sourcing this to change the budget.
DOCKER_TIMEOUT=${DOCKER_TIMEOUT:-120}
dockert() { timeout "$DOCKER_TIMEOUT" docker "$@"; }

# dockerat runs a docker command against one endpoint, with the same timeout.
#
#   dockerat <socket> <args...>
dockerat() {
    local sock=$1
    shift
    timeout "$DOCKER_TIMEOUT" docker -H "unix://$sock" "$@"
}

# The workspace container lives on the RUNNER's daemon. Once DOCKER_HOST points
# at the workspace, plain `docker` talks to the workspace's daemon instead, so
# anything about the container -- exec, logs, inspect -- has to say which
# daemon it means or it silently looks in the wrong place.
hostdocker() { env -u DOCKER_HOST docker "$@"; }

# build_image builds the workspace image from the repo root, because the image
# builds the agent from source.
#
# The output is kept and printed on failure. It used to go to /dev/null with
# -q, so a build that failed reported the Dockerfile line and NOTHING from the
# compiler -- which cost a CI round trip to learn that the actual error was
# never in the log at all. The build's own words are the whole diagnosis.
build_image() {
    if docker build -t "$IMAGE" -f "$REPO/image/Dockerfile" "$REPO"             >"$WORK/image-build.log" 2>&1; then
        return 0
    fi
    echo "--- image build output ---"
    tail -40 "$WORK/image-build.log" | sed 's/^/        /'
    return 1
}

# build_client builds the client binary into $WORK.
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

# cleanup_suite is the EXIT trap of a suite that runs one workspace container:
# it ends the client pids it is given (empty ones are skipped), removes the
# container and the work directory.
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

# enrol generates a keypair for one account and stages its public half where
# the workspace will find it. The FILENAME becomes the ACCOUNT name, which is
# what a client logs in as; the unix user behind it is `rd-<account>`
# (ADR 0025).
enrol() {
    local account=$1 statedir=$2
    REMOTE_DOCKER_STATE_DIR="$statedir" "$WORK/remote-docker" remote enroll >/dev/null 2>&1
    if [ -f "$statedir/id_ed25519.pub" ]; then
        cp "$statedir/id_ed25519.pub" "$WORK/keys/$account.pub"
        return 0
    fi
    return 1
}

# start_workspace runs the workspace container.
#
# The dind mode is a REQUIRED argument with no default, and that is the whole
# reason this function can be shared at all. Give it a default and the two
# suites stop stating which mode they test -- which is one script with a flag,
# the thing both of their headers explicitly refuse.
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

# wait_provisioned waits for the agent to create the named accounts.
#
# Asked for the UNIX user, `rd-<account>`, which is what useradd made. Asking
# for the account name would wait the full timeout on a workspace that had
# provisioned everything correctly.
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
# workspace's own.
#
# They are different daemons with different image stores, which is easy to
# forget: the suites build the workspace image on the runner, and a per-account
# daemon is started by the WORKSPACE's dockerd (ADR 0019), which has never
# heard of it. Without this it tries Docker Hub and fails with
#
#	pull access denied for remote-docker-workspace, repository does not exist
#
# naming a registry nobody meant to use. Real deployments pull the image from
# one, so this is a CI-only step and not a gap in the product.
load_image_into_workspace() {
    local image=$1
    hostdocker save "$image" | hostdocker exec -i "$CONTAINER" docker load >/dev/null 2>&1
}

# wait_parent_dockerd waits for the workspace's own daemon.
#
# Reports when it never arrives, rather than falling through. A silent timeout
# here makes the next section fail for a reason nothing on screen explains.
wait_parent_dockerd() {
    for _ in $(seq 1 90); do
        hostdocker exec "$CONTAINER" docker info >/dev/null 2>&1 && return 0
        sleep 1
    done
    bad "the workspace's own dockerd never came up"
    return 1
}

# start_session runs a client session in the background, from inside <dir>,
# and prints its pid. Extra VAR=value arguments are added to its environment.
# Watching is on because a read=cached share refuses to run without it (ADR
# 0044); harmless to a section that mounts none.
#
#   start_session <statedir> <user> <endpoint> <log> <dir> [VAR=value...]
#
# exec, so the subshell BECOMES the client. Without it $! is the subshell's
# pid, killing that leaves the client running, and the next session finds the
# endpoint held by a process the suite thinks it stopped.
start_session() {
    local statedir=$1 user=$2 endpoint=$3 log=$4 dir=$5
    shift 5
    (
        cd "$dir" || exit 1
        exec env             REMOTE_DOCKER_STATE_DIR="$statedir"             REMOTE_DOCKER_HOST=127.0.0.1             REMOTE_DOCKER_PORT="$SSH_PORT"             REMOTE_DOCKER_USER="$user"             REMOTE_DOCKER_ENDPOINT="$endpoint"             REMOTE_DOCKER_WATCH=partial             "$@"             "$WORK/remote-docker" remote start --foreground
    ) >"$log" 2>&1 &
    echo $!
}

# wait_endpoint waits for a client endpoint to answer.
#
# The optional second argument is a client pid: if that process dies, the wait
# ends immediately instead of running to the full timeout. Without it a client
# that failed at startup costs the suite its entire patience and then reports a
# timeout, which names the symptom and not the cause.
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

# wait_ready polls a probe container's log for up to <secs> for the READY line
# watchprobe prints once its watch is registered. A change made before that
# proves nothing either way.
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

# wait_url polls <url> for up to <secs> until its body matches <regex>;
# LAST_OUTPUT holds the last answer.
#
#   wait_url <url> <regex> <secs>
wait_url() {
    local url=$1 re=$2 secs=$3 _
    for _ in $(seq 1 "$secs"); do
        outputs "$re" curl -fsS --max-time 3 "$url" && return 0
        sleep 1
    done
    return 1
}

# dump_workspace_log prints the tail of the workspace container's log.
dump_workspace_log() {
    echo "== workspace log =="
    hostdocker logs "$CONTAINER" 2>&1 | tail -"${1:-60}" | sed 's/^/        /'
}

# union_is_fuse asks whether a container's /w is a fuse mount (ADR 0044); the
# caller reports, and LAST_OUTPUT holds what /proc/mounts said.
union_is_fuse() {
    local exec_fn=$1 container=$2
    outputs 'fuse' "$exec_fn" exec "$container" sh -c 'grep " /w " /proc/mounts'
}

# wait_for_content polls a local file for exact content for up to <secs>,
# prints what it last saw (empty for no file), and returns 0 once it matched.
wait_for_content() {
    local path=$1 want=$2 secs=$3 seen="" _
    for _ in $(seq 1 "$secs"); do
        [ -f "$path" ] && seen=$(cat "$path") && [ "$seen" = "$want" ] && break
        sleep 1
    done
    printf '%s' "$seen"
    [ "$seen" = "$want" ]
}

# wait_gone polls for up to <secs> until <path> no longer exists inside a
# container, asked through <exec-fn>; returns 0 once it is gone.
wait_gone() {
    local exec_fn=$1 container=$2 path=$3 secs=$4 _
    for _ in $(seq 1 "$secs"); do
        "$exec_fn" exec "$container" test -e "$path" >/dev/null 2>&1 || return 0
        sleep 1
    done
    return 1
}

# union_diagnostics prints what the workspace logged about its unions.
union_diagnostics() {
    hostdocker logs "$CONTAINER" 2>&1 | grep -iE "union|fuse" | tail -8 | sed 's/^/        /'
}

# summary prints the totals and sets the exit status.
summary() {
    echo
    echo "=================================="
    echo "  passed: $PASS   failed: $FAIL"
    echo "=================================="
    [ "$FAIL" -eq 0 ]
}
