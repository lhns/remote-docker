#!/usr/bin/env bash
# End-to-end: a real workspace container, a real client binary, real NFS.
#
# This is the only place the whole thing is exercised together. Everything
# below the client is unit tested -- the NFS wire protocol against a real NFS
# client, the proxy against real HTTP framing -- but the kernel NFS client,
# the dind daemon, and the tunnel between them exist only here.
#
# Requires: docker, and a kernel with NFS client support. The nfs-capability
# job in .github/workflows/integration.yml checks that separately, because a
# failure there is about the runner rather than about this code.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-itest
SSH_PORT=22222
WS_PORT=22280
ACCOUNT=itest

# Every docker command that crosses the proxy is wrapped in a timeout. A
# container whose volume mount never completes would otherwise block forever,
# burning the whole CI budget and reporting nothing about where it stopped.
DOCKER_TIMEOUT=120
dockert() { timeout "$DOCKER_TIMEOUT" docker "$@"; }

# PIN_SH keeps a container alive only while its mount still works, so the
# container's own survival IS the assertion. Used by three sections, all of
# which depend on /w/marker being the same file -- which is why it is written
# once rather than three times.
#
# The script only, not the `sh -c`: quoting a whole command in one variable
# passes it as a single argument.
PIN_SH='while true; do cat /w/marker >/dev/null || exit 1; sleep 1; done'

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

# expect_output runs a container and compares its stdout to a literal.
#
# Seven copies of this shape existed, each nine lines, and each reported an
# empty capture as a content mismatch -- "got []" -- when it actually means the
# container produced no answer at all. That distinction has cost real time
# three times in this suite's history, so it is the reason the helper exists.
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

cleanup() {
    echo
    echo "== cleanup =="
    if [ -n "${CLIENT_PID:-}" ]; then
        kill "$CLIENT_PID" 2>/dev/null
        wait "$CLIENT_PID" 2>/dev/null
    fi
    hostdocker rm -f "$CONTAINER" >/dev/null 2>&1
    rm -rf "$WORK"
}
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
# Idle release is a real behaviour worth testing (ADR 0015), but the default
# minute meant sleeping 75 seconds to observe it -- a quarter of this suite's
# runtime spent waiting for a timer we control.
export REMOTE_DOCKER_IDLE_TIMEOUT=8s

echo
echo "== 3. enrol this machine =="
mkdir -p "$WORK/keys" "$WORK/wsstate"
if enrol "$ACCOUNT" "$REMOTE_DOCKER_STATE_DIR"; then
    ok "keypair generated and staged as $ACCOUNT.pub"
else
    bad "enroll produced no public key"
    exit 1
fi

echo
echo "== 4. start the workspace =="
# Pinned to the SHARED daemon, explicitly, now that a daemon per account is the
# default (ADR 0019). Two suites, one per mode, and each asks for its own:
# this one is the evidence that the shared mode still works, and
# test/per-user-dind.sh covers the other with the second account that mode is
# actually about.
#
# Inheriting the default would have quietly turned this into a second, worse
# test of per-user mode: several assertions below reach the client's containers
# with `docker exec <workspace> docker ps`, which only finds them on the daemon
# the agent itself runs.
# The WebSocket listener is published as well, so section 19 can put a real
# reverse proxy in front of it. The agent serves it by default; nothing else in
# this suite touches it.
# WORKSPACE_DIND_MOUNTS declares which paths the workspace's own daemon
# resolves, which section 9d then binds (ADR 0041). This suite runs the SHARED
# daemon, so the source side is what the daemon sees -- there is no dind to
# mount into -- and both of these exist inside the workspace container.
if start_workspace false -p "$WS_PORT:2280"     -e WORKSPACE_DIND_MOUNTS=/etc/workspace:/etc/workspace:ro,/etc/hostname:/etc/hostname:ro; then
    ok "workspace container started"
else
    bad "workspace container failed to start"
    exit 1
fi

info "waiting for the account to be provisioned"
if wait_provisioned "$ACCOUNT"; then
    ok "the agent provisioned the account"
else
    bad "the account was never provisioned"
    dump_workspace_log 30
    exit 1
fi

info "waiting for dockerd inside the workspace"
wait_parent_dockerd

echo
echo "== 5. status =="
PROJECT="$WORK/project"
OUTSIDE="$WORK/elsewhere"
mkdir -p "$PROJECT" "$OUTSIDE"
echo "from the project directory" >"$PROJECT/marker"
echo "from an unrelated directory" >"$OUTSIDE/data"

cd "$PROJECT" || exit 1
# Wrapped in a timeout: a command that hangs here reports where it stopped
# instead of consuming the whole job budget in silence. This caught a real
# deadlock -- Close waiting on background goroutines that only stopped when
# the caller's context was cancelled, which for a one-shot command it never
# was.
# No session is running yet, so the verdict is "no session" and that is
# correct. What this proves is that the workspace answered: the account row
# only exists when it did.
if timeout 90 "$WORK/remote-docker" remote status >"$WORK/status.log" 2>&1 &&
    grep -q "^status " "$WORK/status.log" &&
    grep -q "tunnel port" "$WORK/status.log"; then
    ok "status reports a verdict and the workspace parameters"
    sed 's/^/        /' "$WORK/status.log"
else
    bad "status failed"
    sed 's/^/        /' "$WORK/status.log"
    hostdocker logs "$CONTAINER" 2>&1 | tail -20
    exit 1
fi

# The agent's build, which is the question worth being able to answer when a
# workspace behaves oddly and is not the same question as the client's build.
# It is reported even when the workspace is too old to send one, because
# silence there is indistinguishable from a failure to answer.
if grep -qE "^versions .*agent [^ ,]+" "$WORK/status.log"; then
    ok "status reports the agent's version"
else
    bad "status did not report the agent version"
fi

echo
echo "== 6. open a session =="
# --foreground because the suite wants the session as a child it can kill and
# whose log it can read. `start` on its own detaches, which is right for a
# person and wrong for a test that needs to end it deterministically.
"$WORK/remote-docker" remote start --foreground >"$WORK/up.log" 2>&1 &
CLIENT_PID=$!

ready=false
for _ in $(seq 1 60); do
    if [ -S "$REMOTE_DOCKER_ENDPOINT" ] && docker -H "unix://$REMOTE_DOCKER_ENDPOINT" info >/dev/null 2>&1; then
        ready=true
        break
    fi
    kill -0 "$CLIENT_PID" 2>/dev/null || break
    sleep 1
done
if [ "$ready" = true ]; then
    ok "the local Docker endpoint answers"
else
    bad "the Docker endpoint never came up"
    sed 's/^/        /' "$WORK/up.log"
    exit 1
fi

export DOCKER_HOST="unix://$REMOTE_DOCKER_ENDPOINT"

info "pulling test images through the workspace"
for image in alpine:3 nginx:alpine; do
    timeout 300 docker pull -q "$image" >/dev/null 2>&1         || info "could not pre-pull $image; the test may be slower"
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
echo "== 7. a bind mount under the working directory =="
expect_output "the container read this machine's file through the tunnel" "from the project directory" -- --rm -v "$PROJECT:/w" alpine:3 cat /w/marker

echo
echo "== 8. a bind mount OUTSIDE the working directory =="
# The case the previous single-mount design could not express at all.
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
# `-v src:/w:ro` says the container must not write. Every bind here is rewritten
# into an NFS volume the workspace daemon mounts for itself (ADR 0006), and the
# export is read-write, so the ONLY thing standing between a container and this
# machine's files is that the read-only flag survived the rewrite and the daemon
# honoured it.
#
# Section 9 above is the control: writes do land when they are allowed, so a
# pass here is the flag working rather than writes being broken.
#
# Both spellings, because they take different paths through the rewriter: `-v`
# arrives as HostConfig.Binds, a string whose options are carried verbatim, and
# `--mount` as HostConfig.Mounts, a JSON object whose ReadOnly field has to
# survive the type being changed from bind to volume.
before=$(ls "$PROJECT" | sort | tr '
' ' ')

if dockert run --rm -v "$PROJECT:/w:ro" alpine:3         sh -c 'echo nope > /w/ro-v' >/dev/null 2>&1; then
    bad "a container wrote through a -v ...:ro mount"
else
    ok "-v with :ro refused the write"
fi

if dockert run --rm --mount "type=bind,source=$PROJECT,target=/w,readonly" alpine:3         sh -c 'echo nope > /w/ro-mount' >/dev/null 2>&1; then
    bad "a container wrote through a --mount readonly mount"
else
    ok "--mount with readonly refused the write"
fi

# The assertion that matters. A refused command proves the daemon reported an
# error; only the directory proves nothing reached this machine.
after=$(ls "$PROJECT" | sort | tr '
' ' ')
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
    bad "a read-only mount could not be read: $(echo "$out" | tail -2 | tr '
' ' ')"
fi

echo
echo "== 9c. a single file can be bind mounted =="
# `-v ./nginx.conf:/etc/nginx/nginx.conf` is ordinary in compose and was refused
# outright until ADR 0039. A file has no directory to export, so the client
# exports a SYNTHESISED directory holding only that file and the mount names it
# as a volume subpath. Two things have to hold, and only a real daemon can say:
# the subpath resolves to a FILE rather than a directory, and it resolves out of
# an NFS-backed volume.
mkdir -p "$PROJECT/conf"
echo "the file the container asked for" >"$PROJECT/conf/wanted.conf"
echo "TOKEN=secret" >"$PROJECT/conf/sibling.env"

# One run answers three questions, because each needs a container start: the
# target is a FILE (a volume mounted whole would put a directory there, which
# looks fine until something opens it), it holds what this machine holds, and
# the sibling that was never asked for did not come with it.
out=$(dockert run --rm -v "$PROJECT/conf/wanted.conf:/etc/app.conf" alpine:3 sh -c '
    test -f /etc/app.conf && echo is-a-file
    cat /etc/app.conf
    cat /etc/sibling.env 2>/dev/null' 2>&1)

if echo "$out" | grep -q "is-a-file"; then
    ok "the target is a file, not a directory"
else
    bad "the target is not a regular file: $(echo "$out" | head -2 | tr '
' ' ')"
fi
if echo "$out" | grep -q "the file the container asked for"; then
    ok "a single file mounted at the path the container asked for"
else
    bad "a single-file bind did not read back: $(echo "$out" | head -2 | tr '
' ' ')"
fi
if echo "$out" | grep -q "TOKEN=secret"; then
    bad "a file beside the exported one was reachable"
else
    ok "a sibling of the exported file did not come with it"
fi

# An edit here reaches the container, which is the case people actually want:
# edit nginx.conf, reload the service.
echo "edited after the mount" >"$PROJECT/conf/wanted.conf"
if out=$(dockert run --rm -v "$PROJECT/conf/wanted.conf:/etc/app.conf" alpine:3     cat /etc/app.conf 2>&1) && echo "$out" | grep -q "edited after the mount"; then
    ok "an edit on this machine is visible through a single-file mount"
else
    bad "the mount served a stale file: $(echo "$out" | head -2 | tr '
' ' ')"
fi

# Read-only has to survive this path too, for the reason section 9b gives: the
# export behind it is read-write.
if dockert run --rm -v "$PROJECT/conf/wanted.conf:/etc/app.conf:ro" alpine:3     sh -c 'echo nope > /etc/app.conf' >/dev/null 2>&1; then
    bad "a container wrote through a read-only single-file mount"
else
    ok "a read-only single-file mount refused the write"
fi
if [ "$(cat "$PROJECT/conf/wanted.conf")" = "edited after the mount" ]; then
    ok "the file on this machine is unchanged"
else
    bad "a read-only single-file mount let the file be rewritten"
fi

# The --mount spelling reaches the rewriter differently: a JSON object whose
# type changes from bind to volume, rather than a string that leaves Binds
# entirely.
if out=$(dockert run --rm --mount "type=bind,source=$PROJECT/conf/wanted.conf,target=/etc/app.conf"     alpine:3 cat /etc/app.conf 2>&1) && echo "$out" | grep -q "edited after the mount"; then
    ok "--mount of a single file works too"
else
    bad "--mount of a single file failed: $(echo "$out" | head -2 | tr '
' ' ')"
fi

echo
echo "== 9d. a bind may name a path the workspace owns =="
# kind builds `-v /lib/modules:/lib/modules:ro` itself, and its flags are not the
# user's to edit. A path the workspace declared is therefore resolved by the
# DAEMON rather than exported from this machine (ADR 0041).
#
# /etc/workspace exists in the workspace container and NOT on this runner, so a
# successful read is the passthrough working: had the client tried to export it,
# there would be nothing here to export.
if out=$(dockert run --rm -v /etc/workspace:/w:ro alpine:3 ls /w 2>&1) &&
    echo "$out" | grep -q "authorized_keys.d"; then
    ok "a declared path was resolved by the workspace"
else
    bad "a declared path did not resolve: $(echo "$out" | head -2 | tr '
' ' ')"
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

reachable=false
for _ in $(seq 1 45); do
    if curl -fsS --max-time 3 http://127.0.0.1:18080/ 2>/dev/null | grep -q "served from the client"; then
        reachable=true
        break
    fi
    sleep 1
done
if [ "$reachable" = true ]; then
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

# The clash moved here: the daemon no longer refuses anything, so the client
# has to, in the wording the daemon itself uses, because that is what it replaces.
#
# Two accounts colliding cannot be shown from one client, because this refusal
# comes first. What is proven above is that no requested number is ever bound
# on the workspace, which is what makes that collision impossible.
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
if ! twice=$(dockert run -d --name itest-twice -p 18082:80 -p 18083:80     -v "$PROJECT:/usr/share/nginx/html" nginx:alpine 2>&1); then
    # head, not tail: docker ends a failure with "Run 'docker run --help' for
    # more information", so the last line is boilerplate and the first is what
    # went wrong. Taking the last one cost a CI round trip.
    bad "a container publishing one port twice was refused: $(echo "$twice" | head -2 | tr '
' ' ')"
fi

for port in 18082 18083; do
    reachable=false
    for _ in $(seq 1 45); do
        if curl -fsS --max-time 3 "http://127.0.0.1:$port/" 2>/dev/null | grep -q "served from the client"; then
            reachable=true
            break
        fi
        sleep 1
    done
    if [ "$reachable" = true ]; then
        ok "one container port published twice is reachable at $port"
    else
        bad "$port never became reachable"
        # What the daemon published and what this machine opened, which is the
        # pair that has to line up. Printed because the failure is otherwise
        # one line with nothing to act on.
        dockert port itest-twice 2>&1 | sed 's/^/        published: /'
        dockert ps --all --filter name=itest-twice --format '{{.Status}}' 2>&1 | sed 's/^/        state: /'
        sed 's/^/        /' "$WORK/up.log" | tail -8
    fi
done
docker rm -f itest-twice >/dev/null 2>&1

echo
echo "== 10b. a published UDP port answers here =="
# SSH forwards TCP, so this was unreachable until ADR 0038 put a length in
# front of each datagram and carried them in a channel of their own. What is
# asserted is the round trip: a datagram sent to the port asked for HERE
# reaches a container in the workspace and its answer comes back.
#
# The probe is both ends deliberately. Nothing in alpine echoes UDP, and the
# two netcats disagree about -u and -w, so a test built on whichever one a
# runner has fails for a reason it is not about.
if ! (cd "$REPO/core" && CGO_ENABLED=0 GOOS=linux go build -o "$PROJECT/udpecho" ./probes/udpecho); then
    bad "could not build the udp echo probe"
elif ! dockert run -d --name itest-udp -p 15353:5353/udp     -v "$PROJECT:/probe:ro" alpine:3 /probe/udpecho :5353 >"$WORK/udp-run.log" 2>&1; then
    bad "the udp echo container did not start: $(tail -2 "$WORK/udp-run.log" | tr '
' ' ')"
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
    answered=false
    for _ in $(seq 1 45); do
        reply=$("$PROJECT/udpecho" send 127.0.0.1:15353 "through the tunnel" 2>/dev/null)
        if [ "$reply" = "through the tunnel" ]; then
            answered=true
            break
        fi
        sleep 1
    done

    if [ "$answered" = true ]; then
        ok "a datagram reached the container and its answer came back"
    else
        bad "no answer came back from 127.0.0.1:15353"
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
# The question that decides how honest the central claim can be. A real
# filesystem rather than a sync is only better than a sync if changes are
# NOTICED, and NFS carries no change notification -- so a watcher inside the
# container may see nothing while the file is plainly there.
#
# Two observations at once: inotify (what every hot-reload tool uses) and
# polling (the control). If polling sees nothing either, the mount is broken
# and the inotify result means nothing.
#
# See docs/adr/0014. This test records the behaviour rather than demanding a
# particular answer, so if a future change makes inotify work, it says so.
WATCHDIR="$WORK/watched"
mkdir -p "$WATCHDIR"

# No image build: a static binary placed on the share runs fine under plain
# alpine. That keeps this test about file watching rather than about whether
# `docker build` works through the proxy, and avoids shipping $WORK -- which
# holds the private key and a live socket -- as a build context.
if (cd "$REPO/core" && CGO_ENABLED=0 GOOS=linux go build -o "$PROJECT/watchprobe" ./probes/watchprobe); then
    # The error is NOT swallowed. The first run of this test produced an
    # empty log and no explanation, which cost a CI round trip to diagnose:
    # the binary was on the share with a synthesised mode 0644 and could not
    # be executed.
    if ! dockert run -d --name itest-watch             -v "$PROJECT:/probe:ro"             -v "$WATCHDIR:/data"             alpine:3 /probe/watchprobe /data >"$WORK/watch-run.log" 2>&1; then
        bad "the watch probe container would not start"
        sed 's/^/        /' "$WORK/watch-run.log"
        probe=""
    else
        # Wait for READY rather than sleeping. watchprobe prints it so that a
        # caller does not have to guess how long registering a watch takes,
        # and a change made before the watch lands proves nothing either way.
        # The two probe sections below already do this; this one slept five
        # seconds and hoped, which is a flake on a loaded runner.
        for _ in $(seq 1 30); do
            outputs '^READY' docker logs itest-watch && break
            sleep 1
        done

        echo "written on the client" >"$WATCHDIR/created-after-watch.txt"

        timeout 60 docker wait itest-watch >/dev/null 2>&1
        probe=$(docker logs itest-watch 2>&1)
    fi
    docker rm -f itest-watch >/dev/null 2>&1
    rm -f "$PROJECT/watchprobe"

    echo "        $(echo "$probe" | grep '^RESULT' || echo 'RESULT missing')"

    # An empty capture is NOT a negative result. The probe failing to start,
    # or producing nothing, used to fall through to the assertion below and be
    # reported as "the mount itself is not working" -- a confident diagnosis
    # pointing at NFS when the fault was `docker run`.
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
# 11b establishes that a client-side change notifies nobody. This asks the
# follow-up: if the AGENT performs a minimal syscall on the same file inside
# the workspace, does the container's watcher fire?
#
# Linux has no way to inject a synthetic inotify event -- fanotify(7) says so
# outright. The only mechanism available to anyone, Docker Desktop included,
# is to perform a real VFS operation and let the kernel emit the event as a
# side effect. So this measures WHICH operation produces WHICH event, one file
# per primitive so the correlation is by name rather than by timing.
#
# Nothing is asserted as pass/fail except the setup: the point is to record
# the matrix, and the design that follows depends on what it says.
POKEDIR="$WORK/poked"
mkdir -p "$POKEDIR"

if (cd "$REPO/core" && CGO_ENABLED=0 GOOS=linux go build -o "$PROJECT/watchprobe" ./probes/watchprobe &&
        CGO_ENABLED=0 GOOS=linux go build -o "$PROJECT/pokeprobe" ./probes/pokeprobe); then

    # The files each primitive acts on. Pre-created on the client where the
    # primitive needs an existing file; 'create' and 'unlink' are handled
    # separately below because their whole question is about a file that has
    # just appeared or just gone.
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
        # Wait for READY rather than sleeping. A poke delivered before the
        # watch lands proves nothing, and is an easy way to record a false
        # negative and believe it.
        watching=false
        for _ in $(seq 1 30); do
            if outputs '^READY' docker logs itest-poke; then
                watching=true
                break
            fi
            sleep 1
        done
        if [ "$watching" = true ]; then
            ok "the watcher is established"
        else
            bad "the watcher never reported READY"
        fi

        # Where the workspace has this share mounted. dockerd's local driver
        # mounts each rd-<id> NFS volume once and bind-mounts it into every
        # container using it -- and a bind mount shares the superblock, hence
        # the inode an inotify mark sits on. If that holds, poking the volume
        # mountpoint is seen by a watcher inside the container, with no
        # namespace entering at all.
        vol=$(docker inspect itest-poke \
            --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' 2>/dev/null)
        mp=$(docker volume inspect "$vol" --format '{{.Mountpoint}}' 2>/dev/null)
        pid=$(docker inspect itest-poke --format '{{.State.Pid}}' 2>/dev/null)
        info "volume=$vol mountpoint=$mp container pid=$pid"

        if [ -z "$mp" ]; then
            bad "could not resolve the volume mountpoint -- the rest of the matrix cannot run"
        else
            hostdocker cp "$PROJECT/pokeprobe" "$CONTAINER:/pokeprobe" >/dev/null 2>&1

            # Do the two views of the same file share a device? If st_dev
            # differs they are separate superblocks, separate inodes, and no
            # poke through the mountpoint can ever notify the container.
            dev_ws=$(hostdocker exec "$CONTAINER" /pokeprobe stat "$mp/poke-openclose.txt" 2>&1)
            dev_ct=$(docker exec itest-poke /probe/pokeprobe stat /data/poke-openclose.txt 2>&1)
            info "workspace: $dev_ws"
            info "container: $dev_ct"
            # Compares dev AND ino, which is the stronger claim and the one
            # that matters: an inotify mark lives on the inode, so identical
            # dev+ino means a poke through either path reaches the same mark.
            ids_ws=${dev_ws##*ok dev=}
            ids_ct=${dev_ct##*ok dev=}
            if [ "$ids_ws" != "$dev_ws" ] && [ "$ids_ws" = "$ids_ct" ]; then
                ok "the volume mountpoint and the container see the same inode"
            else
                ok "INODES DIFFER -- the design must use /proc/<pid>/root instead"
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
            # Whether this can ever fire IN_DELETE is the least certain row in
            # the matrix and decides whether deletes are representable at all.
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
            # dirmtime acts on the watched directory itself, which the probe
            # reports under the directory's own basename rather than a
            # filename -- that distinction is exactly what the coarse
            # fallback would rely on.
            if [ "$p" = dirmtime ]; then
                seen=$(echo "$poke" | grep "^INOTIFY .* data/$" | awk '{print $2}' | sort -u | tr '\n' ',' | sed 's/,$//')
            fi
            printf '        POKE-MATRIX %-10s %s\n' "$p" "${seen:-<nothing>}"
        done
        echo
        echo "        $(echo "$poke" | grep '^RESULT' || echo 'RESULT missing')"

        # The one row that is near-certain from kernel source, so a failure
        # here means the experiment itself is wrong rather than the answer
        # being no.
        if echo "$poke" | grep -q "^INOTIFY .*IN_CLOSE_WRITE.* poke-openclose\.txt$"; then
            ok "open(O_WRONLY)+close() reaches the container's watcher"
        else
            ok "open(O_WRONLY)+close() did NOT reach the watcher -- replay is not viable this way"
        fi
    fi
    rm -f "$PROJECT/watchprobe" "$PROJECT/pokeprobe"
else
    bad "could not build the probes"
fi

echo
echo "== 11c. idle release, and what must survive it =="
# Two claims, and the first one used to be untested while looking tested.
#
# The old probe container was created THROUGH this client, so it carried our
# owner label and held one of our volumes -- hasLiveDependents returned true on
# the first check and the connection was never released at all. The test then
# asserted the container was still alive, which it trivially was. The reconnect
# path ADR 0015 calls load-bearing was never exercised.
#
# So: first prove a release actually happens when nothing depends on us, then
# prove one does NOT happen while a container does.

# (a) nothing running -> the connection must be released and reopen on demand.
#
# 12 seconds against an 8-second timer: a 1.5x margin. It was 20, twice, which
# spent 40 seconds of every run waiting past a timeout the suite itself sets
# short at the top of this file precisely to avoid that.
sleep 12
expect_output "the client reconnects after an idle release" "after-idle" -- --rm alpine:3 echo after-idle

# (b) a container holding one of our volumes must pin the connection: it has a
# live NFS mount, and dropping the tunnel underneath gives it EIO. The loop
# exits non-zero if the mount stops working, so the container's own survival is
# the assertion.
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
echo "== 11d. one account cannot bind another's NFS port =="
# ADR 0010's entire justification, and untested until now. Under sshd this
# depended on a permitlisten string generated correctly into every key's
# authorized_keys; under the agent it is a comparison.
#
# Enrol a second account, then have it ask for the FIRST account's reverse
# port. It must be refused.
OTHER=itest2
cp "$REMOTE_DOCKER_STATE_DIR/id_ed25519.pub" "$WORK/keys/$OTHER.pub"

provisioned2=false
for _ in $(seq 1 90); do
    if hostdocker exec "$CONTAINER" id "rd-$OTHER" >/dev/null 2>&1; then
        provisioned2=true
        break
    fi
    sleep 1
done

if [ "$provisioned2" != true ]; then
    bad "the second account was never provisioned"
    hostdocker logs "$CONTAINER" 2>&1 | tail -15 | sed 's/^/        /' 
else
    # `status` prints a human table, not KEY=VALUE -- the wire format is what
    # the client parses, not what it displays.
    first_port=$(awk '/^account/ {print $NF}' "$WORK/status.log")
    if [ -z "$first_port" ]; then
        bad "could not determine the first account's port"
    else
        # -R on the OTHER account, targeting the FIRST account's port.
        hijack=$(timeout 30 ssh -i "$REMOTE_DOCKER_STATE_DIR/id_ed25519"             -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null             -o ExitOnForwardFailure=yes -o BatchMode=yes             -p "$SSH_PORT" -N -R "127.0.0.1:$first_port:127.0.0.1:1"             "$OTHER@127.0.0.1" 2>&1 </dev/null; echo "rc=$?")

        if echo "$hijack" | grep -q "rc=0"; then
            bad "SECURITY: $OTHER bound $ACCOUNT's NFS port $first_port"
        else
            ok "one account cannot bind another's NFS port"
        fi

        # And cannot DIAL it either, which is the other half and was missing.
        #
        # This suite runs the shared daemon (ADR 0012), where every account
        # lives in the agent's network namespace, so 127.0.0.1:<their port> is
        # genuinely reachable from another account's session. What answers is
        # an NFS export with AuthFlavorNull, so reaching it is read and write
        # access to the files on that person's machine.
        #
        # The forward has to be USED, not merely requested: ssh opens the local
        # listener straight away and asks for the channel only when something
        # connects, so a test that just starts `ssh -L` passes whatever the
        # server decides. The refusal appears on ssh's stderr as
        # "administratively prohibited" when the connection is made.
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

        # The dial refusal above covers ssh -L. It does not cover a shell:
        # with one daemon for everybody the export binds in the agent's own
        # network namespace, which is where every account's shell runs, so a
        # socket opened there passes through no forwarding policy. A daemon per
        # account (ADR 0019, the default) binds inside that account's namespace
        # instead, and test/per-user-dind.sh asserts a shell cannot reach it.
        #
        # Reported rather than asserted, because it follows from this mode
        # rather than from a defect in it. The threat model records it.
        shell_reach=$(timeout 60 ssh -i "$REMOTE_DOCKER_STATE_DIR/id_ed25519" \
            -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            -o BatchMode=yes -p "$SSH_PORT" "$OTHER@127.0.0.1" \
            "nc -w 2 127.0.0.1 $first_port </dev/null && echo CONNECTED || echo REFUSED" \
            2>/dev/null </dev/null | tr -d '\015')
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
# Compose is the reason ADR 0005 put the translation at the API rather than in
# a command wrapper: it speaks the Engine API directly and never shells out to
# `docker`, so a wrapper could not have covered it at all.
#
# It also exercises relative path resolution, which is the original bug
# (docker/compose#8484): compose expands ./html to an absolute path on THIS
# machine before sending it, and that path means nothing to the remote daemon
# until the proxy rewrites it.
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

    composed=false
    for _ in $(seq 1 45); do
        if curl -fsS --max-time 3 http://127.0.0.1:18081/ 2>/dev/null | grep -q "served by compose"; then
            composed=true
            break
        fi
        sleep 1
    done
    if [ "$composed" = true ]; then
        ok "a compose relative bind resolved and its port was forwarded"
    else
        bad "the compose service never served this machine's file"
        sed 's/^/        /' "$WORK/compose.log" | tail -20
    fi

    timeout 120 docker compose -f "$PROJECT/compose.yaml" down -v >/dev/null 2>&1         && ok "compose tore the stack down"         || bad "compose down failed"
else
    bad "compose up failed"
    sed 's/^/        /' "$WORK/compose.log" | tail -20
fi

echo
echo "== 12b. one compose service reaching another =="
# Container-to-container traffic never touches this client: it happens on the
# workspace's own docker network, between containers the workspace's own daemon
# started. But we rewrite every bind mount and forward every published port, so
# "did we disturb the network" is a fair question and it deserves an answer
# rather than an assurance.
#
# Four things at once, which is why this is one test and not four:
#   - `client` resolves `web` by SERVICE NAME, so compose's DNS works
#   - it connects to it over TCP, so the network works
#   - what `web` serves is a file from THIS machine, through a rewritten bind
#   - `client` writes the result to ANOTHER bind, which this test then reads
#     from the client side -- so the answer comes back over NFS rather than
#     being asserted inside the workspace where a mistake could hide
#
# depends_on + condition: service_healthy rather than a retry loop, because it
# is how somebody would actually write this, and because a loop that never
# succeeds looks the same as one that was never scheduled.
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
    reached=false
    for _ in $(seq 1 60); do
        if [ -f "$PROJECT/net/out/status" ] && grep -q reached "$PROJECT/net/out/status" 2>/dev/null; then
            reached=true
            break
        fi
        sleep 1
    done

    if [ "$reached" = true ]; then
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
# Sections 12 and 12b use the runner's own docker CLI. This one uses ours, and
# it is a different claim: that a machine with no docker installed at all can
# run `docker compose up`.
#
# ADR 0009 could not have this. Compose v2 pinned docker/cli back a major
# version and buildx back seven minors, which would have cost BuildKit, so the
# record said to revisit when compose finished the moby/moby migration. It has,
# and this is what checks that the two stay compatible: a version bump on
# either side that breaks the pairing fails here rather than in somebody's
# terminal.
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

    embedded=false
    for _ in $(seq 1 45); do
        if curl -fsS --max-time 3 http://127.0.0.1:18083/ 2>/dev/null | grep -q "served by the embedded compose"; then
            embedded=true
            break
        fi
        sleep 1
    done
    if [ "$embedded" = true ]; then
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
# This is the ONLY test of the agent's exec/pty session, and it is deliberately
# run with a stock ssh rather than anything of ours. `remote-docker shell` is
# gone (ADR 0018) but serveExec and servePTY are not, because an enrolled key
# still logging in is what server.go's argument for unrestricted local
# forwarding rests on -- everything reachable that way is inside the workspace,
# which the account can already reach with a shell. Delete this and ADR 0010's
# central claim, one binary replacing sshd, has no coverage at all.
#
# -tt forces a pty, so `tty` naming one proves the agent allocated it rather
# than falling through to the non-pty branch.
shellout=$(timeout 60 ssh -i "$REMOTE_DOCKER_STATE_DIR/id_ed25519" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o BatchMode=yes -tt -p "$SSH_PORT" "$ACCOUNT@127.0.0.1" \
    'tty; id -un; docker ps --format {{.Names}} 2>&1 | head -3' 2>&1 </dev/null)

# tr squeezes the pty's CRLF out so a failure prints as one readable line.
trim() { echo "$1" | tr -d '
' | tail -3 | tr '
' ' '; }

if echo "$shellout" | grep -q '/dev/pts/'; then
    ok "a stock ssh gets an interactive shell on a pty"
else
    bad "no pty from the agent: $(trim "$shellout")"
fi

# `id -un` names the UNIX user, which is not the account name: an enrolled
# `alice` logs in as `alice` and the unix user behind it is `rd-alice`
# (ADR 0025), so that the workspace does not take a name in the machine's own
# passwd file. This is the only assertion anywhere that sees the unix side, and
# it is the end-to-end proof of the prefix.
if echo "$shellout" | grep -q "^rd-$ACCOUNT"; then
    ok "the shell runs as the unix user behind the enrolled account"
else
    bad "the shell was not rd-$ACCOUNT: $(trim "$shellout")"
fi

# ...and can USE the shared daemon, which needs its supplementary groups.
#
# Go calls setgroups() with Credential.Groups whenever a Credential is set, so
# leaving it nil CLEARS every supplementary group. An account correctly listed
# in `docker` in /etc/group got a shell that was not in it, and `docker ps`
# answered "permission denied while trying to connect to the Docker daemon
# socket" -- which reads like a broken socket and is not one.
#
# Asserted by using it, not by reading `id`: the group file was checked, found
# right, and believed, while the shell had a different view of it.
if echo "$shellout" | grep -q "permission denied"; then
    bad "the shell cannot reach the shared daemon: $(trim "$shellout")"
else
    ok "and it can use the shared docker daemon"
fi

# The embedded CLI: the client's own docker, not the runner's.
if out=$(timeout 60 "$WORK/remote-docker" ps --format '{{.Names}}' 2>&1); then
    ok "the embedded docker CLI talks to the workspace"
else
    bad "the embedded docker CLI failed: $(echo "$out" | tail -2)"
fi

# `status` while `up` holds the export port. An account has exactly ONE
# reverse-tunnel port, so a session that needlessly reserves it fails the
# moment `up` is running -- which is exactly when someone would run another
# command. This is the case that broke.
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
# The question worth settling: a Dockerfile COPYs from the build context, and
# the context is on THIS machine while the daemon is on the workspace. Nothing
# is mounted for a build -- no volume, no NFS -- so the only reason this can
# work is that the docker CLI tars the directory and uploads it, which is what
# "Sending build context to Docker daemon" means.
#
# It does work, and this is the only test of it. Asserted through the CONTENT
# of a file written here, so a build that somehow read a stale or empty context
# fails rather than passing on an image that exists.
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
    # THE builder assertion: is this BuildKit, or the classic builder wearing
    # its name?
    #
    # `build` being present says nothing -- it was present before, and it
    # silently used the pre-BuildKit path because buildx was not vendored, even
    # with DOCKER_BUILDKIT=1. So the two builders are told apart by what they
    # SAY, and both directions are asserted: a fallback would keep passing
    # every other check in this section while quietly losing cache mounts,
    # parallel stages and incremental context transfer.
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
    bad "docker build failed: $(echo "$out" | tail -5 | tr '
' ' ')"
fi

# And the image is real: it runs, and what COPY put there is still there.
expect_output "the built image runs and carries the copied file"     "content-from-the-client-machine" -- --rm itest-build cat /marker.txt

# A file the context excludes must NOT reach the daemon. This is the only
# thing standing between a build and uploading whatever else is in the
# directory -- a .git, a node_modules, somebody's secrets.
echo "must-not-be-uploaded" >"$BUILDCTX/secret.txt"
printf 'secret.txt
' >"$BUILDCTX/.dockerignore"
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
    bad "the .dockerignore build failed: $(echo "$out" | tail -5 | tr '
' ' ')"
fi

timeout 60 docker rmi -f itest-build itest-ignore >/dev/null 2>&1

echo
echo "== 14. elevate =="
# Swarm cannot run privileged tasks, so the service starts unprivileged and
# relaunches itself (ADR 0013). Swarm itself is not worth standing up here --
# the mechanism under test is `docker run`, which is exactly what runs below.
#
# The assertion that matters is the last one: the privileged child must NOT
# inherit the host's Docker socket, or every enrolled workspace user has root
# on the node.
ELEV=remote-docker-elev
hostdocker rm -f "$ELEV" "$ELEV.elevated" >/dev/null 2>&1

if hostdocker run -d --name "$ELEV"         -v /var/run/docker.sock:/var/run/host-docker.sock         "$IMAGE" elevate >/dev/null 2>&1; then

    elevated=false
    for _ in $(seq 1 60); do
        if hostdocker inspect "$ELEV.elevated" >/dev/null 2>&1; then
            elevated=true
            break
        fi
        hostdocker inspect -f '{{.State.Running}}' "$ELEV" 2>/dev/null | grep -q true || break
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
# The payoff, and it goes last because it restarts the client.
#
# 11b shows a client-side change notifies nobody; 11d shows which syscall
# would; this runs the whole path -- client watcher, SSH channel, agent
# replay -- and asks whether an ordinary edit here is noticed there.
#
# It has to be the SAME client, not a second one alongside. A session is the file
# server for this account and there is one NFS port per account, so two would
# collide. And the connection is established on demand (ADR 0015), so a client
# that never issues a Docker request never connects and therefore never opens
# the notification channel at all -- which is why the probe container is
# started through this client rather than the previous one.
kill "$CLIENT_PID" 2>/dev/null
wait "$CLIENT_PID" 2>/dev/null
CLIENT_PID=""

REPLAYDIR="$WORK/replayed"
mkdir -p "$REPLAYDIR"
echo "before the watch" >"$REPLAYDIR/reloaded.txt"

if (cd "$REPO/core" && CGO_ENABLED=0 GOOS=linux go build -o "$PROJECT/watchprobe" ./probes/watchprobe); then
    REMOTE_DOCKER_WATCH=partial "$WORK/remote-docker" remote start --foreground >"$WORK/watch-up.log" 2>&1 &
    CLIENT_PID=$!

    ready=false
    for _ in $(seq 1 60); do
        if [ -S "$REMOTE_DOCKER_ENDPOINT" ] && docker info >/dev/null 2>&1; then
            ready=true
            break
        fi
        kill -0 "$CLIENT_PID" 2>/dev/null || break
        sleep 1
    done

    if [ "$ready" != true ]; then
        bad "the watching client never came up"
        sed 's/^/        /' "$WORK/watch-up.log"
    elif ! dockert run -d --name itest-replay             -v "$PROJECT:/probe:ro"             -v "$REPLAYDIR:/data"             alpine:3 /probe/watchprobe -timeout 45s /data >"$WORK/replay-run.log" 2>&1; then
        bad "the replay probe container would not start"
        sed 's/^/        /' "$WORK/replay-run.log"
    else
        ok "the watching client is serving"

        for _ in $(seq 1 30); do
            outputs '^READY' docker logs itest-replay && break
            sleep 1
        done

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
# Docker's own word for it, applied to the NFS mount: the container may cache
# read data and directory structure, so the kernel stops revalidating every
# attribute (ADR 0042). What makes that safe is the watcher, which is why this
# section runs against the watching client section 15 started and why asking
# for it without one is refused.
#
# The claim being tested is the pair: a long attribute cache AND an edit here
# still arriving. Either alone is easy and neither alone is the feature.
CACHEDIR="$WORK/cachedir"
mkdir -p "$CACHEDIR"
echo "first" >"$CACHEDIR/marker"

if [ -n "${CLIENT_PID:-}" ] && kill -0 "$CLIENT_PID" 2>/dev/null; then
    if dockert run -d --name itest-cached -v "$CACHEDIR:/w:cached"         alpine:3 sleep 300 >"$WORK/cached-run.log" 2>&1; then
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

        # The part a long attribute cache would break. actimeo=60 means the
        # kernel may trust what it has for a minute, so without the watcher's
        # poke this reads "first" until it expires.
        echo "second" >"$CACHEDIR/marker"
        seen=""
        for _ in $(seq 1 20); do
            seen=$(docker exec itest-cached cat /w/marker 2>&1)
            [ "$seen" = "second" ] && break
            sleep 1
        done
        if [ "$seen" = "second" ]; then
            ok "an edit here is visible through the cached mount"
        else
            bad "the cached mount still reads [$seen] after 20s"
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
echo "== 15c. the delegated consistency, which is a union =="
# Docker's word for a container-authoritative mount, and here it is a UNION the
# workspace mounts: this share's live NFS export underneath, a local cache on
# top, and the merged view the container binds (ADR 0044).
#
# The assertion that matters is the fallthrough. A file created here AFTER the
# cache was filled is not in the cache, and the container must still see it --
# that is what makes an incomplete cache correct, and it is the whole reason
# the cache can be filled in the background.
# Run against the WATCHING client section 15 started: invalidation rides the
# watcher, because a cached copy of a file that changed here is the one way this
# mode can be wrong rather than merely slow.
DELEGDIR="$WORK/delegated"
mkdir -p "$DELEGDIR"
echo "first" >"$DELEGDIR/marker"

if [ -n "${CLIENT_PID:-}" ] && kill -0 "$CLIENT_PID" 2>/dev/null; then
    if dockert run -d --name itest-deleg -v "$DELEGDIR:/w:delegated"         alpine:3 sleep 300 >"$WORK/deleg-run.log" 2>&1; then
        ok "a container starts against a delegated union"

        if outputs '^first$' docker exec itest-deleg cat /w/marker; then
            ok "it reads a file the cache was filled with"
        else
            bad "reading the cache: [$LAST_OUTPUT]"
        fi

        # And the cache really was filled, which the read above does NOT show:
        # a miss falls through to the live export and returns the same bytes,
        # so that assertion passes just as well with an empty cache. This is
        # also what write-back is gated on, so a fill that quietly did nothing
        # would otherwise present much later as a write that never arrived.
        filled=""
        for _ in $(seq 1 20); do
            if outputs "delegated: .* files, cached\$"                 "$WORK/remote-docker" remote status; then
                filled=yes
                break
            fi
            sleep 1
        done
        if [ -n "$filled" ]; then
            ok "the fill completed, so the share is cached rather than only live"
        else
            bad "the share never reported a complete cache: [$LAST_OUTPUT]"
        fi

        # THE assertion. This file did not exist when the cache was filled, so
        # it can only be coming from the live export underneath.
        # Retried, because two caches sit between the two sides -- the NFS
        # attribute cache under the union and libfuse's own entry cache, both
        # about a second -- so reading once measures those rather than the
        # fallthrough. The time it took is the answer to "when does a new file
        # appear", so it is reported.
        echo "arrived after the fill" >"$DELEGDIR/late.txt"
        fell=""
        for i in $(seq 1 15); do
            if outputs '^arrived after the fill$' docker exec itest-deleg cat /w/late.txt; then
                fell=$i
                break
            fi
            sleep 1
        done
        if [ -n "$fell" ]; then
            ok "a file the cache does not have falls through to the live export (${fell}s)"
        else
            bad "the union never fell through: [$LAST_OUTPUT]"
        fi

        # What the container mounts is the union, in the daemon's namespace,
        # rather than a volume of its own.
        if outputs '/run/rd-union/' docker inspect             -f '{{range .Mounts}}{{.Source}}{{end}}' itest-deleg; then
            ok "the container binds the union the workspace mounted"
        else
            bad "the mount source is [$LAST_OUTPUT], want a union"
        fi

        # And a write goes into the cache rather than through to this machine,
        # which is what "the container is authoritative" means until write-back
        # exists.
        docker exec itest-deleg sh -c 'echo "from the container" >/w/written-there' >/dev/null 2>&1
        if outputs '^from the container$' docker exec itest-deleg cat /w/written-there; then
            ok "the container can write into its own view"
        else
            bad "writing into the union: [$LAST_OUTPUT]"
        fi
        # Write-back: the container's write reaches this machine, because the
        # cache layer of an overlay IS the record of what it changed (ADR 0044).
        # Polled, so it takes a few seconds rather than being instant, which is
        # the cost of the mode and is stated as such.
        back=""
        for _ in $(seq 1 30); do
            [ -f "$DELEGDIR/written-there" ] && back=$(cat "$DELEGDIR/written-there") && break
            sleep 1
        done
        if [ "$back" = "from the container" ]; then
            ok "a container's write reaches this machine"
        else
            bad "the container's write never arrived here: [$back]"
            # What the session thinks it is doing. Write-back is gated on the
            # fill being complete, so the cache row says whether the gate is
            # the reason, and the log says whether anything was tried.
            "$WORK/remote-docker" remote status 2>&1 | grep -iE "cache|watch" | sed 's/^/        /'
            grep -iE "cache|union|wrote|writing back" "$WORK/watch-up.log" 2>/dev/null |
                tail -10 | sed 's/^/        /'
        fi

        # An edit here reaches the container, because the workspace writes it
        # THROUGH the union rather than into the layer underneath (ADR 0044).
        echo "edited here" >"$DELEGDIR/marker"
        seen=""
        for _ in $(seq 1 20); do
            seen=$(docker exec itest-deleg cat /w/marker 2>&1)
            [ "$seen" = "edited here" ] && break
            sleep 1
        done
        if [ "$seen" = "edited here" ]; then
            ok "an edit here reaches a running delegated container"
        else
            bad "the cache stayed stale after an edit: [$seen]"
        fi

        # And a DELETION, which no mode in this project has managed before: a
        # cached copy of a file that is gone would shadow its absence, and the
        # Docker API cannot remove a path from a volume at all.
        rm -f "$DELEGDIR/marker"
        gone=false
        for _ in $(seq 1 20); do
            if ! docker exec itest-deleg test -e /w/marker 2>/dev/null; then
                gone=true
                break
            fi
            sleep 1
        done
        if [ "$gone" = true ]; then
            ok "a file deleted here disappears from the container"
        else
            bad "a deleted file is still visible through the union"
        fi
    else
        bad "a container would not start against a delegated union"
        sed 's/^/        /' "$WORK/deleg-run.log"
        dump_workspace_log 40
    fi
    docker rm -f itest-deleg >/dev/null 2>&1
else
    bad "no client is running, so the union could not be tested"
fi

echo "== 16. a background session, with no terminal held open =="
# `start --foreground` IS the daemon body, so this is the same session the rest
# of the suite used -- started detached, stopped by asking rather than by
# signalling, and reclaiming itself when nothing needs it.
#
# The suite's own session is stopped first: one endpoint has one owner now,
# which is the point of the lock.
kill "$CLIENT_PID" 2>/dev/null
wait "$CLIENT_PID" 2>/dev/null
CLIENT_PID=""
sleep 2

if "$WORK/remote-docker" remote start >"$WORK/start.log" 2>&1; then
    ok "start returned without holding a terminal"
    sed 's/^/        /' "$WORK/start.log"
else
    bad "start failed"
    sed 's/^/        /' "$WORK/start.log"
fi

if out=$(dockert run --rm alpine:3 echo through-the-daemon 2>&1); then
    if [ "$out" = "through-the-daemon" ]; then
        ok "docker works through the background session"

        # And the verdict, which is the whole point of `status`. A session is
        # demonstrably up: the command above went through it.
        if outputs "^status  *ready" "$WORK/remote-docker" remote status; then
            ok "status says ready while a session is serving"
        else
            bad "status did not say ready: $(echo "$LAST_OUTPUT" | head -1)"
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

# The endpoint has exactly one owner, and it must refuse rather than steal --
# which on Unix it silently used to do, unlinking the socket and leaving the
# first session accepting on an inode nobody could reach.
#
if out=$("$WORK/remote-docker" remote start --foreground 2>&1); then
    bad "a second session took the endpoint from the running one"
else
    case "$out" in
        *"already serving"*) ok "a second session is refused, naming the owner" ;;
        *) bad "a second session failed for the wrong reason: $out" ;;
    esac
fi

# A detached container must outlive the command that started it. This is what
# the in-process session could not do: it died with the command and took the
# container's mount with it.
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

# The sequence a person actually types, with nothing between the commands.
#
# `start` returns only once the endpoint answers, and `stop` only once the
# process is gone -- so this must work with no sleep anywhere. It is the whole
# claim of both commands, and it was not tested: the assertions above prove
# `stop` SAYS "stopped", not that anything could run afterwards.
#
# `stop && start` is the half that used to race. stop waited for the endpoint
# to go quiet, which is where Session.Close STARTS its teardown -- the SSH
# connection, the reverse tunnel and the NFS export go afterwards. An account
# has exactly one export port (ADR 0003), so a start that overtook the release
# failed on a port the workspace had not let go of yet.
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

# A session built from a different build is replaced when that costs nothing,
# and reported when it does not. A stale session serves the endpoint, so an
# updated client talks to the OLD build and behaves like it -- silently, until
# something it should have fixed does not work.
#
# Two binaries, same source, different stamps: the versions cannot be ordered
# and nothing here tries to.
#
# Named for what it is rather than "-old". It is not an older build, it is THIS
# build wearing another version, and the name saying otherwise is what made a
# rename of the whole command shape skip straight past it -- it does not match
# "remote-docker", so it kept calling commands that had moved.
if (cd "$REPO/client" && CGO_ENABLED=0 go build -ldflags="-X main.version=sha-otherbuild"         -o "$WORK/remote-docker-otherbuild" ./cmd/remote-docker); then

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
            *"different version"*) ok "a session in use from another commit is reported, not restarted" ;;
            *) bad "no version warning while a container depended on the old session" ;;
        esac
        # Captured rather than piped, so a failure can show what status said.
        # Which build is serving is exactly the question here, and "it did not
        # match" without the output leaves nothing to reason from.
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

        "$WORK/remote-docker" rm -f itest-pin >/dev/null 2>&1
    else
        bad "could not start the pinning container"
    fi
    "$WORK/remote-docker" remote stop >/dev/null 2>&1
else
    bad "could not build a second client for the version test"
fi

# It reclaims itself. A session that has never been used is the case that
# should go soonest, and the one that used to be unable to: with no last-use
# time, it reported zero idle and could never expire.
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
# `workspace` and `context` used to be two commands doing one job (ADR 0018).
# A context is a side effect now, so the thing to prove is that it appears and
# disappears WITH the workspace rather than on request.
#
# The config file is $HOME/.remote-docker.json, which is real state on whatever
# machine this runs on, so it is saved and put back. Everything else in the
# suite is driven by environment variables, and this section runs last for the
# same reason: setting a default workspace changes what every other command
# would resolve.
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

# Captured, not just tested. This assertion failed intermittently in CI and
# said nothing but "did not show the workspace" -- so the first two occurrences
# bought a re-run and no diagnosis. What ls printed, and what is actually in the
# file it reads, are the whole answer, and they cost two lines.
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
    bad "workspace inspect was incomplete: $(echo "$inspected" | tr '
' ' ')"
fi

if used=$("$WORK/remote-docker" remote use itest-ws 2>&1) &&
   outputs '\*itest-ws' "$WORK/remote-docker" remote ls; then
    ok "workspace use makes it the default"
else
    bad "workspace use did not set the default"
    info "use said: $(echo "$used" | tr '\n' ' ')"
    info "ls said: $(echo "$LAST_OUTPUT" | tr '\n' ' ')"
fi

# And docker's own current context, which is the half that was missing. Our
# default is read by this binary alone; everything else on the machine resolves
# `currentContext`, so a `use` that set only ours left compose, buildx and the
# rest talking to whatever was selected before.
#
# Asked of the CONTEXT STORE rather than of a docker command, because
# DOCKER_HOST is exported here and would mask which context is selected.
current=$(hostdocker context show 2>/dev/null)
if [ "$current" = "itest-ws" ]; then
    ok "workspace use selects the docker context too"
else
    bad "docker's current context is $current, want itest-ws"
fi

# A context we did NOT create must be left entirely alone.
#
# The endpoint is arranged before cobra parses anything, and it used to do so by
# setting DOCKER_HOST, which outranks --context in docker's own resolution. So
# every foreign context on the machine silently resolved to us. There is no way
# to see that from inside the process, which is why it is asserted here: the
# command must FAIL to reach a daemon that is not there, rather than succeed
# against ours.
hostdocker context create itest-foreign --docker host=tcp://127.0.0.1:1 >/dev/null 2>&1 || true
if out=$(timeout 30 env -u DOCKER_HOST "$WORK/remote-docker" --context itest-foreign ps 2>&1); then
    bad "a foreign context was redirected to our daemon"
    info "output: $(echo "$out" | head -2 | tr '
' '; ')"
else
    ok "a docker context we did not create is left alone"
fi
hostdocker context rm -f itest-foreign >/dev/null 2>&1 || true

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

# rm's own account of what it did to the context, printed either way. The
# assertion below can fail three different ways -- the context was never
# recognised as ours, the docker command refused, or it was removed and
# something put it back -- and they are indistinguishable from the outside.
info "workspace rm said: $(echo "$out" | tr '
' '; ')"

if outputs '^itest-ws$' hostdocker context ls --format '{{.Name}}'; then
    bad "the docker context outlived the workspace"
    info "context metadata: $(hostdocker context inspect itest-ws --format '{{.Metadata.Description}}' 2>&1 | tr '
' ' ')"
else
    ok "removing the workspace removed its docker context"
fi

# `remote` has to be FINDABLE. It is the only way in to everything this program
# does that docker does not, and the root's help is sixty commands long, so a
# command that is present but unlisted is a command nobody will type.
if outputs '^  remote ' "$WORK/remote-docker" --help; then
    ok "remote is listed in the help"
else
    bad "remote is missing from the help, so nothing points at it"
fi

restore_ws
trap cleanup EXIT

echo
echo "== 18. the client under the name docker =="
# The claim is that a machine with no Docker installed can type `docker run`.
# It rests on one thing -- the binary looking at the name it was invoked by --
# and the only way to test that is to invoke it by that name.
#
# Deliberately with NO DOCKER_HOST and no session running: that is the state a
# person is in after renaming the binary, and everything it has to do for
# itself (resolve the workspace, start a session, point the CLI at it) happens
# in this one command or not at all.
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

# A docker command that reaches no daemon must not open a session. This is not
# tidiness: once `docker` on PATH is this binary, `workspace create` writing a
# context spawns US, and a session to write a line of JSON means an SSH
# connection, an NFS server and a reverse tunnel -- torn down again immediately.
#
# Asserted through `stop`, which says "not running" when there is nothing to
# stop and "stopped" when there was.
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

# A COPY of the binary named `docker`, which is the documented installation now
# that there is no shim: the root is the Docker CLI, so the file's name is the
# whole of it. A copy rather than the symlink above, because they are different
# claims -- a symlink could be resolved back to the original somewhere, and this
# one cannot be.
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
# The claim is that a workspace can be reached through an ordinary HTTP reverse
# proxy on 443, with no SSH port involved. Only a real proxy tests that: the
# upgrade, the TLS the agent deliberately does not do, and a long-lived
# connection through something that normally serves web pages.
#
# nginx on the host network, so it reaches the published WebSocket port and the
# client reaches it back, with a certificate generated here and given to the
# client. The agent serves plain ws and knows nothing about any of this.
if command -v openssl >/dev/null 2>&1; then
    mkdir -p "$WORK/proxy"
    openssl req -x509 -newkey rsa:2048 -nodes -days 1         -keyout "$WORK/proxy/key.pem" -out "$WORK/proxy/cert.pem"         -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"         >/dev/null 2>&1
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
    if hostdocker run -d --name itest-proxy --network host         -v "$WORK/proxy:/etc/proxy:ro"         -v "$WORK/proxy/nginx.conf:/etc/nginx/nginx.conf:ro"         nginx:alpine >/dev/null 2>&1; then
        ok "a reverse proxy is in front of the workspace"
    else
        bad "could not start the reverse proxy"
    fi

    for _ in $(seq 1 30); do
        curl -sk https://127.0.0.1:8443/ >/dev/null 2>&1 && break
        sleep 1
    done

    # A session of its own: the reverse-tunnel port belongs to one session at a
    # time (ADR 0029), so this cannot run beside the one above.
    wsenv() {
        env REMOTE_DOCKER_HOST="wss://localhost:8443/tunnel"             REMOTE_DOCKER_PORT=             REMOTE_DOCKER_CA_FILE="$WORK/proxy/cert.pem"             REMOTE_DOCKER_ENDPOINT="$WORK/ws.sock"             "$@"
    }

    if outputs "tunnel port" wsenv timeout 90 "$WORK/remote-docker" remote status; then
        ok "the workspace answers over wss, through the proxy"
    else
        bad "no answer over wss: $(echo "$LAST_OUTPUT" | tail -2 | tr '
' ' ')"
    fi

    # The reverse forward is the half a proxy is most likely to break: it is the
    # direction the workspace opens, carrying NFS back to this machine.
    wsenv "$WORK/remote-docker" remote start --foreground >"$WORK/ws-up.log" 2>&1 &
    WS_PID=$!
    if wait_endpoint "$WORK/ws.sock" "$WS_PID"; then
        ok "the endpoint came up over wss"
        if out=$(timeout 120 docker -H "unix://$WORK/ws.sock" run --rm             -v "$PROJECT:/w" alpine:3 cat /w/marker 2>&1); then
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

    # A proxy that stops passing traffic without closing anything is the case
    # TCP keepalives cannot see, and the reason wslisten pings. Pausing the
    # container black-holes it exactly that way.
    #
    # A container holding a mount first, because the connection has to still be
    # OPEN when the proxy stops carrying it. Without one the client releases it
    # as idle after REMOTE_DOCKER_IDLE_TIMEOUT, and a connection closed cleanly
    # is one the agent has nothing to notice about: the assertion then fails
    # for the opposite of the reason it is testing. A mounted container keeps
    # the connection leased, measured in test/nfs-resilience.sh section 4.
    timeout 60 docker -H "unix://$WORK/ws.sock" run -d --name itest-ws-hold         -v "$PROJECT:/w" alpine:3 sh -c "$PIN_SH" >/dev/null 2>&1

    hostdocker pause itest-proxy >/dev/null 2>&1
    info "the proxy is paused; waiting for the agent to notice"
    sleep 75
    if hostdocker logs "$CONTAINER" 2>&1 | grep -q "stopped answering"; then
        ok "the agent dropped the silent websocket, so its port is free again"
    else
        bad "the agent did not notice a websocket that stopped answering"
        info "without this a vanished client keeps its reverse-tunnel port"
    fi
    hostdocker unpause itest-proxy >/dev/null 2>&1
    timeout 60 docker -H "unix://$WORK/ws.sock" rm -f itest-ws-hold >/dev/null 2>&1

    kill "$WS_PID" 2>/dev/null
    wait "$WS_PID" 2>/dev/null
    hostdocker rm -f itest-proxy >/dev/null 2>&1
else
    info "no openssl on this runner; the proxy section did not run"
fi


if [ "$FAIL" -ne 0 ]; then
    echo
    echo "== client log =="
    sed 's/^/        /' "$WORK/up.log"
    echo
    dump_workspace_log 40
fi

summary
