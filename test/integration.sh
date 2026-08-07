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
ACCOUNT=itest

# Every docker command that crosses the proxy is wrapped in a timeout. A
# container whose volume mount never completes would otherwise block forever,
# burning the whole CI budget and reporting nothing about where it stopped.
DOCKER_TIMEOUT=120
dockert() { timeout "$DOCKER_TIMEOUT" docker "$@"; }

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
    docker rm -f "$CONTAINER" >/dev/null 2>&1
    rm -rf "$WORK"
}
trap cleanup EXIT

echo "== 1. build the workspace image =="
if docker build -q -t "$IMAGE" "$REPO/image" >/dev/null; then
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
docker rm -f "$CONTAINER" >/dev/null 2>&1
if docker run -d --name "$CONTAINER" --privileged \
        -p "$SSH_PORT:2222" \
        -v "$WORK/keys:/etc/workspace/authorized_keys.d:ro" \
        -v "$WORK/wsstate:/etc/workspace" \
        -e DOCKER_TLS_CERTDIR= \
        "$IMAGE" >/dev/null; then
    ok "workspace container started"
else
    bad "workspace container failed to start"
    exit 1
fi

info "waiting for the account to be provisioned"
provisioned=false
for _ in $(seq 1 60); do
    if docker exec "$CONTAINER" id "$ACCOUNT" >/dev/null 2>&1; then
        provisioned=true
        break
    fi
    sleep 1
done
if [ "$provisioned" = true ]; then
    ok "key-watcher provisioned the account"
else
    bad "the account was never provisioned"
    docker logs "$CONTAINER" 2>&1 | tail -30
    exit 1
fi

info "waiting for dockerd inside the workspace"
for _ in $(seq 1 60); do
    docker exec "$CONTAINER" docker info >/dev/null 2>&1 && break
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
if "$WORK/remote-docker" status 2>&1 | tee "$WORK/status.log" | grep -q "nfs port"; then
    ok "status reports the workspace parameters"
    sed 's/^/        /' "$WORK/status.log"
else
    bad "status failed"
    sed 's/^/        /' "$WORK/status.log"
    docker logs "$CONTAINER" 2>&1 | tail -20
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
echo "== 12. our volumes are labelled and identifiable =="
if docker volume ls --format '{{.Name}}' 2>/dev/null | grep -q '^rd-'; then
    ok "shares became rd-* volumes on the workspace daemon"
else
    bad "no managed volumes were created"
fi

echo
echo "=================================="
echo "  passed: $PASS   failed: $FAIL"
echo "=================================="
[ "$FAIL" -eq 0 ]
