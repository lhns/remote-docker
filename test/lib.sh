# Shared mechanics for the two integration suites.
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
# Sourced, not executed: it needs the caller's WORK, IMAGE, CONTAINER and
# SSH_PORT.

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
# Never `cmd | grep -q`, which is what this replaces everywhere. grep -q exits
# the instant it matches, so whatever the command still had to write gets
# EPIPE, and Go's runtime turns EPIPE on fd 1 or 2 into a fatal SIGPIPE: exit
# 141. Under `set -o pipefail` the pipeline reports that 141 even though the
# match succeeded, so the assertion fails precisely when it should pass,
# depending only on whether grep was scheduled before the command finished
# writing. A matching line with nothing after it is safe; one with a trailing
# summary line, another row or a log tail is not.
#
# Section 17 has lost two assertions to this, each costing a re-run and naming
# no cause. The earlier one was "fixed" by capturing ls into a variable for the
# error message, which removed the pipeline and, with it, the failure.
#
# The mechanism is Linux-only and so is the evidence: Windows has no SIGPIPE,
# the failed write is silently ignored, and the command still exits 0. It
# cannot be reproduced on a development machine, only in CI.
#
# The command substitution reads to EOF, so there is no reader to close early
# and no pipeline for pipefail to inspect.
# Empty rather than unset, because the suites run under `set -u` and a failure
# message may name it on a path where outputs never ran.
#
# shellcheck disable=SC2034  # read by the suites that source this, not here.
LAST_OUTPUT=""

outputs() {
    local re=$1
    shift
    LAST_OUTPUT=$("$@" 2>&1)
    grep -qE "$re" <<<"$LAST_OUTPUT"
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

# dump_workspace_log prints the tail of the workspace container's log.
dump_workspace_log() {
    echo "== workspace log =="
    hostdocker logs "$CONTAINER" 2>&1 | tail -"${1:-60}" | sed 's/^/        /'
}

# summary prints the totals and sets the exit status.
summary() {
    echo
    echo "=================================="
    echo "  passed: $PASS   failed: $FAIL"
    echo "=================================="
    [ "$FAIL" -eq 0 ]
}
