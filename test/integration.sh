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

# Which server the workspace runs. The agent must pass the suite written
# against sshd, unchanged -- that is what makes it a substitution rather than a
# rewrite with its own bug surface (ADR 0010).
#
#   WORKSPACE_SERVER=sshd    the original shell implementation
#   WORKSPACE_SERVER=agent   remote-dockerd serve
SERVER=${WORKSPACE_SERVER:-agent}
WORK=$(mktemp -d)
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-itest
SSH_PORT=22222
ACCOUNT=itest

# Every docker command that crosses the proxy is wrapped in a timeout. A
# container whose volume mount never completes would otherwise block forever,
# burning the whole CI budget and reporting nothing about where it stopped.
DOCKER_TIMEOUT=120
dockert() { timeout "$DOCKER_TIMEOUT" docker "$@"; }

# The workspace container itself lives on the RUNNER's daemon. Once DOCKER_HOST
# is exported, plain `docker` talks to the WORKSPACE's daemon instead, so
# anything about the container -- exec, logs, inspect -- has to say which
# daemon it means or it silently looks in the wrong place.
hostdocker() { env -u DOCKER_HOST docker "$@"; }

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); echo "  PASS  $*"; }
bad()  { FAIL=$((FAIL + 1)); echo "  FAIL  $*"; }
info() { echo "  ....  $*"; }

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
# Context is the repo root: the image builds the agent from source.
if docker build -q -t "$IMAGE" -f "$REPO/image/Dockerfile" "$REPO" >/dev/null; then
    ok "image builds"
else
    bad "image build failed"
    exit 1
fi

echo
echo "== 2. build the client =="
if (cd "$REPO" && CGO_ENABLED=0 go build -o "$WORK/remote-docker" ./cmd/remote-docker); then
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

echo
echo "== 3. enrol this machine =="
mkdir -p "$WORK/keys" "$WORK/wsstate"
"$WORK/remote-docker" enroll >/dev/null 2>&1
if [ -f "$REMOTE_DOCKER_STATE_DIR/id_ed25519.pub" ]; then
    # The filename becomes the unix account on the workspace.
    cp "$REMOTE_DOCKER_STATE_DIR/id_ed25519.pub" "$WORK/keys/$ACCOUNT.pub"
    ok "keypair generated and staged as $ACCOUNT.pub"
else
    bad "enroll produced no public key"
    exit 1
fi

echo
echo "== 4. start the workspace =="
hostdocker rm -f "$CONTAINER" >/dev/null 2>&1

# The agent is a command on the same image; sshd is the image's default
# entrypoint. Nothing else about the deployment differs, which is the point.
SERVER_ARGS=()
if [ "$SERVER" = "agent" ]; then
    SERVER_ARGS=(remote-dockerd serve)
fi
echo "  ....  server: $SERVER"

if hostdocker run -d --name "$CONTAINER" --privileged \
        -p "$SSH_PORT:2222" \
        -v "$WORK/keys:/etc/workspace/authorized_keys.d:ro" \
        -v "$WORK/wsstate:/etc/workspace" \
        -e DOCKER_TLS_CERTDIR= \
        "$IMAGE" "${SERVER_ARGS[@]}" >/dev/null; then
    ok "workspace container started"
else
    bad "workspace container failed to start"
    exit 1
fi

info "waiting for the account to be provisioned"
provisioned=false
for _ in $(seq 1 60); do
    if hostdocker exec "$CONTAINER" id "$ACCOUNT" >/dev/null 2>&1; then
        provisioned=true
        break
    fi
    sleep 1
done
if [ "$provisioned" = true ]; then
    ok "key-watcher provisioned the account"
else
    bad "the account was never provisioned"
    hostdocker logs "$CONTAINER" 2>&1 | tail -30
    exit 1
fi

info "waiting for dockerd inside the workspace"
for _ in $(seq 1 60); do
    hostdocker exec "$CONTAINER" docker info >/dev/null 2>&1 && break
    sleep 1
done

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
if timeout 90 "$WORK/remote-docker" status >"$WORK/status.log" 2>&1 && grep -q "nfs port" "$WORK/status.log"; then
    ok "status reports the workspace parameters"
    sed 's/^/        /' "$WORK/status.log"
else
    bad "status failed"
    sed 's/^/        /' "$WORK/status.log"
    hostdocker logs "$CONTAINER" 2>&1 | tail -20
    exit 1
fi

echo
echo "== 6. open a session =="
"$WORK/remote-docker" up >"$WORK/up.log" 2>&1 &
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
if out=$(dockert run --rm alpine:3 echo hello-from-container 2>&1); then
    if [ "$out" = "hello-from-container" ]; then
        ok "container stdout reaches the client"
    else
        bad "stdout was lost or altered: got [$out]"
    fi
else
    bad "container failed: $out"
fi

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
if out=$(dockert run --rm -v "$PROJECT:/w" alpine:3 cat /w/marker 2>&1); then
    if [ "$out" = "from the project directory" ]; then
        ok "the container read this machine's file through the tunnel"
    else
        bad "unexpected content: $out"
    fi
else
    bad "container failed: $out"
fi

echo
echo "== 8. a bind mount OUTSIDE the working directory =="
# The case the previous single-mount design could not express at all.
if out=$(dockert run --rm -v "$OUTSIDE:/d" alpine:3 cat /d/data 2>&1); then
    if [ "$out" = "from an unrelated directory" ]; then
        ok "an unrelated local directory resolved"
    else
        bad "unexpected content: $out"
    fi
else
    bad "container failed: $out"
fi

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
docker rm -f itest-web >/dev/null 2>&1

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
if (cd "$REPO" && CGO_ENABLED=0 GOOS=linux go build -o "$PROJECT/watchprobe" ./test/watchprobe); then
    # The error is NOT swallowed. The first run of this test produced an
    # empty log and no explanation, which cost a CI round trip to diagnose:
    # the binary was on the share with a synthesised mode 0644 and could not
    # be executed.
    if ! dockert run -d --name itest-watch             -v "$PROJECT:/probe:ro"             -v "$WATCHDIR:/data"             alpine:3 /probe/watchprobe /data >"$WORK/watch-run.log" 2>&1; then
        bad "the watch probe container would not start"
        sed 's/^/        /' "$WORK/watch-run.log"
    fi

    # Let the watch register before making the change, or the result says
    # nothing either way.
    sleep 5
    echo "written on the client" >"$WATCHDIR/created-after-watch.txt"

    timeout 60 docker wait itest-watch >/dev/null 2>&1
    probe=$(docker logs itest-watch 2>&1)
    docker rm -f itest-watch >/dev/null 2>&1
    rm -f "$PROJECT/watchprobe"

    echo "        $(echo "$probe" | grep '^RESULT' || echo 'RESULT missing')"

    if echo "$probe" | grep -q "POLL created-after-watch.txt"; then
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
echo "== 11c. a container survives an idle disconnect =="
# The connection is released when nothing needs it (ADR 0015), which makes the
# reconnect path load-bearing rather than an error case. It must also NOT be
# released while a container holds one of our volumes -- that container has a
# live NFS mount, and dropping the tunnel underneath gives it EIO.
if dockert run -d --name itest-idle -v "$PROJECT:/w" alpine:3         sh -c 'while true; do cat /w/marker >/dev/null || exit 1; sleep 1; done' >/dev/null 2>&1; then

    # Longer than the idle timeout, so a sweep has certainly run.
    sleep 75

    if [ "$(docker inspect -f '{{.State.Running}}' itest-idle 2>/dev/null)" = "true" ]; then
        ok "a container holding one of our volumes kept working across an idle period"
    else
        bad "the container died during the idle period -- its mount was dropped"
        docker logs itest-idle 2>&1 | tail -5 | sed 's/^/        /'
    fi

    # And the client is still usable afterwards, reconnecting if it released.
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
    if hostdocker exec "$CONTAINER" id "$OTHER" >/dev/null 2>&1; then
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
    first_port=$(awk '/^nfs port/ {print $3}' "$WORK/status.log")
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
echo "== 13. our volumes are labelled and identifiable =="
if docker volume ls --format '{{.Name}}' 2>/dev/null | grep -q '^rd-'; then
    ok "shares became rd-* volumes on the workspace daemon"
else
    bad "no managed volumes were created"
fi

echo
if [ "$FAIL" -ne 0 ]; then
    echo
    echo "== client log =="
    sed 's/^/        /' "$WORK/up.log"
    echo
    echo "== workspace log =="
    hostdocker logs "$CONTAINER" 2>&1 | tail -40 | sed 's/^/        /'
fi

echo
echo "=================================="
echo "  passed: $PASS   failed: $FAIL"
echo "=================================="
[ "$FAIL" -eq 0 ]
