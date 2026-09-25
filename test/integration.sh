#!/usr/bin/env bash
# End-to-end: a real workspace container, a real client binary, real NFS. The
# kernel NFS client, the dind daemon and the tunnel between them meet only here.
#
# Requires: docker, and a kernel with NFS client support. The `gate` job in
# .github/workflows/integration.yml checks that separately, as a runner fault.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-itest
SSH_PORT=22222
WS_PORT=22280
ACCOUNT=itest

# The docker timeout is lib.sh's default, 120s.

# PIN_SH keeps a container alive only while its mount still works, so the
# container's survival IS the assertion. Only the script, not the `sh -c`: a
# whole command quoted in one variable passes as a single argument.
PIN_SH='while true; do cat /w/marker >/dev/null || exit 1; sleep 1; done'

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

# expect_output runs a container and compares its stdout to a literal. An empty
# capture is its own case: "got []" reads as a wrong answer when the container
# produced no answer at all.
#
#   expect_output <description> <expected> -- <docker run args...>
expect_output() {
    local what=$1 want=$2
    shift 2
    [ "$1" = "--" ] && shift

    local out
    if ! out=$(dockert run "$@" 2>&1); then
        bad "$what: the container failed: $(echo "$out" | tail -3)"
        return 1
    fi
    if [ -z "$out" ]; then
        bad "$what: the container produced no output, so nothing can be concluded"
        return 1
    fi
    if [ "$out" != "$want" ]; then
        bad "$what: got [$out], want [$want]"
        return 1
    fi
    ok "$what"
}

cleanup() { cleanup_suite "${CLIENT_PID:-}"; }
trap cleanup EXIT

echo "== 1. build the workspace image =="
if build_image; then
    ok "image builds"
else
    bad "image build failed"
    exit 1
fi

echo
echo "== 2. build the client =="
if build_client; then
    ok "client builds"
else
    bad "client build failed"
    exit 1
fi

export REMOTE_DOCKER_STATE_DIR="$WORK/state"
export REMOTE_DOCKER_HOST=127.0.0.1
export REMOTE_DOCKER_PORT=$SSH_PORT
export REMOTE_DOCKER_USER=$ACCOUNT
export REMOTE_DOCKER_ENDPOINT="$WORK/docker.sock"
# Idle release (ADR 0015) at the default minute cost a 75s sleep to observe.
export REMOTE_DOCKER_IDLE_TIMEOUT=8s

echo
echo "== 3. enrol this machine =="
enrol_machine "$ACCOUNT" "$REMOTE_DOCKER_STATE_DIR"

echo
echo "== 4. start the workspace =="
# Pinned to the SHARED daemon (ADR 0012); test/per-user-dind.sh covers the
# per-account default (ADR 0019). Several assertions below find the client's
# containers with `docker exec <workspace> docker ps`, which only sees the
# agent's own daemon.
# The WebSocket port is published for section 19's reverse proxy.
# WORKSPACE_DIND_MOUNTS declares paths the workspace daemon resolves, which 9d
# binds (ADR 0041). With the shared daemon the source side is what it sees.
workspace_up false "$ACCOUNT" -p "$WS_PORT:2280" \
    -e WORKSPACE_DIND_MOUNTS=/etc/workspace:/etc/workspace:ro,/etc/hostname:/etc/hostname:ro

echo
echo "== 5. status =="
PROJECT="$WORK/project"
OUTSIDE="$WORK/elsewhere"
mkdir -p "$PROJECT" "$OUTSIDE"
echo "from the project directory" >"$PROJECT/marker"
echo "from an unrelated directory" >"$OUTSIDE/data"

cd "$PROJECT" || exit 1
# A timeout, so a hang reports where it stopped instead of eating the job
# budget (it once caught Close waiting on a context a one-shot command never
# cancels). No session runs yet, so "no session" and exit 1 are correct; the
# account row exists only if the workspace answered.
timeout 90 "$WORK/remote-docker" remote status >"$WORK/status.log" 2>&1
status_rc=$?
if [ "$status_rc" = 1 ] &&
    grep -q "^status  *no session" "$WORK/status.log" &&
    grep -q "tunnel port" "$WORK/status.log"; then
    ok "status reports a verdict and the workspace parameters, and exits 1 when not ready"
    sed 's/^/        /' "$WORK/status.log"
else
    bad "status failed (exit $status_rc, want 1)"
    sed 's/^/        /' "$WORK/status.log"
    hostdocker logs "$CONTAINER" 2>&1 | tail -20
    exit 1
fi

# The agent's build is reported even by a workspace too old to send one,
# because silence there looks the same as a failure to answer.
if grep -qE "^versions .*agent [^ ,]+" "$WORK/status.log"; then
    ok "status reports the agent's version"
else
    bad "status did not report the agent version"
fi

echo
echo "== 6. open a session =="
# --foreground: the suite needs a child it can kill and whose log it can read.
"$WORK/remote-docker" remote start --foreground >"$WORK/up.log" 2>&1 &
CLIENT_PID=$!
endpoint_up "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID" "$WORK/up.log" || exit 1

export DOCKER_HOST="unix://$REMOTE_DOCKER_ENDPOINT"

info "pulling test images through the workspace"
for image in alpine:3 nginx:alpine; do
    timeout 300 docker pull -q "$image" >/dev/null 2>&1 \
        || info "could not pre-pull $image; the test may be slower"
done

echo
echo "== 6a. the workspace's own daemon binds no TCP API =="
# per-user-dind.sh section 14 for the shared daemon, which lives in the
# workspace container's namespace: the one every shell runs in. Both halves,
# because either alone can pass for the wrong reason.
listeners=$(hostdocker exec "$CONTAINER" netstat -lnt 2>&1)
case "$listeners" in
*:2375*|*:2376*)
    bad "SECURITY: the shared daemon is listening on a TCP port: [$listeners]" ;;
*Active*|*Proto*)
    ok "the shared daemon binds no Docker API on 2375 or 2376" ;;
*)
    # No header means netstat did not run, so no listener was measured.
    bad "netstat said nothing in the workspace, so no listener was measured: [$listeners]" ;;
esac

for port in 2375 2376; do
    reach=$(dockert run --rm --network host alpine:3 \
        sh -c "nc -w 2 127.0.0.1 $port </dev/null && echo CONNECTED || echo REFUSED" 2>&1 | tr -d '\015')
    case "$reach" in
    *CONNECTED*) bad "SECURITY: a container on the shared daemon reached a Docker API on $port" ;;
    *REFUSED*)   ok "a container on the shared daemon finds nothing on $port" ;;
    *)           bad "the $port probe said nothing, so it proves nothing: [$reach]" ;;
    esac
done

echo
echo "== 6b. container stdout, with no volume involved =="
# Isolates the attach/stdout path from anything to do with mounts. If this
# fails, no mount test below can be trusted to be telling us about mounts.
expect_output "container stdout reaches the client" "hello-from-container" -- --rm alpine:3 echo hello-from-container

# And the same output read back through the logs endpoint, which is a
# different code path from attach.
if dockert run -d --name itest-echo alpine:3 echo hello-from-logs >/dev/null 2>&1; then
    sleep 2
    logs=$(dockert logs itest-echo 2>&1)
    if [ "$logs" = "hello-from-logs" ]; then
        ok "container logs reach the client"
    else
        bad "logs were lost or altered: got [$logs]"
    fi
    docker rm -f itest-echo >/dev/null 2>&1
else
    bad "could not start the logs test container"
fi

echo
echo "== 6c. a container's exit status reaches the client =="
# The status crosses the HIJACKED stream through the proxy: read as an ordinary
# response, `docker run` exits 0 having printed nothing. The SSH session's exit
# status is a different mechanism, tested in 13b.

#   expect_status <description> <want> <cmd...>
# stdin is the caller's: whether it is attached is half of what 6c tests.
expect_status() {
    local what=$1 want=$2
    shift 2
    local out status
    out=$("$@" 2>&1)
    status=$?
    if [ "$status" -eq "$want" ]; then
        ok "$what"
    else
        bad "$what: exited $status, want $want; output [$out]"
    fi
}

expect_status "a container's non-zero exit reaches the client" 7 \
    dockert run --rm alpine:3 sh -c 'exit 7' </dev/null

expect_status "a container that succeeds exits 0" 0 \
    dockert run --rm alpine:3 true </dev/null

# With -i, so stdin is attached and the client half-closes it when the
# container is done with it. Closing the whole stream instead tears down the
# session carrying the container's output, and the status with it.
printf 'go\n' >"$WORK/exit-stdin"
expect_status "a status survives an attached stdin (-i)" 3 \
    dockert run -i --rm alpine:3 sh -c 'read line; exit 3' <"$WORK/exit-stdin"

# The embedded CLI is where exitCode() actually runs: the runner's docker above
# proves the proxy carries the status, this proves this binary returns it.
expect_status "the embedded CLI returns the container's status" 42 \
    timeout 60 "$WORK/remote-docker" run --rm alpine:3 sh -c 'exit 42' </dev/null

# And says nothing while doing it. cli.StatusError carrying only a code has an
# empty Error(), and main.go prints only a non-empty one; printing regardless
# puts a bare "remote-docker:" after every failing container.
quiet=$(timeout 60 "$WORK/remote-docker" run --rm alpine:3 sh -c 'exit 5' 2>&1 </dev/null)
if [ -z "$quiet" ]; then
    ok "a non-zero container puts nothing on the terminal"
else
    bad "a non-zero container printed [$quiet]"
fi

# Detached, where the status is read back with `docker wait` and never crosses
# an attached stream at all: a number here that the attached cases did not get
# says the daemon recorded it and the attach path lost it.
if cid=$(dockert run -d alpine:3 sh -c 'exit 9' 2>&1); then
    waited=$(dockert wait "$cid" 2>&1)
    if [ "$waited" = "9" ]; then
        ok "docker wait reports a detached container's status"
    else
        bad "docker wait said [$waited], want 9"
    fi
    docker rm -f "$cid" >/dev/null 2>&1
else
    bad "could not start the detached exit-status container: $(echo "$cid" | head -3)"
fi

echo
echo "== 6d. an exec's exit status reaches the client =="
# A second hijacked stream: /exec/<id>/start and /exec/<id>/json, where 6c
# reads attach and wait. A distinct code per case, so a collapse to 1 still
# names which one broke.
if dockert run -d --name itest-exec alpine:3 sleep 300 >/dev/null 2>&1; then
    expect_status "an exec's non-zero exit reaches the client" 11 \
        dockert exec itest-exec sh -c 'exit 11' </dev/null

    expect_status "an exec that succeeds exits 0" 0 \
        dockert exec itest-exec true </dev/null

    printf 'go\n' >"$WORK/exec-stdin"
    expect_status "an exec status survives an attached stdin (-i)" 13 \
        dockert exec -i itest-exec sh -c 'read line; exit 13' <"$WORK/exec-stdin"

    expect_status "the embedded CLI returns an exec's status" 21 \
        timeout 60 "$WORK/remote-docker" exec itest-exec sh -c 'exit 21' </dev/null

    docker rm -f itest-exec >/dev/null 2>&1
else
    bad "could not start the container the exec status assertions run in"
fi

echo
echo "== 6e. an interrupted docker run =="
# Ctrl-C. The number comes from the CONTAINER: the CLI catches every signal
# during a `run` (cli/command/container/run.go, notifyAllSignals), forwards it,
# and the status arrives as a cli.StatusError. Docker's own 128+N mapping is
# unreachable here (errCtxSignalTerminated is unexported).
#
#   run_interrupted <container name> <sh script>
# Leaves the client's status in INTERRUPTED_STATUS; fails if the container
# never started. The signal goes in once it runs: earlier lands mid-create.
run_interrupted() {
    local name=$1 script=$2
    INTERRUPTED_STATUS=

    "$WORK/remote-docker" run --rm --name "$name" alpine:3 sh -c "$script" \
        >"$WORK/$name.log" 2>&1 </dev/null &
    local pid=$!

    if ! wait_output '^true$' 60 docker inspect -f '{{.State.Running}}' "$name"; then
        bad "$name never started: $(head -3 "$WORK/$name.log")"
        kill -KILL "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
        docker rm -f "$name" >/dev/null 2>&1
        return 1
    fi
    # Running is not ready: the trap is installed a moment later, and a signal
    # before it hits the pid-1 rule of the third case below. The scripts use
    # `sleep 60 & wait` because a shell defers a trap until a foreground
    # command returns.
    sleep 2

    # A client that catches the signal and then waits forever would hang the
    # suite rather than failing it.
    (
        sleep 90
        kill -KILL "$pid" 2>/dev/null
    ) &
    local watchdog=$!

    kill -INT "$pid" 2>/dev/null
    wait "$pid"
    INTERRUPTED_STATUS=$?

    kill "$watchdog" 2>/dev/null
    wait "$watchdog" 2>/dev/null
    docker rm -f "$name" >/dev/null 2>&1
    return 0
}

# The container re-raises the signal with its handler removed, so the daemon
# derives 128+2 itself and nothing on this side invents the number.
if run_interrupted itest-interrupt-signal 'trap "trap - INT; kill -INT $$" INT; sleep 60 & wait'; then
    if [ "$INTERRUPTED_STATUS" -eq 130 ]; then
        ok "an interrupted docker run exits 130"
    else
        bad "an interrupted docker run exited $INTERRUPTED_STATUS, want 130; output [$(head -3 "$WORK/itest-interrupt-signal.log")]"
    fi
fi

# The same through a status the container chooses, with no 128+N anywhere in
# it: a client that only ever reports 130 passes the case above and fails this.
if run_interrupted itest-interrupt-status 'trap "exit 77" INT; sleep 60 & wait'; then
    if [ "$INTERRUPTED_STATUS" -eq 77 ]; then
        ok "an interrupted container's own status reaches the client"
    else
        bad "an interrupted container's status arrived as $INTERRUPTED_STATUS, want 77; output [$(head -3 "$WORK/itest-interrupt-status.log")]"
    fi
fi

# A container whose pid 1 has no handler IGNORES the signal (the kernel's rule
# for pid 1; stock docker does the same), so it runs to completion and its 0 is
# reported. A `sleep 60` here ran its full minute (run 34242976755).
if run_interrupted itest-interrupt-ignored 'sleep 10'; then
    if [ "$INTERRUPTED_STATUS" -eq 0 ]; then
        ok "a container that ignores the signal runs on, and its status is reported"
    else
        bad "a container that ignores the signal ended as $INTERRUPTED_STATUS, want 0; output [$(head -3 "$WORK/itest-interrupt-ignored.log")]"
    fi
fi

echo
echo "== 7. a bind mount under the working directory =="
expect_output "the container read this machine's file through the tunnel" "from the project directory" -- --rm -v "$PROJECT:/w" alpine:3 cat /w/marker

echo
echo "== 8. a bind mount OUTSIDE the working directory =="
expect_output "an unrelated local directory resolved" "from an unrelated directory" -- --rm -v "$OUTSIDE:/d" alpine:3 cat /d/data

echo
echo "== 9. writes reach this machine =="
if dockert run --rm -v "$PROJECT:/w" alpine:3 sh -c 'echo written-by-container > /w/out' 2>&1; then
    if [ -f "$PROJECT/out" ] && [ "$(cat "$PROJECT/out")" = "written-by-container" ]; then
        ok "the container's write landed on this filesystem"
    else
        bad "the write did not appear locally"
    fi
else
    bad "the write failed"
fi

echo
echo "== 9b. a read-only bind mount stays read-only =="
# Every bind becomes an NFS volume over a read-write export (ADR 0006), so the
# read-only flag surviving the rewrite is ALL that protects this machine's
# files. Section 9 is the control. Both spellings, because they take different
# rewriter paths: `-v` is a Binds string carried verbatim, `--mount` a Mounts
# object whose ReadOnly must survive the type changing from bind to volume.
before=$(ls "$PROJECT" | sort | tr '\n' ' ')

# A refusal counts only as EROFS: a container that failed to start refuses
# every write too.
if out=$(dockert run --rm -v "$PROJECT:/w:ro" alpine:3 \
        sh -c 'echo nope > /w/ro-v' 2>&1); then
    bad "a container wrote through a -v ...:ro mount"
elif [[ $out == *"Read-only file system"* ]]; then
    ok "-v with :ro refused the write"
else
    bad "the -v :ro container failed for some other reason: [$out]"
fi

if out=$(dockert run --rm --mount "type=bind,source=$PROJECT,target=/w,readonly" alpine:3 \
        sh -c 'echo nope > /w/ro-mount' 2>&1); then
    bad "a container wrote through a --mount readonly mount"
elif [[ $out == *"Read-only file system"* ]]; then
    ok "--mount with readonly refused the write"
else
    bad "the --mount readonly container failed for some other reason: [$out]"
fi

# The assertion that matters. A refused command proves the daemon reported an
# error; only the directory proves nothing reached this machine.
after=$(ls "$PROJECT" | sort | tr '\n' ' ')
if [ "$before" = "$after" ]; then
    ok "nothing new appeared on this machine"
else
    bad "the directory changed under a read-only mount: [$before] -> [$after]"
fi

# And read-only means readable. A mount that refuses writes by being broken
# would pass everything above.
if out=$(dockert run --rm -v "$PROJECT:/w:ro" alpine:3 cat /w/marker 2>&1) &&
    echo "$out" | grep -q "from the project directory"; then
    ok "a read-only mount is still readable"
else
    bad "a read-only mount could not be read: $(echo "$out" | tail -2 | tr '\n' ' ')"
fi

echo
echo "== 9c. a single file can be bind mounted =="
# A file has no directory to export, so the client exports a SYNTHESISED
# directory holding only it and names it as a volume subpath (ADR 0039).
mkdir -p "$PROJECT/conf"
echo "the file the container asked for" >"$PROJECT/conf/wanted.conf"
echo "TOKEN=secret" >"$PROJECT/conf/sibling.env"

# One run, three questions: the target is a FILE (a volume mounted whole puts a
# directory there), it holds this machine's content, and its sibling stayed out.
out=$(dockert run --rm -v "$PROJECT/conf/wanted.conf:/etc/app.conf" alpine:3 sh -c '
    test -f /etc/app.conf && echo is-a-file
    cat /etc/app.conf
    cat /etc/sibling.env 2>/dev/null' 2>&1)

if echo "$out" | grep -q "is-a-file"; then
    ok "the target is a file, not a directory"
else
    bad "the target is not a regular file: $(echo "$out" | head -2 | tr '\n' ' ')"
fi
if echo "$out" | grep -q "the file the container asked for"; then
    ok "a single file mounted at the path the container asked for"
else
    bad "a single-file bind did not read back: $(echo "$out" | head -2 | tr '\n' ' ')"
fi
if echo "$out" | grep -q "TOKEN=secret"; then
    bad "a file beside the exported one was reachable"
else
    ok "a sibling of the exported file did not come with it"
fi

# An edit here reaches the container, which is the case people actually want:
# edit nginx.conf, reload the service.
echo "edited after the mount" >"$PROJECT/conf/wanted.conf"
if out=$(dockert run --rm -v "$PROJECT/conf/wanted.conf:/etc/app.conf" alpine:3 \
    cat /etc/app.conf 2>&1) && echo "$out" | grep -q "edited after the mount"; then
    ok "an edit on this machine is visible through a single-file mount"
else
    bad "the mount served a stale file: $(echo "$out" | head -2 | tr '\n' ' ')"
fi

# Read-only has to survive this path too, for the reason section 9b gives: the
# export behind it is read-write.
if out=$(dockert run --rm -v "$PROJECT/conf/wanted.conf:/etc/app.conf:ro" alpine:3 \
        sh -c 'echo nope > /etc/app.conf' 2>&1); then
    bad "a container wrote through a read-only single-file mount"
elif [[ $out == *"Read-only file system"* ]]; then
    ok "a read-only single-file mount refused the write"
else
    bad "the read-only single-file container failed for some other reason: [$out]"
fi
if [ "$(cat "$PROJECT/conf/wanted.conf")" = "edited after the mount" ]; then
    ok "the file on this machine is unchanged"
else
    bad "a read-only single-file mount let the file be rewritten"
fi

# The --mount spelling reaches the rewriter differently: a JSON object whose
# type changes from bind to volume, rather than a string that leaves Binds
# entirely.
if out=$(dockert run --rm --mount "type=bind,source=$PROJECT/conf/wanted.conf,target=/etc/app.conf" \
    alpine:3 cat /etc/app.conf 2>&1) && echo "$out" | grep -q "edited after the mount"; then
    ok "--mount of a single file works too"
else
    bad "--mount of a single file failed: $(echo "$out" | head -2 | tr '\n' ' ')"
fi

echo
echo "== 9d. a bind may name a path the workspace owns =="
# A declared path is resolved by the DAEMON, not exported from here (ADR 0041).
# /etc/workspace exists only in the workspace container, so a read proves it.
if out=$(dockert run --rm -v /etc/workspace:/w:ro alpine:3 ls /w 2>&1) &&
    echo "$out" | grep -q "authorized_keys.d"; then
    ok "a declared path was resolved by the workspace"
else
    bad "a declared path did not resolve: $(echo "$out" | head -2 | tr '\n' ' ')"
fi

# And the other half of the rule: THIS machine wins when it has the path too.
# /etc/hostname is declared as well and exists on the runner, so the container
# must read the RUNNER's file, not the workspace container's.
runner_host=$(cat /etc/hostname)
ws_host=$(hostdocker exec "$CONTAINER" cat /etc/hostname 2>/dev/null)
if [ "$runner_host" = "$ws_host" ]; then
    info "the runner and the workspace report the same hostname; skipping the tie-break"
elif out=$(dockert run --rm -v /etc/hostname:/x:ro alpine:3 cat /x 2>&1) &&
    echo "$out" | grep -q "^$runner_host$"; then
    ok "a path this machine also has was exported from here, not the workspace"
else
    bad "the tie-break read [$(echo "$out" | head -1)], want the runner's [$runner_host]"
fi

echo
echo "== 10. a published port is reachable here =="
dockert run -d --name itest-web -p 18080:80 -v "$PROJECT:/usr/share/nginx/html" nginx:alpine >/dev/null 2>&1
echo "<h1>served from the client</h1>" >"$PROJECT/index.html"

if wait_url http://127.0.0.1:18080/ "served from the client" 45; then
    ok "the published port was forwarded automatically and served this machine's file"
else
    bad "the published port never became reachable"
    sed 's/^/        /' "$WORK/up.log" | tail -20
fi

# And the workspace published somewhere else entirely, which is what stops two
# accounts on one daemon colliding over 18080 (ADR 0008). The number above is
# this machine's; this one is the daemon's own choice.
published=$(dockert port itest-web 80/tcp 2>/dev/null | head -1)
case "$published" in
*:18080)
    bad "the workspace bound 18080 itself, so a second account asking for it still collides" ;;
*:[0-9]*)
    ok "the workspace published ${published##*:}, not the 18080 that was asked for" ;;
*)
    bad "could not read the workspace-side port: [$published]" ;;
esac

# The daemon no longer refuses a clash, so the client does, in the daemon's
# wording. Two accounts colliding cannot be shown from one client; what makes it
# impossible is that no requested number is bound on the workspace, proven above.
if out=$(dockert run -d --name itest-web2 -p 18080:80 nginx:alpine 2>&1); then
    bad "a second container took a local port this session already forwards"
    docker rm -f itest-web2 >/dev/null 2>&1
else
    case "$out" in
    *"port is already allocated"*)
        ok "a second container asking for 18080 is refused, as the daemon would" ;;
    *)
        bad "it was refused for the wrong reason: $(echo "$out" | tail -1)" ;;
    esac
fi
docker rm -f itest-web >/dev/null 2>&1

# One container port published twice, which is the case that cannot be paired
# back and does not need to be: both assigned ports front port 80, so both
# numbers work whichever way round they were matched.
if ! twice=$(dockert run -d --name itest-twice -p 18082:80 -p 18083:80 \
    -v "$PROJECT:/usr/share/nginx/html" nginx:alpine 2>&1); then
    # head, not tail: docker's last line is "Run 'docker run --help'...".
    bad "a container publishing one port twice was refused: $(echo "$twice" | head -2 | tr '\n' ' ')"
fi

for port in 18082 18083; do
    if wait_url "http://127.0.0.1:$port/" "served from the client" 45; then
        ok "one container port published twice is reachable at $port"
    else
        bad "$port never became reachable"
        # What the daemon published against what this machine opened.
        dockert port itest-twice 2>&1 | sed 's/^/        published: /'
        dockert ps --all --filter name=itest-twice --format '{{.Status}}' 2>&1 | sed 's/^/        state: /'
        sed 's/^/        /' "$WORK/up.log" | tail -8
    fi
done
docker rm -f itest-twice >/dev/null 2>&1

echo
echo "== 10b. a published UDP port answers here =="
# UDP over SSH (ADR 0038): a datagram sent to the port asked for HERE reaches
# the container and its answer comes back. The probe is both ends because
# nothing in alpine echoes UDP and the two netcats disagree about -u and -w.
if ! build_probe udpecho "$PROJECT/udpecho"; then
    bad "could not build the udp echo probe"
elif ! dockert run -d --name itest-udp -p 15353:5353/udp \
    -v "$PROJECT:/probe:ro" alpine:3 /probe/udpecho :5353 >"$WORK/udp-run.log" 2>&1; then
    bad "the udp echo container did not start: $(tail -2 "$WORK/udp-run.log" | tr '\n' ' ')"
else
    # The daemon publishes where it likes (ADR 0008): the number above is this
    # machine's, and these two must not be the same.
    published=$(dockert port itest-udp 5353/udp 2>/dev/null | head -1)
    case "$published" in
    *:15353) bad "the workspace bound 15353 itself" ;;
    *:[0-9]*) ok "the workspace published ${published##*:}/udp, not the 15353 asked for" ;;
    *) bad "could not read the workspace-side udp port: [$published]" ;;
    esac

    # Retried rather than sent once: the forward opens when the ports manager
    # next reconciles, and the probe has to be listening by then.
    if wait_output '^through the tunnel$' 45 "$PROJECT/udpecho" send 127.0.0.1:15353 "through the tunnel"; then
        ok "a datagram reached the container and its answer came back"
    else
        bad "no answer came back from 127.0.0.1:15353: [$LAST_OUTPUT]"
        dockert logs itest-udp 2>&1 | sed 's/^/        probe: /' | tail -5
        sed 's/^/        /' "$WORK/up.log" | tail -8
    fi

    docker rm -f itest-udp >/dev/null 2>&1
fi
rm -f "$PROJECT/udpecho"

echo
echo "== 11. named volumes are left alone =="
docker volume create itest-named >/dev/null 2>&1
if out=$(dockert run --rm -v itest-named:/data alpine:3 sh -c 'echo ok > /data/f && cat /data/f' 2>&1); then
    if [ "$out" = "ok" ]; then
        ok "a named volume still behaves as a named volume"
    else
        bad "unexpected content: $out"
    fi
else
    bad "named volume container failed: $out"
fi
docker volume rm itest-named >/dev/null 2>&1

echo
echo "== 11b. does a file watcher see client-side changes? =="
# NFS carries no change notification, so a watcher in the container may see
# nothing while the file is plainly there (ADR 0014). Polling is the control:
# if it sees nothing, the mount is broken and the inotify result means nothing.
# Recorded rather than demanded, so inotify starting to work would say so.
WATCHDIR="$WORK/watched"
mkdir -p "$WATCHDIR"

# A static binary on the share rather than an image build, which keeps this
# about watching and avoids shipping $WORK (the private key, a live socket) as a
# build context. Built once here and copied onto the share by each user.
WATCHPROBE="$WORK/watchprobe"
if build_probe watchprobe "$WATCHPROBE" && cp "$WATCHPROBE" "$PROJECT/watchprobe"; then
    # The error is NOT swallowed: a probe the share would not execute
    # otherwise leaves an empty log and no explanation.
    if ! dockert run -d --name itest-watch \
        -v "$PROJECT:/probe:ro" \
        -v "$WATCHDIR:/data" \
        alpine:3 /probe/watchprobe /data >"$WORK/watch-run.log" 2>&1; then
        bad "the watch probe container would not start"
        sed 's/^/        /' "$WORK/watch-run.log"
        probe=""
    else
        wait_ready itest-watch 30

        echo "written on the client" >"$WATCHDIR/created-after-watch.txt"

        timeout 60 docker wait itest-watch >/dev/null 2>&1
        probe=$(docker logs itest-watch 2>&1)
    fi
    docker rm -f itest-watch >/dev/null 2>&1
    rm -f "$PROJECT/watchprobe"

    echo "        $(echo "$probe" | grep '^RESULT' || echo 'RESULT missing')"

    # An empty capture is NOT a negative result: a probe that never ran would
    # otherwise be reported as "the mount itself is not working".
    if [ -z "$probe" ]; then
        bad "the watch probe produced no output; nothing below can be concluded"
    elif echo "$probe" | grep -q "POLL created-after-watch.txt"; then
        ok "a polling watcher sees client-side changes"
    else
        bad "a polling watcher saw nothing -- the mount itself is not working"
        echo "$probe" | sed 's/^/        /' | tail -10
    fi

    if echo "$probe" | grep -q "INOTIFY.*created-after-watch.txt"; then
        ok "inotify FIRES for client-side changes (better than expected -- update ADR 0014)"
    else
        ok "inotify does not fire for client-side changes (expected; ADR 0014)"
    fi
else
    bad "could not build the watch probe"
fi

echo
echo "== 11d. which syscall makes a container's watcher fire? (ADR 0014 spike) =="
# Which syscall by the AGENT, on the same file inside the workspace, makes the
# container's watcher fire. Linux cannot inject a synthetic inotify event
# (fanotify(7)), so the only mechanism is a real VFS operation. One file per
# primitive, so correlation is by name, not timing. Only the setup is asserted:
# the point is to record the matrix.
POKEDIR="$WORK/poked"
mkdir -p "$POKEDIR"

if [ -x "$WATCHPROBE" ] && cp "$WATCHPROBE" "$PROJECT/watchprobe" && build_probe pokeprobe "$PROJECT/pokeprobe"; then

    # Pre-created where the primitive needs an existing file; 'create' and
    # 'unlink' are about a file just appearing or going, so handled below.
    for p in openclose mtime touch dirmtime procroot; do
        echo "before the watch" >"$POKEDIR/poke-$p.txt"
    done
    echo "to be deleted" >"$POKEDIR/poke-unlink.txt"

    if ! dockert run -d --name itest-poke \
            -v "$PROJECT:/probe:ro" \
            -v "$POKEDIR:/data" \
            alpine:3 /probe/watchprobe -timeout 90s /data >"$WORK/poke-run.log" 2>&1; then
        bad "the poke probe container would not start"
        sed 's/^/        /' "$WORK/poke-run.log"
    else
        if wait_ready itest-poke 30; then
            ok "the watcher is established"
        else
            bad "the watcher never reported READY"
        fi

        # dockerd's local driver mounts each rd-<id> volume once and
        # bind-mounts it into every container, sharing the superblock and so
        # the inode an inotify mark sits on: poking the mountpoint should reach
        # the container's watcher without entering any namespace.
        vol=$(docker inspect itest-poke \
            --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' 2>/dev/null)
        mp=$(docker volume inspect "$vol" --format '{{.Mountpoint}}' 2>/dev/null)
        pid=$(docker inspect itest-poke --format '{{.State.Pid}}' 2>/dev/null)
        info "volume=$vol mountpoint=$mp container pid=$pid"

        if [ -z "$mp" ]; then
            bad "could not resolve the volume mountpoint -- the rest of the matrix cannot run"
        else
            hostdocker cp "$PROJECT/pokeprobe" "$CONTAINER:/pokeprobe" >/dev/null 2>&1

            # A different st_dev means separate superblocks and inodes, and no
            # poke through the mountpoint could ever notify the container.
            dev_ws=$(hostdocker exec "$CONTAINER" /pokeprobe stat "$mp/poke-openclose.txt" 2>&1)
            dev_ct=$(docker exec itest-poke /probe/pokeprobe stat /data/poke-openclose.txt 2>&1)
            info "workspace: $dev_ws"
            info "container: $dev_ct"
            # dev AND ino, because an inotify mark lives on the inode.
            ids_ws=${dev_ws##*ok dev=}
            ids_ct=${dev_ct##*ok dev=}
            if [ "$ids_ws" != "$dev_ws" ] && [ "$ids_ws" = "$ids_ct" ]; then
                ok "the volume mountpoint and the container see the same inode"
            else
                bad "the volume mountpoint and the container see different inodes, so a poke through one misses the other's watch"
            fi

            for p in openclose mtime touch dirmtime; do
                out=$(hostdocker exec "$CONTAINER" /pokeprobe "$p" "$mp/poke-$p.txt" 2>&1)
                info "$out"
            done

            # create: the file must appear on the CLIENT first, so the
            # question is whether the container's dcache still holds a
            # negative dentry and open(O_CREAT) therefore fires IN_CREATE.
            echo "created on the client" >"$POKEDIR/poke-create.txt"
            info "$(hostdocker exec "$CONTAINER" /pokeprobe create "$mp/poke-create.txt" 2>&1)"

            # unlink: gone on the client, so the REMOVE has nothing to remove.
            # The least certain row; it decides whether deletes are representable.
            rm -f "$POKEDIR/poke-unlink.txt"
            info "$(hostdocker exec "$CONTAINER" /pokeprobe unlink "$mp/poke-unlink.txt" 2>&1)"

            # The fallback route, in case the superblock assumption fails.
            if [ -n "$pid" ] && [ "$pid" != "0" ]; then
                info "$(hostdocker exec "$CONTAINER" /pokeprobe openclose "/proc/$pid/root/data/poke-procroot.txt" 2>&1)"
            fi
        fi

        timeout 120 docker wait itest-poke >/dev/null 2>&1
        poke=$(docker logs itest-poke 2>&1)
        docker rm -f itest-poke >/dev/null 2>&1

        echo
        echo "        --- poke matrix: which primitive produced which events ---"
        for p in openclose mtime touch create unlink dirmtime procroot; do
            seen=$(echo "$poke" | grep "^INOTIFY .* poke-$p\.txt$" | awk '{print $2}' | sort -u | tr '\n' ',' | sed 's/,$//')
            # dirmtime acts on the watched directory, reported under its own
            # basename, which is what the coarse fallback relies on.
            if [ "$p" = dirmtime ]; then
                seen=$(echo "$poke" | grep "^INOTIFY .* data/$" | awk '{print $2}' | sort -u | tr '\n' ',' | sed 's/,$//')
            fi
            printf '        POKE-MATRIX %-10s %s\n' "$p" "${seen:-<nothing>}"
        done
        echo
        echo "        $(echo "$poke" | grep '^RESULT' || echo 'RESULT missing')"

        # Near-certain from kernel source: a miss means the experiment is wrong.
        if grep -q "^INOTIFY .*IN_CLOSE_WRITE.* poke-openclose\.txt$" <<<"$poke"; then
            ok "open(O_WRONLY)+close() reaches the container's watcher"
        else
            bad "open(O_WRONLY)+close() did NOT reach the watcher"
        fi

        # The asymmetry replay rests on (ADR 0016): utimensat with atime
        # omitted is IN_MODIFY, with both times set it is IN_ATTRIB.
        if grep -q "^INOTIFY IN_MODIFY .*poke-mtime\.txt$" <<<"$poke"; then
            ok "utimensat(atime=UTIME_OMIT) fires IN_MODIFY"
        else
            bad "utimensat(atime=UTIME_OMIT) did not fire IN_MODIFY: [$(grep 'poke-mtime' <<<"$poke" | tr -s '[:space:]' ' ')]"
        fi
        if grep -q "^INOTIFY IN_ATTRIB .*poke-touch\.txt$" <<<"$poke"; then
            ok "utimensat with both times set fires IN_ATTRIB"
        else
            bad "utimensat with both times set did not fire IN_ATTRIB: [$(grep 'poke-touch' <<<"$poke" | tr -s '[:space:]' ' ')]"
        fi
    fi
    rm -f "$PROJECT/watchprobe" "$PROJECT/pokeprobe"
else
    bad "could not build the probes"
fi

echo
echo "== 11c. idle release, and what must survive it =="
# A release happens when nothing depends on us, and does NOT while a container
# does. In that order: run first, the container would pin the session from the
# start and leave the reconnect path (ADR 0015) unexercised.

# (a) nothing running -> the connection must be released and reopen on demand.
# 12s against the 8s timer: a 1.5x margin, no more, since every run pays it.
sleep 12
expect_output "the client reconnects after an idle release" "after-idle" -- --rm alpine:3 echo after-idle

# (b) a container holding one of our volumes must pin the connection: dropping
# the tunnel under its NFS mount gives it EIO. PIN_SH makes survival the check.
if dockert run -d --name itest-idle -v "$PROJECT:/w" alpine:3 sh -c "$PIN_SH" >/dev/null 2>&1; then

    sleep 12

    if [ "$(docker inspect -f '{{.State.Running}}' itest-idle 2>/dev/null)" = "true" ]; then
        ok "a container holding one of our volumes kept working across an idle period"
    else
        bad "the container died during the idle period -- its mount was dropped"
        docker logs itest-idle 2>&1 | tail -5 | sed 's/^/        /'
    fi

    if out=$(dockert run --rm -v "$PROJECT:/w" alpine:3 cat /w/marker 2>&1); then
        if [ "$out" = "from the project directory" ]; then
            ok "the client still works after an idle period"
        else
            bad "unexpected content after idle: $out"
        fi
    else
        bad "the client failed after an idle period: $out"
    fi
    docker rm -f itest-idle >/dev/null 2>&1
else
    bad "could not start the idle-test container"
fi

echo
echo "== 11e. one account cannot bind another's NFS port =="
# ADR 0010: a second account asks for the FIRST account's reverse port and
# must be refused.
OTHER=itest2
cp "$REMOTE_DOCKER_STATE_DIR/id_ed25519.pub" "$WORK/keys/$OTHER.pub"

if ! wait_provisioned "$OTHER"; then
    bad "the second account was never provisioned"
    hostdocker logs "$CONTAINER" 2>&1 | tail -15 | sed 's/^/        /' 
else
    first_port=$(tunnel_port <"$WORK/status.log")
    if [ -z "$first_port" ]; then
        bad "could not determine the first account's port"
    else
        # -R on the OTHER account, targeting the FIRST account's port.
        hijack=$(timeout 30 ssh -i "$REMOTE_DOCKER_STATE_DIR/id_ed25519" \
            -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            -o ExitOnForwardFailure=yes -o BatchMode=yes \
            -p "$SSH_PORT" -N -R "127.0.0.1:$first_port:127.0.0.1:1" \
            "$OTHER@127.0.0.1" 2>&1 </dev/null; echo "rc=$?")

        # -N holds an ACCEPTED forward open until the timeout, so success is
        # rc=124 and never rc=0; only ssh's own refusal counts as a pass.
        case "$hijack" in
        *"remote port forwarding failed"*rc=255)
            ok "one account cannot bind another's NFS port" ;;
        *rc=124)
            bad "SECURITY: $OTHER bound $ACCOUNT's NFS port $first_port: [$hijack]" ;;
        *)
            bad "the bind attempt ended without a refusal: [$hijack]" ;;
        esac

        # And cannot DIAL it. With the shared daemon (ADR 0012) the port is
        # reachable from any account's session, and what answers is an NFS
        # export with AuthFlavorNull: read and write access to that person's
        # files. The forward must be USED: ssh opens the local listener at once
        # and asks for the channel only on connect, so merely starting `ssh -L`
        # passes whatever the server decides.
        local_port=$((first_port + 5000))
        timeout 30 ssh -i "$REMOTE_DOCKER_STATE_DIR/id_ed25519" \
            -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            -o ExitOnForwardFailure=yes -o BatchMode=yes \
            -p "$SSH_PORT" -N -L "127.0.0.1:$local_port:127.0.0.1:$first_port" \
            "$OTHER@127.0.0.1" >"$WORK/reach.log" 2>&1 </dev/null &
        reach_pid=$!
        sleep 2
        timeout 5 bash -c "exec 3<>/dev/tcp/127.0.0.1/$local_port" 2>/dev/null
        sleep 1
        kill "$reach_pid" 2>/dev/null
        wait "$reach_pid" 2>/dev/null

        if grep -qi "administratively prohibited\|open failed" "$WORK/reach.log"; then
            ok "one account cannot dial another's NFS port"
        else
            bad "SECURITY: $OTHER reached $ACCOUNT's NFS port $first_port"
            sed 's/^/        /' "$WORK/reach.log"
        fi

        # That covers ssh -L, not a shell: with the shared daemon the export
        # binds in the agent's namespace, where every shell runs, and a socket
        # there asks no forwarding policy. A daemon per account (ADR 0019) binds
        # in its own namespace, asserted by test/per-user-dind.sh. Reported, not
        # asserted: it follows from this mode, and the threat model records it.
        shell_reach=$(ssh_account "$REMOTE_DOCKER_STATE_DIR/id_ed25519" "$OTHER" 60 \
            "nc -w 2 127.0.0.1 $first_port </dev/null && echo CONNECTED || echo REFUSED" \
            2>/dev/null | tr -d '\015')
        case "$shell_reach" in
        *CONNECTED*)
            info "shared daemon: $OTHER's shell reaches $ACCOUNT's export on $first_port (ADR 0012)" ;;
        *REFUSED*)
            info "shared daemon: $OTHER's shell cannot reach $first_port" ;;
        *)
            info "shared daemon: the shell probe said nothing: [$shell_reach]" ;;
        esac
    fi
fi

echo
echo "== 12. docker compose =="
# Compose speaks the Engine API and never shells out to `docker`, which is why
# ADR 0005 translates at the API. It expands ./html to an absolute path on THIS
# machine (docker/compose#8484), meaningless remotely until the proxy rewrites it.
mkdir -p "$PROJECT/html"
echo "served by compose" >"$PROJECT/html/index.html"
cat >"$PROJECT/compose.yaml" <<'COMPOSE'
services:
  web:
    image: nginx:alpine
    ports:
      - "18081:80"
    volumes:
      - ./html:/usr/share/nginx/html:ro
COMPOSE

if timeout 180 docker compose -f "$PROJECT/compose.yaml" up -d >"$WORK/compose.log" 2>&1; then
    ok "compose brought the stack up through the proxy"

    if wait_url http://127.0.0.1:18081/ "served by compose" 45; then
        ok "a compose relative bind resolved and its port was forwarded"
    else
        bad "the compose service never served this machine's file"
        sed 's/^/        /' "$WORK/compose.log" | tail -20
    fi

    timeout 120 docker compose -f "$PROJECT/compose.yaml" down -v >/dev/null 2>&1 \
        && ok "compose tore the stack down" \
        || bad "compose down failed"
else
    bad "compose up failed"
    sed 's/^/        /' "$WORK/compose.log" | tail -20
fi

echo
echo "== 12b. one compose service reaching another =="
# Container-to-container traffic never touches this client, but every bind is
# rewritten and every port forwarded, so "did we disturb the network" gets an
# answer. One test, four claims:
#   - `client` resolves `web` by SERVICE NAME (compose's DNS)
#   - it connects over TCP (the network)
#   - `web` serves a file from THIS machine through a rewritten bind
#   - the result comes back through ANOTHER bind and is read from this side
# service_healthy rather than a retry loop: a loop that never succeeds looks
# the same as one never scheduled.
mkdir -p "$PROJECT/net/html" "$PROJECT/net/out"
echo "served from the client machine" >"$PROJECT/net/html/index.html"
cat >"$PROJECT/net/compose.yaml" <<'COMPOSE'
services:
  web:
    image: nginx:alpine
    volumes:
      - ./html:/usr/share/nginx/html:ro
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "-", "http://127.0.0.1/"]
      interval: 2s
      timeout: 3s
      retries: 20
  client:
    image: alpine:3
    depends_on:
      web:
        condition: service_healthy
    volumes:
      - ./out:/out
    command:
      - sh
      - -c
      - "wget -qO /out/fetched http://web/ && nc -z web 80 && echo reached > /out/status"
COMPOSE

if timeout 240 docker compose -f "$PROJECT/net/compose.yaml" up -d >"$WORK/compose-net.log" 2>&1; then
    if wait_output reached 60 cat "$PROJECT/net/out/status"; then
        ok "a compose service resolved and reached another by service name"
    else
        bad "one compose service never reached the other"
        sed 's/^/        /' "$WORK/compose-net.log" | tail -15
        timeout 60 docker compose -f "$PROJECT/net/compose.yaml" logs 2>&1 | tail -15 | sed 's/^/        /'
    fi

    # And what came back is this machine's file, fetched by one container from
    # another and written back through a second bind.
    if [ "$(cat "$PROJECT/net/out/fetched" 2>/dev/null)" = "served from the client machine" ]; then
        ok "the body it fetched is this machine's file, returned through a bind"
    else
        bad "unexpected body: [$(cat "$PROJECT/net/out/fetched" 2>/dev/null)]"
    fi

    timeout 120 docker compose -f "$PROJECT/net/compose.yaml" down -v >/dev/null 2>&1
else
    bad "the two-service stack would not come up"
    sed 's/^/        /' "$WORK/compose-net.log" | tail -20
fi

echo
echo "== 12c. the compose INSIDE this binary =="
# Sections 12 and 12b use the runner's docker CLI; this uses ours, so a machine
# with no docker installed can run `docker compose up` (ADR 0009). A version
# bump on either side that breaks the pairing fails here.
mkdir -p "$PROJECT/embedded"
echo "served by the embedded compose" >"$PROJECT/embedded/index.html"
cat >"$PROJECT/embedded/compose.yaml" <<'COMPOSE'
services:
  web:
    image: nginx:alpine
    ports:
      - "18083:80"
    volumes:
      - .:/usr/share/nginx/html:ro
COMPOSE

if timeout 180 "$WORK/remote-docker" compose -f "$PROJECT/embedded/compose.yaml" up -d \
    >"$WORK/compose-embedded.log" 2>&1; then
    ok "the embedded compose brought a stack up"

    if wait_url http://127.0.0.1:18083/ "served by the embedded compose" 45; then
        ok "its relative bind resolved and its port was forwarded"
    else
        bad "the embedded compose service never served this machine's file"
        sed 's/^/        /' "$WORK/compose-embedded.log" | tail -20
    fi

    timeout 120 "$WORK/remote-docker" compose -f "$PROJECT/embedded/compose.yaml" down -v \
        >/dev/null 2>&1 && ok "and tore it down again" || bad "the embedded compose down failed"
else
    bad "the embedded compose could not bring a stack up"
    sed 's/^/        /' "$WORK/compose-embedded.log" | tail -20
fi

echo
echo "== 13. our volumes are labelled and identifiable =="
if outputs '^rd-' docker volume ls --format '{{.Name}}'; then
    ok "shares became rd-* volumes on the workspace daemon"
else
    bad "no managed volumes were created"
fi

echo
echo
echo "== 13b. a stock ssh still gets a shell, and the embedded CLI =="
# The ONLY test of the agent's exec/pty session, with a stock ssh rather than
# anything of ours: `remote-docker shell` is gone (ADR 0018) but serveExec and
# servePTY are not. An enrolled key still getting a session is ADR 0010's claim
# that one binary replaces sshd, and nothing else covers it.
# -tt forces a pty, so `tty` naming one proves the agent allocated it.
shellout=$(ssh_account "$REMOTE_DOCKER_STATE_DIR/id_ed25519" "$ACCOUNT" 60 \
    'tty; id -un; if docker ps >/dev/null 2>&1; then echo DOCKER-OK; else docker ps 2>&1 | head -3; fi' -tt 2>&1)

# tr squeezes the pty's CRLF out so a failure prints as one readable line.
trim() { echo "$1" | tr -d '\r' | tail -3 | tr '\n' ' '; }

if echo "$shellout" | grep -q '/dev/pts/'; then
    ok "a stock ssh gets an interactive shell on a pty"
else
    bad "no pty from the agent: $(trim "$shellout")"
fi

# `id -un` names the UNIX user, `rd-<account>` (ADR 0025): the only assertion
# anywhere that sees the unix side.
if echo "$shellout" | grep -q "^rd-$ACCOUNT"; then
    ok "the shell runs as the unix user behind the enrolled account"
else
    bad "the shell was not rd-$ACCOUNT: $(trim "$shellout")"
fi

# ...and can USE the shared daemon, which needs supplementary groups: Go calls
# setgroups() whenever a Credential is set, so a nil Credential.Groups CLEARS
# them and `docker ps` answers "permission denied ... Docker daemon socket".
# Asserted by use, because `id` looked right while the shell's view differed.
if grep -q "DOCKER-OK" <<<"$shellout"; then
    ok "and it can use the shared docker daemon"
else
    bad "the shell cannot use the shared daemon: $(trim "$shellout")"
fi

# The exit status must follow the COMMAND, not stdin. ssh_account redirects
# </dev/null, which cannot show the hang; this holds stdin OPEN through a fifo,
# as a terminal does.
stdin_fifo="$WORK/ssh-stdin-open"
rm -f "$stdin_fifo"
mkfifo "$stdin_fifo"
# Writes nothing and keeps the write end open, so ssh never sends stdin EOF.
sleep 120 >"$stdin_fifo" &
stdin_holder=$!

ssh_exec_status() {
    timeout 20 ssh -i "$REMOTE_DOCKER_STATE_DIR/id_ed25519" \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o BatchMode=yes -p "$SSH_PORT" "$ACCOUNT@127.0.0.1" true \
        <"$stdin_fifo"
    echo "rc=$?"
}

if outputs '^rc=0$' ssh_exec_status; then
    ok "a non-pty ssh command returns its exit status with stdin still open"
else
    bad "ssh true withheld its status with stdin open (124 is the timeout): $LAST_OUTPUT"
fi

kill "$stdin_holder" 2>/dev/null || true
wait "$stdin_holder" 2>/dev/null || true
rm -f "$stdin_fifo"

# The embedded CLI: the client's own docker, not the runner's.
if out=$(timeout 60 "$WORK/remote-docker" ps --format '{{.Names}}' 2>&1); then
    ok "the embedded docker CLI talks to the workspace"
else
    bad "the embedded docker CLI failed: $(echo "$out" | tail -2)"
fi

# `status` while a session holds the reverse-tunnel port. The port is per client
# machine (ADR 0029) and its reservation belongs to one session (ADR 0028), so a
# command that needlessly reserved it would fail whenever a session is running.
if out=$(timeout 60 "$WORK/remote-docker" remote status 2>&1); then
    ok "status works while up is running"
else
    bad "status failed while up was running: $(echo "$out" | tail -2)"
fi

# gc must not remove a volume that is in use, and must not fail.
if out=$(timeout 90 "$WORK/remote-docker" remote gc 2>&1); then
    if echo "$out" | grep -q "removed"; then
        ok "gc ran and reported what it removed"
    else
        bad "gc gave no account of itself: $out"
    fi
else
    bad "gc failed: $(echo "$out" | tail -2)"
fi

echo
echo "== 13c. docker build, and whether COPY sees this machine's files =="
# Nothing is mounted for a build: the CLI uploads the context from THIS machine.
# Asserted through the CONTENT of a file written here, so a stale or empty
# context fails rather than passing on an image that exists.
BUILDCTX="$WORK/buildctx"
mkdir -p "$BUILDCTX/sub"
echo "content-from-the-client-machine" >"$BUILDCTX/marker.txt"
echo "nested-file" >"$BUILDCTX/sub/nested.txt"
cat >"$BUILDCTX/Dockerfile" <<'DOCKERFILE'
FROM alpine:3
COPY marker.txt /marker.txt
ADD sub /sub
RUN cat /marker.txt /sub/nested.txt
DOCKERFILE

if out=$(cd "$BUILDCTX" && timeout 300 "$WORK/remote-docker" build -t itest-build . 2>&1); then
    # BuildKit, or the classic builder wearing its name? Without buildx
    # vendored, `build` silently took the classic path even with
    # DOCKER_BUILDKIT=1 and passed every other check here. So the builders are
    # told apart by what they SAY, in both directions.
    if echo "$out" | grep -qE "exporting to image|\[internal\] load build definition"; then
        ok "docker build goes through BuildKit"
    else
        bad "the build did not use BuildKit"
        echo "$out" | head -5 | sed 's/^/        /'
    fi
    if echo "$out" | grep -q "Sending build context to Docker daemon"; then
        bad "the build fell back to the classic builder"
    else
        ok "and not the classic builder"
    fi

    if echo "$out" | grep -q "content-from-the-client-machine"; then
        ok "COPY read a file from this machine during the build"
    else
        bad "the build ran but COPY did not produce the file's content"
        echo "$out" | tail -10 | sed 's/^/        /'
    fi
    if echo "$out" | grep -q "nested-file"; then
        ok "ADD carried a subdirectory too"
    else
        bad "ADD did not carry the subdirectory"
    fi
else
    bad "docker build failed: $(echo "$out" | tail -5 | tr '\n' ' ')"
fi

# And the image is real: it runs, and what COPY put there is still there.
expect_output "the built image runs and carries the copied file" \
    "content-from-the-client-machine" -- --rm itest-build cat /marker.txt

# A file the context excludes must NOT reach the daemon: a .git, a
# node_modules, somebody's secrets.
echo "must-not-be-uploaded" >"$BUILDCTX/secret.txt"
printf 'secret.txt\n' >"$BUILDCTX/.dockerignore"
cat >"$BUILDCTX/Dockerfile" <<'DOCKERFILE'
FROM alpine:3
COPY . /ctx
RUN ls /ctx
DOCKERFILE
if out=$(cd "$BUILDCTX" && timeout 300 "$WORK/remote-docker" build -t itest-ignore . 2>&1); then
    if echo "$out" | grep -q "secret.txt"; then
        bad ".dockerignore was not honoured; the excluded file was uploaded"
    else
        ok ".dockerignore keeps a file out of the build context"
    fi
else
    bad "the .dockerignore build failed: $(echo "$out" | tail -5 | tr '\n' ' ')"
fi

timeout 60 docker rmi -f itest-build itest-ignore >/dev/null 2>&1

echo
echo "== 14. elevate =="
# Swarm cannot run privileged tasks, so the service relaunches itself with
# `docker run` (ADR 0013), which is what runs here; no Swarm is needed. The
# assertion that matters is the last: a child inheriting the host's Docker
# socket gives every enrolled user root on the node.
ELEV=remote-docker-elev
hostdocker rm -f "$ELEV" "$ELEV.elevated" >/dev/null 2>&1

if hostdocker run -d --name "$ELEV" \
    -v /var/run/docker.sock:/var/run/host-docker.sock \
    "$IMAGE" elevate >/dev/null 2>&1; then

    elevated=false
    for _ in $(seq 1 60); do
        if hostdocker inspect "$ELEV.elevated" >/dev/null 2>&1; then
            elevated=true
            break
        fi
        outputs true hostdocker inspect -f '{{.State.Running}}' "$ELEV" || break
        sleep 1
    done

    if [ "$elevated" != true ]; then
        bad "elevate never launched a privileged container"
        hostdocker logs "$ELEV" 2>&1 | tail -15 | sed 's/^/        /'
    else
        ok "elevate launched a privileged container"

        if [ "$(hostdocker inspect -f '{{.HostConfig.Privileged}}' "$ELEV.elevated" 2>/dev/null)" = "true" ]; then
            ok "the child is privileged"
        else
            bad "the child is not privileged, which is the entire point"
        fi

        # Sharing the launcher's network namespace is what lets a published
        # port reach the workspace. Without it nothing can connect, and
        # nothing says why.
        parent_id=$(hostdocker inspect -f '{{.Id}}' "$ELEV" 2>/dev/null)
        netmode=$(hostdocker inspect -f '{{.HostConfig.NetworkMode}}' "$ELEV.elevated" 2>/dev/null)
        if [ "$netmode" = "container:$parent_id" ]; then
            ok "the child shares the launcher's network namespace"
        else
            bad "the child's network mode is [$netmode], not the launcher's namespace"
        fi

        sockets=$(hostdocker inspect -f '{{range .Mounts}}{{.Destination}} {{end}}' "$ELEV.elevated" 2>/dev/null | tr ' ' '
' | grep -c "docker.sock")
        if [ "$sockets" = "0" ]; then
            ok "the child did NOT inherit the host docker socket"
        else
            bad "SECURITY: the privileged child has the host docker socket"
        fi
    fi
else
    bad "could not start the elevate launcher"
fi
hostdocker rm -f "$ELEV" "$ELEV.elevated" >/dev/null 2>&1

echo
echo "== 15. does replay make a container's watcher fire? =="
# 11b shows a client-side change notifies nobody, 11d which syscall would; this
# runs the whole path (client watcher, SSH channel, agent replay). The client is
# restarted with watching on, and sections 15b onwards keep it.
#
# Restarted rather than run beside the first: both would want this machine's
# one reverse-tunnel port (ADR 0029). The probe starts through the new client
# because a session connects on demand (ADR 0015), and one that never connects
# never opens the notification channel.
stop_pid "$CLIENT_PID"
CLIENT_PID=""

REPLAYDIR="$WORK/replayed"
mkdir -p "$REPLAYDIR"
echo "before the watch" >"$REPLAYDIR/reloaded.txt"

if [ -x "$WATCHPROBE" ] && cp "$WATCHPROBE" "$PROJECT/watchprobe"; then
    # Prefetch is off by default (ADR 0045); this client turns the tree on so
    # 15c and 15e keep covering it. The other suites run the default.
    REMOTE_DOCKER_WATCH=partial REMOTE_DOCKER_PREFETCH=tree "$WORK/remote-docker" remote start --foreground >"$WORK/watch-up.log" 2>&1 &
    CLIENT_PID=$!

    if ! wait_endpoint "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID"; then
        bad "the watching client never came up"
        sed 's/^/        /' "$WORK/watch-up.log"
    elif ! dockert run -d --name itest-replay \
        -v "$PROJECT:/probe:ro" \
        -v "$REPLAYDIR:/data" \
        alpine:3 /probe/watchprobe -timeout 45s /data >"$WORK/replay-run.log" 2>&1; then
        bad "the replay probe container would not start"
        sed 's/^/        /' "$WORK/replay-run.log"
    else
        ok "the watching client is serving"

        wait_ready itest-replay 30

        # The edit an editor would make: rewrite an existing file in place.
        sleep 2
        echo "edited on the client at $(date +%s)" >"$REPLAYDIR/reloaded.txt"

        timeout 90 docker wait itest-replay >/dev/null 2>&1
        replay=$(docker logs itest-replay 2>&1)
        docker rm -f itest-replay >/dev/null 2>&1
        rm -f "$PROJECT/watchprobe"

        echo "        $(echo "$replay" | grep -o 'RESULT inotify_events=[0-9]* poll_entries=[0-9]*' || echo 'RESULT missing')"
        echo "        events for the edited file:"
        echo "$replay" | grep -E "^INOTIFY .* reloaded\.txt$" | sort | uniq -c | sed 's/^/          /'

        if echo "$replay" | grep -qE "^INOTIFY .*(IN_MODIFY|IN_CLOSE_WRITE).* reloaded\.txt$"; then
            ok "an edit on the client fires inotify inside the container (ADR 0016)"
        else
            bad "replay did not reach the container's watcher"
            sed 's/^/        /' "$WORK/watch-up.log" | tail -20
        fi

        # The echo loop this design has to avoid: the poke travels back over
        # NFS, the client reports it as a change, and that produces another
        # poke. openclose is silent over NFSv3 and the mtime write-back is an
        # identity, so the count stays bounded rather than climbing.
        pokes=$(echo "$replay" | grep -cE "^INOTIFY .* reloaded\.txt$")
        info "the watcher saw $pokes events for one edit"
        if [ "$pokes" -gt 0 ] && [ "$pokes" -lt 25 ]; then
            ok "replay does not echo into a loop"
        elif [ "$pokes" -ge 25 ]; then
            bad "replay looks like it is looping: $pokes events for one edit"
        fi
    fi
else
    bad "could not build the replay probe"
fi

echo
echo "== 15b. the cached consistency =="
# `cached` gives the NFS mount a long attribute cache (ADR 0042), made safe by
# the watcher: hence section 15's watching client, and a refusal without one.
CACHEDIR="$WORK/cachedir"
mkdir -p "$CACHEDIR"
echo "first" >"$CACHEDIR/marker"

if [ -n "${CLIENT_PID:-}" ] && kill -0 "$CLIENT_PID" 2>/dev/null; then
    if dockert run -d --name itest-cached -v "$CACHEDIR:/w:cached" \
        alpine:3 sleep 300 >"$WORK/cached-run.log" 2>&1; then
        ok "a container starts against a cached mount"

        if outputs '^first$' docker exec itest-cached cat /w/marker; then
            ok "it reads the file through the cached mount"
        else
            bad "reading through a cached mount: [$LAST_OUTPUT]"
        fi

        # The volume carries the mount options, which is where the mode lives:
        # nothing else about the mount differs, so this is the whole of it.
        vol=$(docker inspect -f '{{range .Mounts}}{{.Name}}{{end}}' itest-cached 2>/dev/null)
        if outputs 'actimeo=60' docker volume inspect -f '{{.Options.o}}' "$vol"; then
            ok "the volume was built with the long attribute cache"
        else
            bad "volume $vol options: [$LAST_OUTPUT]"
        fi

        # actimeo=60 lets the kernel trust its copy for a minute: without the
        # watcher's poke this reads "first" until then.
        echo "second" >"$CACHEDIR/marker"
        if wait_output '^second$' 20 docker exec itest-cached cat /w/marker; then
            ok "an edit here is visible through the cached mount"
        else
            bad "the cached mount still reads [$LAST_OUTPUT] after 20s"
        fi
    else
        bad "a container would not start against a cached mount"
        sed 's/^/        /' "$WORK/cached-run.log"
    fi
    docker rm -f itest-cached >/dev/null 2>&1
else
    bad "no watching client is running, so cached could not be tested"
fi

echo
echo "== 15c. read=cached,write=back, which is a union =="
# Docker's `delegated`: a UNION the workspace mounts, the live NFS export under
# a local cache (ADR 0044). The assertion that matters is the fallthrough: a
# file created after the fill must still be seen. Invalidation rides the
# watcher, hence section 15's watching client.
#
# share_diagnostics prints the session's cache state, the client log and the
# WORKSPACE log, which the workflow's log step cannot show: the suite has torn
# the container down by then.
share_diagnostics() {
    "$WORK/remote-docker" remote status 2>&1 |
        grep -iE "cache|watch" | sed 's/^/        client: /'
    grep -iE "cache|union|fuse|writ|chang|collect" "$WORK/watch-up.log" 2>/dev/null |
        tail -12 | sed 's/^/        client: /'
    # Not `docker logs`: `docker` here is the CLIENT's endpoint, where the
    # workspace container does not exist and would look like it logged nothing.
    dump_workspace_log 25
}

UNIONDIR="$WORK/write-back"
mkdir -p "$UNIONDIR"
echo "first" >"$UNIONDIR/marker"
# Deleted in section 16 with no client running: no watcher sees it, so only the
# record of what the last fill sent can take it out of the cache.
echo "here before the fill" >"$UNIONDIR/while-down.txt"

if [ -n "${CLIENT_PID:-}" ] && kill -0 "$CLIENT_PID" 2>/dev/null; then
    if dockert run -d --name itest-deleg -v "$UNIONDIR:/w:read=cached,write=back" \
        alpine:3 sleep 900 >"$WORK/deleg-run.log" 2>&1; then
        ok "a container starts against a write=back union"

        if outputs '^first$' docker exec itest-deleg cat /w/marker; then
            ok "it reads a file the cache was filled with"
        else
            bad "reading the cache: [$LAST_OUTPUT]"
        fi

        # a bare directory passes every other check here (ADR 0044)
        if union_is_fuse docker itest-deleg; then
            ok "the container's share is a union, not a directory that resembles one"
        else
            bad "/w is not a fuse mount: [$LAST_OUTPUT]"
            share_diagnostics
        fi

        # The read above passes with an empty cache too, since a miss falls
        # through. Write-back is gated on a complete fill, so a fill that did
        # nothing would otherwise surface later as a write that never arrived.
        if wait_output "$UNIONDIR: .*, cached\$" 20 "$WORK/remote-docker" remote status; then
            ok "the fill completed, so the share is cached rather than only live"
        else
            bad "the share never reported a complete cache: [$LAST_OUTPUT]"
        fi

        # THE assertion: this file postdates the fill, so it can only come from
        # the live export. Retried because the NFS attribute cache and libfuse's
        # entry cache (about a second each) sit in between; the time is reported.
        echo "arrived after the fill" >"$UNIONDIR/late.txt"
        if wait_output '^arrived after the fill$' 15 docker exec itest-deleg cat /w/late.txt; then
            ok "a file the cache does not have falls through to the live export (${WAITED}s)"
        else
            bad "the union never fell through: [$LAST_OUTPUT]"
        fi

        # What the container mounts is the union, in the daemon's namespace,
        # rather than a volume of its own.
        if outputs '/run/rd-union/' docker inspect \
            -f '{{range .Mounts}}{{.Source}}{{end}}' itest-deleg; then
            ok "the container binds the union the workspace mounted"
        else
            bad "the mount source is [$LAST_OUTPUT], want a union"
        fi

        # Write-back: an overlay's cache layer IS the record of what the
        # container changed (ADR 0044). Polled, so it takes seconds.
        write_comes_back docker itest-deleg "$UNIONDIR" "read=cached,write=back" back ||
            share_diagnostics

        # An edit here reaches the container, because the workspace writes it
        # THROUGH the union rather than into the layer underneath (ADR 0044).
        echo "edited here" >"$UNIONDIR/marker"
        # The lower carries the share's read mode (actimeo=60), so the edit
        # arriving in seconds proves the watcher's replayed SETATTR refreshes
        # it. The time is printed so a pass at nineteen seconds shows as such.
        if wait_output '^edited here$' 20 docker exec itest-deleg cat /w/marker; then
            ok "an edit here reaches a running write=back container (${WAITED}s, under actimeo=60)"
        else
            bad "the cache stayed stale after an edit: [$LAST_OUTPUT]"
            share_diagnostics
        fi

        # And a DELETION: a cached copy would shadow the file's absence, and
        # the Docker API cannot remove a path from a volume.
        rm -f "$UNIONDIR/marker"
        if wait_gone dockert itest-deleg /w/marker 20; then
            ok "a file deleted here disappears from the container"
        else
            bad "a deleted file is still visible through the union: [$LAST_OUTPUT]"
            share_diagnostics
        fi
    else
        bad "a container would not start against a write=back union"
        sed 's/^/        /' "$WORK/deleg-run.log"
        dump_workspace_log 40
    fi
    # itest-deleg is LEFT RUNNING: section 16 asserts its share survives a
    # client restart.
else
    bad "no client is running, so the union could not be tested"
fi

echo
echo "== 15d. a linker finishing its output on a share =="
# From a build on a real workspace:
#   /usr/bin/ld: cmTC_438e8: final close failed: Stale file handle
# Compiling succeeds; ld FINISHING an output file fails, so it is a handle
# problem. go-nfs maps a handle to a PATH and drops it on REMOVE and RENAME,
# where a real server keeps it valid for an open file, and an open descriptor
# cannot be re-resolved the way a lookup can. A PASS here is worth reading: CI
# runs dind on the same machine, and the report came from another host.
LINKDIR="$WORK/linkdir"
mkdir -p "$LINKDIR"
cat >"$LINKDIR/hello.c" <<'CEOF'
int main(void) { return 0; }
CEOF

if [ -n "${CLIENT_PID:-}" ] && kill -0 "$CLIENT_PID" 2>/dev/null; then
    # Exit codes name the step: the reported bug compiles and fails to link.
    link_rc=0
    dockert run --rm -v "$LINKDIR:/src" -w /src alpine:3 sh -c '
        apk add --no-cache gcc musl-dev >/dev/null 2>&1 || exit 97
        cc -c hello.c -o hello.o || exit 98
        cc hello.o -o hello       || exit 99
        # Not just that ld finished: a linker whose output cannot be run has
        # not done its job. The bit is written by a SETATTR, which the share
        # used to accept and discard.
        test -x hello || { echo "not executable:"; ls -l hello; exit 96; }
    ' >"$WORK/link.log" 2>&1 || link_rc=$?

    case "$link_rc" in
        0)  ok "a container compiles and links on a share" ;;
        97) bad "no compiler in the container, so linking on a share was not tested"
            sed 's/^/        /' "$WORK/link.log" ;;
        98) bad "COMPILING on a share failed, which the report said worked"
            sed 's/^/        /' "$WORK/link.log" ;;
        99) bad "LINKING on a share failed, which is the reported bug"
            sed 's/^/        /' "$WORK/link.log"
            # go-nfs sends no STALE here (every NFS3ERR_STALE comes from
            # FromHandle, which logged nothing), so the CLIENT decided, and its
            # kernel log says why: e.g. an inode number changed under a handle.
            echo "        -- kernel, nfs client --"
            sudo dmesg 2>/dev/null | grep -iE "nfs|stale|fileid|inode number" | tail -20 | sed 's/^/        /' ;;
        *)  bad "linking on a share ended with $link_rc"
            sed 's/^/        /' "$WORK/link.log" ;;
    esac

    # Narrowed: ld finishes by sizing and permissioning the file it wrote, so
    # each step is one part of that shape and the first to fail names it.
    narrow_rc=0
    dockert run --rm -v "$LINKDIR:/src" -w /src alpine:3 sh -c '
        echo hi > p1                                    || exit 91
        echo hi > p2 && truncate -s 4 p2                || exit 92
        echo hi > p3 && chmod 755 p3                    || exit 93
        echo hi > p4 && truncate -s 65536 p4            || exit 94
        echo hi > p5 && dd if=/dev/zero of=p5 bs=1 count=2 conv=notrunc 2>/dev/null || exit 95
        echo hi > p6 && truncate -s 65536 p6 && chmod 755 p6 && truncate -s 3 p6   || exit 96
    ' >"$WORK/link-narrow.log" 2>&1 || narrow_rc=$?

    case "$narrow_rc" in
        0)  ok "create, truncate, chmod and rewrite-in-place all survive on a share" ;;
        91) bad "a plain create-write-close failed on a share" ;;
        92) bad "truncating a file DOWN failed on a share" ;;
        93) bad "chmod after a write failed on a share" ;;
        94) bad "truncating a file UP failed on a share" ;;
        95) bad "rewriting bytes in place failed on a share" ;;
        96) bad "grow-then-chmod-then-shrink, which is ld's shape, failed on a share" ;;
        *)  bad "narrowing the linker failure ended with $narrow_rc" ;;
    esac
    [ "$narrow_rc" = 0 ] || sed 's/^/        /' "$WORK/link-narrow.log"

else
    bad "no client is running, so linking on a share could not be tested"
fi

echo "== 15e. the other corners of the mode grid =="
# Two read modes by three write modes (ADR 0042); 15b and 15c covered
# read=cached with write=through and write=back. union_corner asks every corner
# the same things before its own write assertion, so a corner cannot pass by
# resembling another. Section 16 asserts the client restart for the container
# left running here.
union_corner() {
    local name=$1 dir=$2 mode=$3
    mkdir -p "$dir"
    echo "first" >"$dir/marker"
    if ! dockert run -d --name "$name" -v "$dir:/w:$mode" alpine:3 sleep 900 >"$WORK/$name-run.log" 2>&1; then
        bad "$mode: a container would not start against the union"
        sed 's/^/        /' "$WORK/$name-run.log"
        dump_workspace_log 40
        return 1
    fi
    ok "$mode: a container starts"

    if union_is_fuse docker "$name"; then
        ok "$mode: the share is a union, not a directory that resembles one"
    else
        bad "$mode: /w is not a fuse mount: [$LAST_OUTPUT]"
        share_diagnostics
    fi
    if outputs '^first$' docker exec "$name" cat /w/marker; then
        ok "$mode: it reads a file through the union"
    else
        bad "$mode: reading through the union: [$LAST_OUTPUT]"
    fi
    if outputs '/run/rd-union/' docker inspect -f '{{range .Mounts}}{{.Source}}{{end}}' "$name"; then
        ok "$mode: the container binds the union the workspace mounted"
    else
        bad "$mode: the mount source is [$LAST_OUTPUT], want a union"
    fi

    echo "edited here" >"$dir/marker"
    if wait_output '^edited here$' 20 docker exec "$name" cat /w/marker; then
        ok "$mode: an edit here reaches the container (${WAITED}s)"
    else
        bad "$mode: the union stayed stale after an edit: [$LAST_OUTPUT]"
        share_diagnostics
    fi

    rm -f "$dir/marker"
    if wait_gone dockert "$name" /w/marker 20; then
        ok "$mode: a file deleted here disappears from the container"
    else
        bad "$mode: a deleted file is still visible through the union: [$LAST_OUTPUT]"
        share_diagnostics
    fi
    return 0
}

# nothing_prefetched asserts the status line for a share reports nothing sent.
nothing_prefetched() {
    local label=$1 dir=$2
    if outputs "$dir: .* 0B sent" "$WORK/remote-docker" remote status; then
        ok "$label: nothing was prefetched into the union"
    else
        bad "$label: the status says something was sent: [$LAST_OUTPUT]"
    fi
}

if [ -n "${CLIENT_PID:-}" ] && kill -0 "$CLIENT_PID" 2>/dev/null; then
    # read=direct,write=back: writes come back, reads are live, nothing is
    # prefetched.
    DIRECTBACK="$WORK/direct-back"
    if union_corner itest-direct-back "$DIRECTBACK" "read=direct,write=back"; then
        write_comes_back docker itest-direct-back "$DIRECTBACK" "read=direct,write=back" back ||
            share_diagnostics
        nothing_prefetched "read=direct,write=back" "$DIRECTBACK"
        dockert rm -f itest-direct-back >/dev/null 2>&1
    fi

    # read=direct,write=ephemeral: a build directory. The container's writes
    # never come back, including after the container is gone and the union
    # released.
    EPHDIR="$WORK/direct-ephemeral"
    if union_corner itest-direct-eph "$EPHDIR" "read=direct,write=ephemeral"; then
        write_comes_back docker itest-direct-eph "$EPHDIR" "read=direct,write=ephemeral" ephemeral ||
            share_diagnostics
        nothing_prefetched "read=direct,write=ephemeral" "$EPHDIR"
        dockert rm -f itest-direct-eph >/dev/null 2>&1
        sleep 10
        if [ ! -f "$EPHDIR/written-there" ]; then
            ok "read=direct,write=ephemeral: nothing arrived after the container was gone either"
        else
            bad "read=direct,write=ephemeral: a write arrived once the union was released: [$(cat "$EPHDIR/written-there")]"
            share_diagnostics
        fi
    fi

    # read=cached,write=ephemeral: prefetch on (this client runs
    # REMOTE_DOCKER_PREFETCH=tree), writes dropped. Left running for section
    # 16, which asserts it survives a client restart.
    CEPHDIR="$WORK/cached-ephemeral"
    if union_corner itest-cached-eph "$CEPHDIR" "read=cached,write=ephemeral"; then
        write_comes_back docker itest-cached-eph "$CEPHDIR" "read=cached,write=ephemeral" ephemeral ||
            share_diagnostics
        # Polled: the walk yields to reads for a couple of seconds before it
        # fills the rest (ADR 0045).
        if wait_output "$CEPHDIR: .* [1-9][0-9.]*[KMG]?B sent" 20 "$WORK/remote-docker" remote status; then
            ok "read=cached,write=ephemeral: the union was prefetched"
        else
            bad "read=cached,write=ephemeral: nothing was ever sent: [$LAST_OUTPUT]"
            share_diagnostics
        fi
    fi
else
    bad "no client is running, so the other corners could not be tested"
fi

echo
echo "== 15f. a container that is not root, and a tar that sets attributes =="
# A share reports the account as owner with wide bits, and a union's upper is
# wide too (ADR 0046). GNU tar sets utime, owner and mode on the descriptor it
# just wrote; the defects behind that showed only from a Windows client and are
# pinned by fileid_abs_test.go, resolve_test.go and machine.yml.
if [ -n "${CLIENT_PID:-}" ] && kill -0 "$CLIENT_PID" 2>/dev/null; then
    USERDIR="$WORK/non-root"
    mkdir -p "$USERDIR/sub"
    echo "x" >"$USERDIR/sub/a"

    if outputs '^10000 10000 777$' dockert run --rm --user 1000 -v "$USERDIR:/w" alpine:3 stat -c '%u %g %a' /w; then
        ok "a share reports the account as owner with wide bits"
    else
        bad "a share reports [$LAST_OUTPUT], want the account uid and 777"
    fi
    if outputs 'MKDIR-OK' dockert run --rm --user 1000 -v "$USERDIR:/w" alpine:3 sh -c 'mkdir /w/made && echo MKDIR-OK'; then
        ok "a uid the image chose can create a directory on a plain mount"
    else
        bad "uid 1000 could not mkdir on a plain mount: [$LAST_OUTPUT]"
    fi
    if outputs 'TOP-OK SUB-OK' dockert run --rm --user 1000 -v "$USERDIR:/w:write=back" alpine:3 sh -c 'mkdir /w/top && printf "TOP-OK " && mkdir /w/sub/inner && echo SUB-OK'; then
        ok "a uid the image chose can create directories at the top and inside a union"
    else
        bad "uid 1000 could not create directories in a union: [$LAST_OUTPUT]"
        share_diagnostics
    fi

    # GNU tar as an ordinary uid; debian ships it, and a non-root uid cannot
    # install one. One numbered exit per step.
    if outputs 'TAR-OK' dockert run --rm --user 1001 -v "$USERDIR:/w" debian:stable-slim sh -c '
        cd /w || exit 91
        mkdir -p src && echo x >src/b || exit 92
        tar -cf t.tar src || exit 93
        rm -r src || exit 94
        tar -xf t.tar || exit 95
        touch src/b && chmod 600 src/b || exit 96
        echo TAR-OK'; then
        ok "GNU tar extracts into a share as uid 1001, attributes included"
    else
        case $LAST_STATUS in
            95) bad "tar -xf failed on a share: [$LAST_OUTPUT]" ;;
            96) bad "attributes could not be set after the extraction: [$LAST_OUTPUT]" ;;
            *) bad "the tar probe failed at step $LAST_STATUS: [$LAST_OUTPUT]" ;;
        esac
    fi
else
    bad "no client is running, so the non-root cases could not be tested"
fi

echo
echo "== 16. a background session, with no terminal held open =="
# `start --foreground` IS the daemon body, so this is the same session detached,
# stopped by asking rather than signalling, and reclaiming itself when idle.
# The suite's own session stops first: an endpoint has one owner.
stop_pid "$CLIENT_PID"
CLIENT_PID=""
sleep 2

# WHILE NOTHING IS RUNNING: no watcher sees this and a fill only overwrites and
# adds, so only the record of what the last fill sent removes it (ADR 0044).
rm -f "$UNIONDIR/while-down.txt"

# Watching on: this session serves 15c's write=back share, a mode refused
# without a watcher because only it keeps the cached copies honest (ADR 0044).
if REMOTE_DOCKER_WATCH=partial "$WORK/remote-docker" remote start >"$WORK/start.log" 2>&1; then
    ok "start returned without holding a terminal"
    sed 's/^/        /' "$WORK/start.log"
else
    bad "start failed"
    sed 's/^/        /' "$WORK/start.log"
fi

if out=$(dockert run --rm alpine:3 echo through-the-daemon 2>&1); then
    if [ "$out" = "through-the-daemon" ]; then
        ok "docker works through the background session"

        # The union outlives the channel that asked for it (ADR 0044): every
        # channel it was prepared on died with the client, and its container
        # still runs. A file the CONTAINER wrote proves mount and cache layer
        # are the same; a union released under a container cannot be repaired.
        if outputs '^written by itest-deleg$' docker exec itest-deleg cat /w/written-there; then
            ok "a container's union survives the client restarting"
        else
            bad "the union did not survive a client restart: [$LAST_OUTPUT]"
            share_diagnostics
        fi
        docker rm -f itest-deleg >/dev/null 2>&1
        # And the ephemeral one section 15e left running, whose upper holds
        # only what the container wrote: that write must still be there.
        if outputs '^written by itest-cached-eph$' docker exec itest-cached-eph cat /w/written-there; then
            ok "an ephemeral union survives the client restarting"
        else
            bad "the ephemeral union did not survive a client restart: [$LAST_OUTPUT]"
            share_diagnostics
        fi
        docker rm -f itest-cached-eph >/dev/null 2>&1

        # And the deletion made while nothing ran. A NEW container, because the
        # reconcile happens at the next fill; polled, because the fill is
        # asynchronous and the container does not wait for it.
        if dockert run -d --name itest-reconcile -v "$UNIONDIR:/w:read=cached,write=back" \
            alpine:3 sleep 120 >"$WORK/reconcile-run.log" 2>&1; then
            if wait_gone dockert itest-reconcile /w/while-down.txt 20; then
                ok "a file deleted while the session was down is gone from the cache"
            else
                bad "a file deleted while the session was down is still in the cache: [$LAST_OUTPUT]"
                share_diagnostics
            fi
            docker rm -f itest-reconcile >/dev/null 2>&1
        else
            bad "the reconcile container would not start"
            sed 's/^/        /' "$WORK/reconcile-run.log"
        fi

        # And the verdict: the command above went through a live session.
        if outputs "^status  *ready" "$WORK/remote-docker" remote status && [ "$LAST_STATUS" = 0 ]; then
            ok "status says ready while a session is serving, and exits 0"
        else
            bad "status did not say ready (exit $LAST_STATUS): $(echo "$LAST_OUTPUT" | head -1)"
        fi
    else
        bad "unexpected output through the daemon: $out"
    fi
else
    bad "docker failed through the daemon: $out"
fi

# Idempotent: a second start must not fight the first for the endpoint.
if outputs "already running" "$WORK/remote-docker" remote start; then
    ok "a second start reports the running one rather than racing it"
else
    bad "a second start did not recognise the running session; it said: $LAST_OUTPUT"
fi

# The endpoint has one owner and must refuse rather than steal: on Unix it once
# unlinked the socket, leaving the first session on an unreachable inode.
if out=$("$WORK/remote-docker" remote start --foreground 2>&1); then
    bad "a second session took the endpoint from the running one"
else
    case "$out" in
        *"already serving"*) ok "a second session is refused, naming the owner" ;;
        *) bad "a second session failed for the wrong reason: $out" ;;
    esac
fi

# A detached container keeps its mount after the command that started it exits.
if dockert run -d --name itest-detached -v "$PROJECT:/w" alpine:3 sh -c "$PIN_SH" >/dev/null 2>&1; then
    sleep 10
    if [ "$(docker inspect -f '{{.State.Running}}' itest-detached 2>/dev/null)" = "true" ]; then
        ok "a detached container keeps its mount after the command exits"
    else
        bad "the detached container lost its mount"
        docker logs itest-detached 2>&1 | tail -5 | sed 's/^/        /'
    fi
    docker rm -f itest-detached >/dev/null 2>&1
else
    bad "could not start the detached container"
fi

if outputs "stopped" "$WORK/remote-docker" remote stop; then
    ok "stop ends the background session"
else
    bad "stop did not end the background session; it said: $LAST_OUTPUT"
fi
if outputs "not running" "$WORK/remote-docker" remote stop; then
    ok "stopping an already-stopped session says so"
else
    bad "stopping twice did not report it was not running; it said: $LAST_OUTPUT"
fi

# The sequence a person types, with no sleep: `start` returns once the endpoint
# answers and `stop` once the process is gone. The checks above prove only that
# `stop` SAYS "stopped".
# `stop && start` raced: stop waited for the endpoint to go quiet, which is where
# Session.Close STARTS tearing down the tunnel and the export. This machine has
# one reverse-tunnel port (ADR 0029), so a start that overtook the release asked
# for a port the workspace had not let go of yet.
if "$WORK/remote-docker" remote start >/dev/null 2>&1 &&
    dockert run --rm -v "$PROJECT:/w" alpine:3 cat /w/marker >"$WORK/after-start.txt" 2>&1 &&
    grep -q "from the project directory" "$WORK/after-start.txt"; then
    ok "start && docker run works with nothing in between"
else
    bad "a container run straight after start failed: $(tail -2 "$WORK/after-start.txt" 2>/dev/null | tr '\n' ' ')"
fi

if "$WORK/remote-docker" remote stop >/dev/null 2>&1 &&
    "$WORK/remote-docker" remote start >/dev/null 2>&1 &&
    dockert run --rm -v "$PROJECT:/w" alpine:3 cat /w/marker >"$WORK/after-restart.txt" 2>&1 &&
    grep -q "from the project directory" "$WORK/after-restart.txt"; then
    ok "stop && start && docker run works with nothing in between"
else
    bad "a container run straight after stop && start failed: $(tail -2 "$WORK/after-restart.txt" 2>/dev/null | tr '\n' ' ')"
fi

"$WORK/remote-docker" remote stop >/dev/null 2>&1

# A session from a different build is replaced when nothing depends on it and
# reported when something does; otherwise an updated client silently talks to
# the OLD build. Same source, different stamps, so versions cannot be ordered.
if (cd "$REPO/client" && CGO_ENABLED=0 go build -ldflags="-X main.version=sha-otherbuild" \
    -o "$WORK/remote-docker-otherbuild" ./cmd/remote-docker); then

    "$WORK/remote-docker-otherbuild" remote start >/dev/null 2>&1

    # (a) nothing depends on it -> replaced silently.
    "$WORK/remote-docker" ps >/dev/null 2>&1
    if outputs "DIFFERENT" "$WORK/remote-docker" remote status; then
        bad "an unused session from another build was not replaced"
    else
        ok "an unused session from another commit is replaced silently"
    fi

    # (b) something depends on it -> warned about, left alone. The old binary
    # starts the container so the session holding it is the old one.
    "$WORK/remote-docker" remote stop >/dev/null 2>&1
    "$WORK/remote-docker-otherbuild" remote start >/dev/null 2>&1
    if "$WORK/remote-docker-otherbuild" run -d --name itest-pin -v "$PROJECT:/w" alpine:3 sh -c "$PIN_SH" >/dev/null 2>&1; then

        warned=$("$WORK/remote-docker" ps 2>&1)
        case "$warned" in
            *"different build"*) ok "a session in use from another commit is reported, not restarted" ;;
            *) bad "no version warning while a container depended on the old session" ;;
        esac
        # Captured, so a failure can show which build status says is serving.
        insitu=$("$WORK/remote-docker" remote status 2>&1)
        if echo "$insitu" | grep -q "sha-otherbuild"; then
            ok "the in-use session was left running"
        else
            bad "the in-use session was replaced, taking its container's mount with it"
            echo "$insitu" | sed 's/^/        /'
        fi

        # restart must refuse rather than break it.
        if "$WORK/remote-docker" remote restart >/dev/null 2>&1; then
            bad "restart proceeded while a container depended on the session"
        else
            ok "restart refuses while something depends on the session"
        fi
        # And stop, the same way, naming -f as the way through.
        if outputs "fix: .*remote stop.* -f" "$WORK/remote-docker" remote stop &&
            [ "$LAST_STATUS" != 0 ]; then
            ok "stop refuses while something depends on the session, and names -f"
        else
            bad "stop did not refuse (exit $LAST_STATUS): $LAST_OUTPUT"
        fi

        "$WORK/remote-docker" rm -f itest-pin >/dev/null 2>&1
    else
        bad "could not start the pinning container"
    fi
    "$WORK/remote-docker" remote stop >/dev/null 2>&1
else
    bad "could not build a second client for the version test"
fi

# Standby drops the workspace connection and the watches but keeps serving the
# endpoint. Asserted through the endpoint, not the log: a run afterwards proves
# the socket survived AND the request woke the session.
if REMOTE_DOCKER_DAEMON_STANDBY=5s "$WORK/remote-docker" remote start >/dev/null 2>&1; then
    # Longer than the standby and its poll, which runs at a quarter of it.
    sleep 12

    if outputs "already running" "$WORK/remote-docker" remote start; then
        ok "a session on standby is still running"
    else
        bad "the session exited when it should only have stood by"
    fi

    if dockert run --rm -v "$PROJECT:/w" alpine:3 cat /w/marker >"$WORK/after-standby.txt" 2>&1 &&
        grep -q "from the project directory" "$WORK/after-standby.txt"; then
        ok "the endpoint still serves after standby, and the request woke it"
    else
        bad "a container run after standby failed: $(tail -2 "$WORK/after-standby.txt" 2>/dev/null | tr '\n' ' ')"
    fi

    # Standing by and waking must be repeatable, not a one-shot.
    sleep 12
    if dockert run --rm -v "$PROJECT:/w" alpine:3 cat /w/marker >"$WORK/after-standby2.txt" 2>&1 &&
        grep -q "from the project directory" "$WORK/after-standby2.txt"; then
        ok "it stands by and wakes again"
    else
        bad "a second standby did not wake: $(tail -2 "$WORK/after-standby2.txt" 2>/dev/null | tr '\n' ' ')"
    fi

    "$WORK/remote-docker" remote stop >/dev/null 2>&1
else
    bad "could not start a session for the standby test"
fi

# It reclaims itself, including a session never used: with no last-use time,
# one once reported zero idle and never expired.
if REMOTE_DOCKER_DAEMON_IDLE=8s "$WORK/remote-docker" remote start >/dev/null 2>&1; then
    reclaimed=false
    for _ in $(seq 1 6); do
        sleep 5
        if ! outputs "already running" "$WORK/remote-docker" remote start; then
            reclaimed=true
            "$WORK/remote-docker" remote stop >/dev/null 2>&1
            break
        fi
    done
    if [ "$reclaimed" = true ]; then
        ok "an unused background session reclaims itself"
    else
        bad "an unused background session never exited"
    fi
else
    bad "could not start a session for the idle test"
fi


echo
echo "== 17. the workspace lifecycle, and the docker context that follows it =="
# A docker context is a side effect of a workspace (ADR 0018): it appears and
# disappears WITH it. $HOME/.remote-docker.json is real state on this machine,
# so it is saved and restored before section 18: a default workspace changes
# what every later command resolves, which is why the rest of the suite runs on
# environment variables.
WSFILE="$HOME/.remote-docker.json"
WSBACKUP="$WORK/remote-docker.json.bak"
[ -f "$WSFILE" ] && cp "$WSFILE" "$WSBACKUP"

restore_ws() {
    if [ -f "$WSBACKUP" ]; then
        cp "$WSBACKUP" "$WSFILE"
    else
        rm -f "$WSFILE"
    fi
    hostdocker context rm -f itest-ws >/dev/null 2>&1 || true
}
trap 'cleanup; restore_ws' EXIT

if out=$("$WORK/remote-docker" remote create itest-ws --host 127.0.0.1 --port "$SSH_PORT" --user "$ACCOUNT" 2>&1); then
    ok "workspace create added a workspace"
else
    bad "workspace create failed: $(echo "$out" | tail -2)"
fi

if outputs '^itest-ws$' hostdocker context ls --format '{{.Name}}'; then
    ok "creating a workspace created its docker context"
else
    bad "no docker context appeared for the workspace"
fi

# Captured: this fails intermittently in CI, so a failure prints what ls said
# and what the file it reads holds.
wsls=$("$WORK/remote-docker" remote ls 2>&1)
if echo "$wsls" | grep -q "itest-ws"; then
    ok "workspace ls shows it"
else
    bad "workspace ls did not show the workspace; it said: $(echo "$wsls" | tr '\n' ' ')"
    info "$WSFILE holds: $(cat "$WSFILE" 2>&1 | tr '\n' ' ')"
fi

# inspect is the one place the four derivations meet: the config file's view,
# the endpoint derived from the name, the context named after it, and whether
# anything is serving it.
inspected=$("$WORK/remote-docker" remote inspect itest-ws 2>&1)
if echo "$inspected" | grep -q "docker context" && echo "$inspected" | grep -q "endpoint"; then
    ok "workspace inspect reports the endpoint and the docker context together"
else
    bad "workspace inspect was incomplete: $(echo "$inspected" | tr '\n' ' ')"
fi

if used=$("$WORK/remote-docker" remote use itest-ws 2>&1) &&
   outputs '\*itest-ws' "$WORK/remote-docker" remote ls; then
    ok "workspace use makes it the default"
else
    bad "workspace use did not set the default"
    info "use said: $(echo "$used" | tr '\n' ' ')"
    info "ls said: $(echo "$LAST_OUTPUT" | tr '\n' ' ')"
fi

# And docker's own current context: only this binary reads our default, and
# compose, buildx and the rest resolve `currentContext`. Asked of the CONTEXT
# STORE, because the exported DOCKER_HOST would mask the selection.
current=$(hostdocker context show 2>/dev/null)
if [ "$current" = "itest-ws" ]; then
    ok "workspace use selects the docker context too"
else
    bad "docker's current context is $current, want itest-ws"
fi

# A context we did NOT create must be left entirely alone. Arranging the
# endpoint by setting DOCKER_HOST, which outranks --context, silently resolved
# every foreign context to us, and nothing inside the process can see that: the
# command must FAIL against a daemon that is not there. The context must EXIST,
# or naming it fails anyway and reports a pass.
if ! hostdocker context create itest-foreign --docker host=tcp://127.0.0.1:1 >/dev/null 2>&1; then
    bad "could not create a foreign context, so nothing was asked of one"
elif out=$(timeout 30 env -u DOCKER_HOST "$WORK/remote-docker" --context itest-foreign ps 2>&1); then
    bad "a foreign context was redirected to our daemon"
    info "output: $(echo "$out" | head -2 | tr '\n' '; ')"
else
    ok "a docker context we did not create is left alone"
fi
hostdocker context rm -f itest-foreign >/dev/null 2>&1

# And ours, named explicitly, reaches the workspace it names rather than the
# default one.
if out=$(timeout 60 env -u DOCKER_HOST "$WORK/remote-docker" --context itest-ws ps 2>&1); then
    ok "--context <ours> reaches that workspace"
else
    bad "--context itest-ws did not work: $(echo "$out" | tail -2)"
fi

# The old verbs are aliases, not history: something out there is scripted
# against them.
if outputs "itest-ws" "$WORK/remote-docker" remote list; then
    ok "the old verb 'list' still works"
else
    bad "the list alias stopped working"
fi

if out=$("$WORK/remote-docker" remote rm itest-ws 2>&1); then
    ok "workspace rm removed it"
else
    bad "workspace rm failed: $(echo "$out" | tail -2)"
fi

# rm's own account, printed either way: a context never recognised as ours, a
# refused docker command, and one removed then recreated look alike from outside.
info "workspace rm said: $(echo "$out" | tr '\n' '; ')"

if outputs '^itest-ws$' hostdocker context ls --format '{{.Name}}'; then
    bad "the docker context outlived the workspace"
    info "context metadata: $(hostdocker context inspect itest-ws --format '{{.Metadata.Description}}' 2>&1 | tr '\n' ' ')"
else
    ok "removing the workspace removed its docker context"
fi

# `remote` must be FINDABLE: the root's help is sixty commands long, and an
# unlisted command is one nobody types.
if outputs '^  remote ' "$WORK/remote-docker" --help; then
    ok "remote is listed in the help"
else
    bad "remote is missing from the help, so nothing points at it"
fi

restore_ws
trap cleanup EXIT

echo
echo "== 18. the client under the name docker =="
# A machine with no Docker installed types `docker run`. With NO DOCKER_HOST and
# no session, as after renaming the binary, so resolving the workspace, starting
# a session and pointing the CLI at it all happen in this one command.
ALIASDIR="$WORK/aliasbin"
mkdir -p "$ALIASDIR"
ln -sf "$WORK/remote-docker" "$ALIASDIR/docker"

"$WORK/remote-docker" remote stop >/dev/null 2>&1 || true

if out=$(cd "$PROJECT" && env -u DOCKER_HOST PATH="$ALIASDIR:$PATH" \
        timeout "$DOCKER_TIMEOUT" docker run --rm -v "$PROJECT:/w" alpine:3 cat /w/marker 2>&1) &&
    echo "$out" | grep -q "from the project directory"; then
    ok "docker run works under the plain name, with no DOCKER_HOST and no session"
else
    bad "docker under its own name failed: $(echo "$out" | tail -3 | tr '\n' ' ')"
fi

if env -u DOCKER_HOST PATH="$ALIASDIR:$PATH" timeout 60 docker ps >/dev/null 2>&1; then
    ok "and so does docker ps"
else
    bad "docker ps failed under the plain name"
fi

# A docker command that reaches no daemon must not open a session: once
# `docker` on PATH is us, `remote create` writing a context spawns us, and a
# session means SSH, an NFS server and a reverse tunnel for a line of JSON.
# Asserted through `stop`, which says "not running" when there is nothing.
if outputs "stopped" "$WORK/remote-docker" remote stop; then
    ok "the session the alias started was there to stop"
else
    bad "no session was running after docker run under the alias"
fi
env -u DOCKER_HOST PATH="$ALIASDIR:$PATH" timeout 60 docker context ls >/dev/null 2>&1
if outputs "not running" "$WORK/remote-docker" remote stop; then
    ok "docker context ls started no session"
else
    bad "a command that reaches no daemon opened a session anyway"
    "$WORK/remote-docker" remote stop >/dev/null 2>&1
fi

# A COPY named `docker`, the documented installation (ADR 0024). A copy as well
# as the symlink above, because a symlink can be resolved back to the original.
COPYDIR="$WORK/copybin"
mkdir -p "$COPYDIR"
cp "$WORK/remote-docker" "$COPYDIR/docker"
if env -u DOCKER_HOST timeout 60 "$COPYDIR/docker" version --format '{{.Client.Version}}' >/dev/null 2>&1; then
    ok "a copy of the binary named docker is a working docker CLI"
else
    bad "the renamed copy did not run"
fi

# And it still finds our own commands, under the name the reader typed.
if env -u DOCKER_HOST timeout 60 "$COPYDIR/docker" remote version >/dev/null 2>&1; then
    ok "the renamed copy still carries the remote commands"
else
    bad "remote is unreachable from the renamed copy"
fi

"$WORK/remote-docker" remote stop >/dev/null 2>&1 || true

echo
echo "== 19. through a real reverse proxy, over wss =="
# A workspace reached through an ordinary HTTPS reverse proxy, no SSH port: the
# upgrade, the TLS the agent does not do, and a long-lived connection. nginx on
# the host network with a certificate generated here; the agent serves plain ws.
if command -v openssl >/dev/null 2>&1; then
    mkdir -p "$WORK/proxy"
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
        -keyout "$WORK/proxy/key.pem" -out "$WORK/proxy/cert.pem" \
        -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
        >/dev/null 2>&1
    chmod 644 "$WORK/proxy/key.pem" "$WORK/proxy/cert.pem"

    cat >"$WORK/proxy/nginx.conf" <<'NGINX'
events {}
http {
    server {
        listen 8443 ssl;
        ssl_certificate     /etc/proxy/cert.pem;
        ssl_certificate_key /etc/proxy/key.pem;

        location / {
            proxy_pass http://127.0.0.1:22280;
            proxy_http_version 1.1;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection "upgrade";
            proxy_set_header Host $host;
            # A tunnel is one request that lasts as long as the session, and a
            # proxy that times it out looks exactly like the workspace dying.
            proxy_read_timeout 3600s;
            proxy_send_timeout 3600s;
        }
    }
}
NGINX

    hostdocker rm -f itest-proxy >/dev/null 2>&1
    if hostdocker run -d --name itest-proxy --network host \
        -v "$WORK/proxy:/etc/proxy:ro" \
        -v "$WORK/proxy/nginx.conf:/etc/nginx/nginx.conf:ro" \
        nginx:alpine >/dev/null 2>&1; then
        ok "a reverse proxy is in front of the workspace"
    else
        bad "could not start the reverse proxy"
    fi

    for _ in $(seq 1 30); do
        curl -sk https://127.0.0.1:8443/ >/dev/null 2>&1 && break
        sleep 1
    done

    # A session of its own: this machine's reverse-tunnel port (ADR 0029) is
    # reserved by one session at a time (ADR 0028).
    wsenv() {
        env REMOTE_DOCKER_HOST="wss://localhost:8443/tunnel" \
            REMOTE_DOCKER_PORT= \
            REMOTE_DOCKER_CA_FILE="$WORK/proxy/cert.pem" \
            REMOTE_DOCKER_ENDPOINT="$WORK/ws.sock" \
            "$@"
    }

    if outputs "tunnel port" wsenv timeout 90 "$WORK/remote-docker" remote status; then
        ok "the workspace answers over wss, through the proxy"
    else
        bad "no answer over wss: $(echo "$LAST_OUTPUT" | tail -2 | tr '\n' ' ')"
    fi

    # The reverse forward is the half a proxy is most likely to break: it is the
    # direction the workspace opens, carrying NFS back to this machine.
    wsenv "$WORK/remote-docker" remote start --foreground >"$WORK/ws-up.log" 2>&1 &
    WS_PID=$!
    if wait_endpoint "$WORK/ws.sock" "$WS_PID"; then
        ok "the endpoint came up over wss"
        if out=$(timeout 120 docker -H "unix://$WORK/ws.sock" run --rm \
            -v "$PROJECT:/w" alpine:3 cat /w/marker 2>&1); then
            if [ "$out" = "from the project directory" ]; then
                ok "a bind mount resolves through the proxy, so the reverse forward works"
            else
                bad "the container read [$out] through the proxy"
            fi
        else
            bad "the container failed through the proxy: $(echo "$out" | tail -1)"
        fi
    else
        bad "the endpoint never came up over wss"
        sed 's/^/        /' "$WORK/ws-up.log"
    fi

    # A proxy that stops passing traffic without closing anything is invisible
    # to TCP keepalives, hence wslisten's pings; pausing the container does
    # exactly that. A container holding a mount keeps the connection OPEN
    # (test/nfs-resilience.sh section 4): released as idle after
    # REMOTE_DOCKER_IDLE_TIMEOUT, it would close cleanly and leave nothing to notice.
    timeout 60 docker -H "unix://$WORK/ws.sock" run -d --name itest-ws-hold \
        -v "$PROJECT:/w" alpine:3 sh -c "$PIN_SH" >/dev/null 2>&1

    hostdocker pause itest-proxy >/dev/null 2>&1
    info "the proxy is paused; waiting for the agent to notice"
    sleep 75
    if outputs "stopped answering" hostdocker logs "$CONTAINER"; then
        ok "the agent dropped the silent websocket, so its port is free again"
    else
        bad "the agent did not notice a websocket that stopped answering"
        info "without this a vanished client keeps its reverse-tunnel port"
    fi
    hostdocker unpause itest-proxy >/dev/null 2>&1
    timeout 60 docker -H "unix://$WORK/ws.sock" rm -f itest-ws-hold >/dev/null 2>&1

    stop_pid "$WS_PID"
    hostdocker rm -f itest-proxy >/dev/null 2>&1
else
    info "no openssl on this runner; the proxy section did not run"
fi

echo
echo "== 20. the workspace daemon comes back from an unclean restart =="
# LAST on purpose: it kills the workspace container.
# The shared daemon's exec-root is in the container's writable layer, so a
# stale containerd.pid outlives an unclean end and stops dockerd two ways
# (agent/internal/daemons.ExecRoot). Arranged, as per-user-dind.sh section 13
# does: a stale pid stops dockerd only while the pid it names is alive, and
# pid 1, the agent, always is.
EXECROOT=/var/run/docker
PIDFILE=$EXECROOT/containerd/containerd.pid

# The mechanism, asserted apart from the outcome: a daemon that happened to
# start would otherwise hide a missing tmpfs.
if outputs '^tmpfs$' hostdocker exec "$CONTAINER" stat -f -c %T "$EXECROOT"; then
    ok "the shared daemon's exec-root is a tmpfs, so nothing in it survives a restart"
else
    bad "the exec-root is not a tmpfs: [$LAST_OUTPUT]"
fi

# And a mount of its OWN, not a directory on a /run somebody else made a tmpfs:
# st_dev against the parent, as supervise.mountedAt asks.
if outputs '^differ$' hostdocker exec "$CONTAINER" sh -c \
        "if [ \"\$(stat -c %d $EXECROOT)\" = \"\$(stat -c %d $EXECROOT/..)\" ]; then echo same; else echo differ; fi"; then
    ok "the exec-root is a mount of its own, not a directory on its parent"
else
    bad "nothing is mounted at $EXECROOT: [$LAST_OUTPUT]"
fi

if ! planted=$(hostdocker exec "$CONTAINER" \
        sh -c "mkdir -p $(dirname "$PIDFILE") && echo 1 >$PIDFILE && cat $PIDFILE" 2>&1); then
    bad "could not plant a stale containerd pid in the workspace: [$planted]"
else
    # -t 0 is SIGKILL, the unclean end: a clean stop removes the pid file. The
    # writable layer is reused, which a restart on Kubernetes would not do.
    if hostdocker restart -t 0 "$CONTAINER" >/dev/null 2>&1; then
        ok "the workspace container was killed and started again on the same layer"
    else
        bad "could not restart the workspace container"
    fi

    if wait_parent_dockerd; then
        ok "the shared daemon came back with a stale $PIDFILE behind it"
    else
        # wait_parent_dockerd reports its own failure.
        dump_workspace_log 60
    fi

    # The file now holds the new containerd's pid or is absent; a `1` means
    # the directory survived.
    if outputs '^1$' hostdocker exec "$CONTAINER" cat "$PIDFILE"; then
        bad "the planted containerd pid survived the restart: [$LAST_OUTPUT]"
    else
        ok "the planted containerd pid did not survive the restart"
    fi

    if outputs '^tmpfs$' hostdocker exec "$CONTAINER" stat -f -c %T "$EXECROOT"; then
        ok "the exec-root is a tmpfs again after the restart"
    else
        bad "the exec-root is not a tmpfs after the restart: [$LAST_OUTPUT]"
    fi
fi

if [ "$FAIL" -ne 0 ]; then
    echo
    echo "== client log =="
    sed 's/^/        /' "$WORK/up.log"
    echo
    dump_workspace_log 40
fi

summary
