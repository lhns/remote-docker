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
# Idle release is a real behaviour worth testing (ADR 0015), but the default
# minute meant sleeping 75 seconds to observe it -- a quarter of this suite's
# runtime spent waiting for a timer we control.
export REMOTE_DOCKER_IDLE_TIMEOUT=8s

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

if hostdocker run -d --name "$CONTAINER" --privileged \
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
    if hostdocker exec "$CONTAINER" id "$ACCOUNT" >/dev/null 2>&1; then
        provisioned=true
        break
    fi
    sleep 1
done
if [ "$provisioned" = true ]; then
    ok "the agent provisioned the account"
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

# The agent's build, which is the question worth being able to answer when a
# workspace behaves oddly and is not the same question as the client's build.
# It is reported even when the workspace is too old to send one, because
# silence there is indistinguishable from a failure to answer.
if grep -qE "^agent +[^ ]" "$WORK/status.log"; then
    ok "status reports the agent's version"
else
    bad "status did not report the agent version"
fi

echo
echo "== 6. open a session =="
# --foreground because the suite wants the session as a child it can kill and
# whose log it can read. `start` on its own detaches, which is right for a
# person and wrong for a test that needs to end it deterministically.
"$WORK/remote-docker" start --foreground >"$WORK/up.log" 2>&1 &
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

if (cd "$REPO" && CGO_ENABLED=0 GOOS=linux go build -o "$PROJECT/watchprobe" ./test/watchprobe &&
        CGO_ENABLED=0 GOOS=linux go build -o "$PROJECT/pokeprobe" ./test/pokeprobe); then

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
            if docker logs itest-poke 2>&1 | grep -q '^READY'; then
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
sleep 20
if out=$(dockert run --rm alpine:3 echo after-idle 2>&1); then
    if [ "$out" = "after-idle" ]; then
        ok "the client reconnects after an idle release"
    else
        bad "unexpected output after an idle period: $out"
    fi
else
    bad "the client could not reconnect after an idle period: $out"
fi

# (b) a container holding one of our volumes must pin the connection: it has a
# live NFS mount, and dropping the tunnel underneath gives it EIO. The loop
# exits non-zero if the mount stops working, so the container's own survival is
# the assertion.
if dockert run -d --name itest-idle -v "$PROJECT:/w" alpine:3         sh -c 'while true; do cat /w/marker >/dev/null || exit 1; sleep 1; done' >/dev/null 2>&1; then

    sleep 20

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
echo "== 13. our volumes are labelled and identifiable =="
if docker volume ls --format '{{.Name}}' 2>/dev/null | grep -q '^rd-'; then
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
    'tty; id -un' 2>&1 </dev/null)

# tr squeezes the pty's CRLF out so a failure prints as one readable line.
trim() { echo "$1" | tr -d '
' | tail -3 | tr '
' ' '; }

if echo "$shellout" | grep -q '/dev/pts/'; then
    ok "a stock ssh gets an interactive shell on a pty"
else
    bad "no pty from the agent: $(trim "$shellout")"
fi

if echo "$shellout" | grep -q "^$ACCOUNT"; then
    ok "the shell runs as the enrolled account"
else
    bad "the shell was not $ACCOUNT: $(trim "$shellout")"
fi

# The embedded CLI: the client's own docker, not the runner's.
if out=$(timeout 60 "$WORK/remote-docker" docker ps --format '{{.Names}}' 2>&1); then
    ok "the embedded docker CLI talks to the workspace"
else
    bad "the embedded docker CLI failed: $(echo "$out" | tail -2)"
fi

# `status` while `up` holds the export port. An account has exactly ONE
# reverse-tunnel port, so a session that needlessly reserves it fails the
# moment `up` is running -- which is exactly when someone would run another
# command. This is the case that broke.
if out=$(timeout 60 "$WORK/remote-docker" status 2>&1); then
    ok "status works while up is running"
else
    bad "status failed while up was running: $(echo "$out" | tail -2)"
fi

# gc must not remove a volume that is in use, and must not fail.
if out=$(timeout 90 "$WORK/remote-docker" gc 2>&1); then
    if echo "$out" | grep -q "removed"; then
        ok "gc ran and reported what it removed"
    else
        bad "gc gave no account of itself: $out"
    fi
else
    bad "gc failed: $(echo "$out" | tail -2)"
fi

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

if (cd "$REPO" && CGO_ENABLED=0 GOOS=linux go build -o "$PROJECT/watchprobe" ./test/watchprobe); then
    REMOTE_DOCKER_WATCH=partial "$WORK/remote-docker" start --foreground >"$WORK/watch-up.log" 2>&1 &
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
            docker logs itest-replay 2>&1 | grep -q '^READY' && break
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

if "$WORK/remote-docker" start >"$WORK/start.log" 2>&1; then
    ok "start returned without holding a terminal"
    sed 's/^/        /' "$WORK/start.log"
else
    bad "start failed"
    sed 's/^/        /' "$WORK/start.log"
fi

if out=$(dockert run --rm alpine:3 echo through-the-daemon 2>&1); then
    if [ "$out" = "through-the-daemon" ]; then
        ok "docker works through the background session"
    else
        bad "unexpected output through the daemon: $out"
    fi
else
    bad "docker failed through the daemon: $out"
fi

# Idempotent: a second start must not fight the first for the endpoint.
if "$WORK/remote-docker" start 2>&1 | grep -q "already running"; then
    ok "a second start reports the running one rather than racing it"
else
    bad "a second start did not recognise the running session"
fi

# The endpoint has exactly one owner, and it must refuse rather than steal --
# which on Unix it silently used to do, unlinking the socket and leaving the
# first session accepting on an inode nobody could reach.
#
# Deliberately spelled `up` rather than `start --foreground`: `up` is a hidden
# alias kept for scripts, and an alias nothing exercises is an alias nobody
# notices breaking. This is its coverage.
if out=$("$WORK/remote-docker" up 2>&1); then
    bad "a second up took the endpoint from the running session"
else
    case "$out" in
        *"already serving"*) ok "a second up is refused, naming the owner" ;;
        *) bad "a second up failed for the wrong reason: $out" ;;
    esac
fi

# A detached container must outlive the command that started it. This is what
# the in-process session could not do: it died with the command and took the
# container's mount with it.
if dockert run -d --name itest-detached -v "$PROJECT:/w" alpine:3         sh -c 'while true; do cat /w/marker >/dev/null || exit 1; sleep 1; done' >/dev/null 2>&1; then
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

if "$WORK/remote-docker" stop 2>&1 | grep -q "stopped"; then
    ok "stop ends the background session"
else
    bad "stop did not end the background session"
fi
if "$WORK/remote-docker" stop 2>&1 | grep -q "not running"; then
    ok "stopping an already-stopped session says so"
else
    bad "stopping twice did not report it was not running"
fi

# A session built from a different commit is replaced when that costs nothing,
# and reported when it does not. A stale session serves the endpoint, so an
# updated client talks to the OLD build and behaves like it -- silently, until
# something it should have fixed does not work.
#
# Two binaries, same source, different stamps: the versions cannot be ordered
# and nothing here tries to.
if (cd "$REPO" && CGO_ENABLED=0 go build -ldflags="-X main.version=sha-oldbuild"         -o "$WORK/remote-docker-old" ./cmd/remote-docker); then

    "$WORK/remote-docker-old" start >/dev/null 2>&1

    # (a) nothing depends on it -> replaced silently.
    "$WORK/remote-docker" docker ps >/dev/null 2>&1
    if "$WORK/remote-docker" status 2>/dev/null | grep -q "DIFFERENT"; then
        bad "an unused session built from another commit was not replaced"
    else
        ok "an unused session from another commit is replaced silently"
    fi

    # (b) something depends on it -> warned about, left alone. The old binary
    # starts the container so the session holding it is the old one.
    "$WORK/remote-docker" stop >/dev/null 2>&1
    "$WORK/remote-docker-old" start >/dev/null 2>&1
    if "$WORK/remote-docker-old" docker run -d --name itest-pin -v "$PROJECT:/w" alpine:3             sh -c 'while true; do cat /w/marker >/dev/null || exit 1; sleep 1; done' >/dev/null 2>&1; then

        warned=$("$WORK/remote-docker" docker ps 2>&1)
        case "$warned" in
            *"different version"*) ok "a session in use from another commit is reported, not restarted" ;;
            *) bad "no version warning while a container depended on the old session" ;;
        esac
        if "$WORK/remote-docker" status 2>/dev/null | grep -q "sha-oldbuild"; then
            ok "the in-use session was left running"
        else
            bad "the in-use session was replaced, taking its container's mount with it"
        fi

        # restart must refuse rather than break it.
        if "$WORK/remote-docker" restart >/dev/null 2>&1; then
            bad "restart proceeded while a container depended on the session"
        else
            ok "restart refuses while something depends on the session"
        fi

        "$WORK/remote-docker" docker rm -f itest-pin >/dev/null 2>&1
    else
        bad "could not start the pinning container"
    fi
    "$WORK/remote-docker" stop >/dev/null 2>&1
else
    bad "could not build a second client for the version test"
fi

# It reclaims itself. A session that has never been used is the case that
# should go soonest, and the one that used to be unable to: with no last-use
# time, it reported zero idle and could never expire.
if REMOTE_DOCKER_DAEMON_IDLE=8s "$WORK/remote-docker" start >/dev/null 2>&1; then
    reclaimed=false
    for _ in $(seq 1 6); do
        sleep 5
        if ! "$WORK/remote-docker" start 2>&1 | grep -q "already running"; then
            reclaimed=true
            "$WORK/remote-docker" stop >/dev/null 2>&1
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

if out=$("$WORK/remote-docker" workspace create itest-ws --host 127.0.0.1 --port "$SSH_PORT" --user "$ACCOUNT" 2>&1); then
    ok "workspace create added a workspace"
else
    bad "workspace create failed: $(echo "$out" | tail -2)"
fi

if hostdocker context ls --format '{{.Name}}' 2>/dev/null | grep -qx "itest-ws"; then
    ok "creating a workspace created its docker context"
else
    bad "no docker context appeared for the workspace"
fi

if "$WORK/remote-docker" workspace ls 2>&1 | grep -q "itest-ws"; then
    ok "workspace ls shows it"
else
    bad "workspace ls did not show the workspace"
fi

# inspect is the one place the four derivations meet: the config file's view,
# the endpoint derived from the name, the context named after it, and whether
# anything is serving it.
inspected=$("$WORK/remote-docker" workspace inspect itest-ws 2>&1)
if echo "$inspected" | grep -q "docker context" && echo "$inspected" | grep -q "endpoint"; then
    ok "workspace inspect reports the endpoint and the docker context together"
else
    bad "workspace inspect was incomplete: $(echo "$inspected" | tr '
' ' ')"
fi

if "$WORK/remote-docker" workspace use itest-ws >/dev/null 2>&1 &&
   "$WORK/remote-docker" workspace ls 2>&1 | grep -q "\*itest-ws"; then
    ok "workspace use makes it the default"
else
    bad "workspace use did not set the default"
fi

# The old verbs are aliases, not history: something out there is scripted
# against them.
if "$WORK/remote-docker" workspace list 2>&1 | grep -q "itest-ws"; then
    ok "the old verb 'list' still works"
else
    bad "the list alias stopped working"
fi

if out=$("$WORK/remote-docker" workspace rm itest-ws 2>&1); then
    ok "workspace rm removed it"
else
    bad "workspace rm failed: $(echo "$out" | tail -2)"
fi

if hostdocker context ls --format '{{.Name}}' 2>/dev/null | grep -qx "itest-ws"; then
    bad "the docker context outlived the workspace"
else
    ok "removing the workspace removed its docker context"
fi

# `context` is gone, and gone means gone -- a command that still half-exists is
# worse than one that does not.
if "$WORK/remote-docker" --help 2>&1 | grep -qE '^  context'; then
    bad "the context command is still in the help"
else
    ok "context is no longer a command"
fi

restore_ws
trap cleanup EXIT


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
