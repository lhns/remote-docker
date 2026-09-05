#!/usr/bin/env bash
# What a mount does when the thing behind it goes away.
#
# A bind mount becomes an NFS volume the workspace daemon mounts for itself,
# and there are TWO layers under it that can fail independently:
#
#   the kernel NFS client  ->  127.0.0.1:<port> in the workspace
#                          ->  a listener the SSH session created
#                          ->  SSH channels to this machine
#                          ->  the client's in-process NFS server
#
# Neither layer is ours to configure once a container is running. Docker's
# local driver calls mount(2) with the options we chose and never speaks to the
# volume again; everything after that is the kernel's behaviour and our mount
# options. So what a container SEES when a session drops, and whether it comes
# back, is not something to reason about. It is something to measure.
#
# This suite is the measurement, and each section states what it expects before
# it looks, so a wrong expectation is a finding rather than a red line.
#
# WHAT IS KNOWN, all of it measured here rather than reasoned about:
#
#   The share ROOT handle is the one that must survive a client restart. MOUNT
#   issues it once and the kernel never mounts again, so a root that stops
#   resolving leaves every lookup starting from something dead. Below the root,
#   Linux re-looks-up after ESTALE and needs nothing stable (ADR 0033).
#
#   Docker's local driver REFCOUNTS a mount. A volume already mounted is handed
#   to the next container as it is, stale included, so nothing recovers while
#   any container still holds it. That is why `compose down && up` cures a
#   broken mount where restarting the session does not: down drops the count to
#   zero and unmounts.
#
#   The mount address exists only while a session is connected. Anything that
#   starts a container without this client -- a restart policy, the daemon
#   coming back -- gets "connection refused" against the port (section 8).
#
#   An idle release does not fire under a live mount: the container's own
#   traffic keeps the connection leased (section 4).
#
#   A blocked port costs a mount about 180 SECONDS, not the ~60 that
#   timeo=30,retrans=2 suggests. Those govern RPCs after a mount; the mount
#   call retries on its own clock (section 7).
#
#   A container holding a file OPEN across a client restart still gets ESTALE
#   on that descriptor. There is no path lookup left to retry, and that is
#   correct rather than fixable.
#
# TWO RULES FOR EDITING THIS FILE, both of which cost a day when broken:
#
#   Never `cmd | grep -q`; use `outputs` (why: test/lib.sh).
#
#   Never observe a mount with `docker exec` or any other docker command. Every
#   one reaches the daemon through the client, which reopens the connection and
#   rebinds the listener, so the observation repairs what it came to observe.
#   The watching containers log to their own stdout, read afterwards.
#
# Requires: docker, and a kernel with NFS client support. Runs the shared
# daemon (ADR 0012); the per-account mode binds its listener inside the
# daemon's netns and deserves its own run once this one says something.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-nfsres
SSH_PORT=22224
ACCOUNT=nfsres

# The docker timeout is lib.sh's default, 120s.

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

# WATCH_SH reads its mount once a second and says what happened, with the time,
# to its own stdout. Read afterwards with `docker logs`.
#
# Never `docker exec` to observe this: every docker command reaches the daemon
# THROUGH the client, which reopens the connection and rebinds the listener. An
# observation would repair what it was there to observe.
WATCH_SH='while true; do
    if out=$(cat /w/marker 2>&1); then
        echo "$(date +%s) OK $out"
    else
        echo "$(date +%s) ERR $out"
    fi
    sleep 1
done'

cleanup() {
    echo
    echo "== cleanup =="
    if [ -n "${CLIENT_PID:-}" ]; then
        kill "$CLIENT_PID" 2>/dev/null
        wait "$CLIENT_PID" 2>/dev/null
    fi
    hostdocker rm -f "$CONTAINER" >/dev/null 2>&1
    # The agent runs as root and its host keys are owned by root, so a plain
    # rm leaves "Permission denied" as the last thing in the log of an
    # otherwise clean run.
    rm -rf "$WORK" 2>/dev/null || sudo rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

echo "== 1. build =="
if build_image; then ok "workspace image built"; else bad "image build failed"; exit 1; fi
if build_client; then ok "client built"; else bad "client build failed"; exit 1; fi

export REMOTE_DOCKER_STATE_DIR="$WORK/state"
export REMOTE_DOCKER_HOST=127.0.0.1
export REMOTE_DOCKER_PORT=$SSH_PORT
export REMOTE_DOCKER_USER=$ACCOUNT
export REMOTE_DOCKER_ENDPOINT="$WORK/docker.sock"
# Short, because two sections here are ABOUT the idle release and the default
# minute would be spent waiting rather than testing.
export REMOTE_DOCKER_IDLE_TIMEOUT=8s

mkdir -p "$WORK/keys" "$WORK/wsstate"
if enrol "$ACCOUNT" "$REMOTE_DOCKER_STATE_DIR"; then
    ok "enrolled"
else
    bad "enroll produced no public key"; exit 1
fi

if start_workspace false; then
    ok "workspace started"
else
    bad "workspace failed to start"; exit 1
fi
if wait_provisioned "$ACCOUNT"; then ok "account provisioned"; else bad "never provisioned"; exit 1; fi
if wait_parent_dockerd; then ok "the workspace daemon is up"; fi

PROJECT="$WORK/project"
mkdir -p "$PROJECT"
echo "the file the container reads" >"$PROJECT/marker"
cd "$PROJECT" || exit 1

echo
echo "== 2. a session, and the port it binds =="
CLIENT_LOG="$WORK/up.log"
"$WORK/remote-docker" remote start --foreground >"$CLIENT_LOG" 2>&1 &
CLIENT_PID=$!
if wait_endpoint "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID"; then
    ok "the endpoint answers"
else
    bad "the endpoint never came up"
    sed 's/^/        /' "$CLIENT_LOG"
    exit 1
fi
export DOCKER_HOST="unix://$REMOTE_DOCKER_ENDPOINT"

# The port is per machine and allocated, so it is READ rather than assumed. A
# hardcoded 30000 would pass here by luck and mislead on the day it moved.
if outputs "tunnel port [0-9]+" "$WORK/remote-docker" remote status; then
    PORT=$(echo "$LAST_OUTPUT" | sed -n 's/.*tunnel port \([0-9]*\).*/\1/p' | head -1)
    ok "the session bound a reverse-tunnel port: $PORT"
else
    bad "status did not report a tunnel port"
    exit 1
fi

# Whether the port is open is asked INSIDE the workspace, which is where both
# ends of it live in shared-daemon mode.
#
# Read from /proc/net/tcp rather than netstat or ss, neither of which the image
# promises. State 0A is LISTEN; the port is the hex after the colon.
HEXPORT=$(printf '%04X' "$PORT")
listening() {
    hostdocker exec "$CONTAINER" sh -c \
        "awk '\$4 == \"0A\" && \$2 ~ /:$HEXPORT\$/ {found=1} END {print (found ? \"yes\" : \"no\")}' /proc/net/tcp" \
        2>/dev/null
}

if [ "$(listening)" = yes ]; then
    ok "the port is open while the session is connected"
else
    bad "the port is not open even with a session connected; nothing below means anything"
    exit 1
fi

dockert pull -q alpine:3 >/dev/null 2>&1

echo
echo "== 3. a container holding a mount =="
if dockert run -d --name nfsres-watch -v "$PROJECT:/w" alpine:3 sh -c "$WATCH_SH" >/dev/null 2>&1; then
    ok "the watching container started"
else
    bad "could not start the watching container"; exit 1
fi
sleep 3
if outputs "OK the file the container reads" dockert logs nfsres-watch; then
    ok "it can read its mount"
else
    bad "it could not read its mount at all"
    dockert logs nfsres-watch 2>&1 | tail -5 | sed 's/^/        /'
    exit 1
fi

echo
echo "== 4. E1/E2: what an idle release does to a running container =="
# Nothing is asked of docker during this window, deliberately. The gate
# releases the connection after REMOTE_DOCKER_IDLE_TIMEOUT of no leases, and
# any docker command here would take a lease and prevent the thing under test.
info "waiting out the idle timeout with no docker commands"
mark=$(date +%s)
sleep 25

# Whether a release HAPPENED is asked before what it did, because the first run
# of this section proved nothing: the port was still open and every read
# succeeded, which reads like good news and is equally consistent with the
# release never having occurred. A mounted container talks to its server about
# once a second, and that traffic may be exactly what keeps the connection
# leased.
released=no
grep -q "released the idle connection" "$CLIENT_LOG" 2>/dev/null && released=yes
info "did the client release its connection during the window: $released"

if [ "$released" = no ]; then
    ok "a mounted container keeps the connection alive; no release to observe"
    info "which is a finding in itself: an idle timeout does not fire under a live mount"
elif [ "$(listening)" = no ]; then
    ok "E1 holds: an idle release closed the port a running mount points at"
else
    info "E1 does not hold: the port is still open after an idle release"
    ok "E1 was wrong, which is the better outcome"
fi

# What the container saw during the window, from ITS log, which needed no
# docker command at the time.
window=$(dockert logs nfsres-watch 2>&1 | awk -v t="$mark" '$1 >= t')
errs=$(echo "$window" | grep -c "ERR")
info "during the idle window the container logged $(echo "$window" | grep -c .) lines, $errs of them errors"
if [ "$errs" -gt 0 ]; then
    ok "E2 holds: reads across an idle release fail"
    echo "$window" | grep "ERR" | head -3 | sed 's/^/        /'
elif [ "$released" = yes ]; then
    ok "E2 does not hold: the mount survived a real idle release untouched"
else
    ok "E2 not exercised: no release happened, so there was nothing to survive"
fi

echo
echo "== 5. the port after a docker command =="
# This section used to claim a docker command "healed" the mount. It could
# not: section 4 established that no release happens under a live mount, so
# there was never a drop to recover from and the assertion passed on a
# connection that had never gone anywhere. What is left is the honest half.
dockert ps >/dev/null 2>&1
sleep 5
if [ "$(listening)" = yes ]; then
    ok "the port is open after a docker command"
else
    bad "the port is not open after a docker command"
fi

after=$(dockert logs nfsres-watch 2>&1 | tail -5)
if echo "$after" | grep -q "OK the file"; then
    ok "and the container is still reading, having never been interrupted"
else
    bad "the container stopped reading without anything having dropped"
    echo "$after" | sed 's/^/        /'
fi

echo
echo "== 6. E3: a NEW client process, and the mount it inherits =="
# The kernel keeps the handles it was given, including the SHARE ROOT handle
# that MOUNT returned, and it never mounts again. So a restarted client that
# cannot resolve that one leaves every lookup starting from something dead.
#
# ADR 0033 derives the root handle from the export path for exactly this, and
# leaves everything below it a cache: given a root that answers, the kernel
# re-looks-up the rest after ESTALE. This section is the only place that claim
# meets a real kernel.
kill "$CLIENT_PID" 2>/dev/null
wait "$CLIENT_PID" 2>/dev/null
sleep 2
CLIENT_LOG="$WORK/up2.log"
"$WORK/remote-docker" remote start --foreground >"$CLIENT_LOG" 2>&1 &
CLIENT_PID=$!
if wait_endpoint "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID"; then
    ok "a second client process is serving"
else
    bad "the second client never came up"
    sed 's/^/        /' "$CLIENT_LOG"
    exit 1
fi

mark=$(date +%s)
dockert ps >/dev/null 2>&1
sleep 15
window=$(dockert logs nfsres-watch 2>&1 | awk -v t="$mark" '$1 >= t')
if echo "$window" | grep -q "OK the file"; then
    ok "a running container keeps reading across a client restart"
else
    bad "a client restart still strands the running container"
    info "if this is a stale handle, the derived ROOT handle is not enough on its"
    info "own and per-file stability is back on the table (ADR 0033 says so)"
    echo "$window" | tail -3 | sed 's/^/        /'
fi
info "what the container reports now: $(echo "$window" | tail -1)"

echo
echo "== 6b. the stale mount is SHARED, and that is why down/up cures things =="
# Docker's local driver refcounts: a volume already mounted is reused rather
# than mounted again. So the stale mount the watcher holds is handed to every
# later container using the same directory, and no fresh container can recover
# while it lives.
#
# Which is why everything below this point gets a directory of its own, and why
# the watcher is removed here: a section sharing a mount with an earlier one
# measures that mount's state rather than its own subject.
if out=$(timeout 60 docker run --rm -v "$PROJECT:/w" alpine:3 cat /w/marker 2>&1); then
    ok "a NEW container reading the same stale volume works"
else
    ok "a new container inherits the stale mount: $(echo "$out" | grep -m1 -iE 'error|stale|refused' | cut -c1-160)"
    info "the refcount is the reason, and dropping it to zero is what down/up does"
fi

dockert rm -f nfsres-watch >/dev/null 2>&1
sleep 5
if out=$(timeout 60 docker run --rm -v "$PROJECT:/w" alpine:3 cat /w/marker 2>&1); then
    ok "and once the last container is gone, the volume mounts fresh and works"
else
    bad "even with nothing holding it, the volume did not recover: $(echo "$out" | tail -1 | cut -c1-160)"
fi

# Everything below wants a mount of its own, for the reason just measured.
BLACK="$WORK/black"; mkdir -p "$BLACK"; echo "black hole marker" >"$BLACK/marker"
EXIST="$WORK/existing"; mkdir -p "$EXIST"; echo "existing marker" >"$EXIST/marker"

echo
echo "== 7. E4: a black hole rather than a refusal =="
# DROP, not REJECT: a refused connection answers immediately and a dropped one
# does not answer at all, and those are different failures with different
# costs. This is the one that costs timeo*retrans.
if hostdocker exec "$CONTAINER" iptables -A INPUT -p tcp --dport "$PORT" -j DROP 2>/dev/null; then
    ok "blocked the port inside the workspace"

    dockert rm -f nfsres-black >/dev/null 2>&1
    start=$(date +%s)
    out=$(timeout 300 docker run --rm --name nfsres-black -v "$BLACK:/w" alpine:3 cat /w/marker 2>&1)
    rc=$?
    elapsed=$(( $(date +%s) - start ))
    # The daemon's line, not the tail: the tail is docker's "Run --help"
    # footer, so a bare `tail -1` records advice instead of the failure.
    said=$(echo "$out" | grep -m1 -iE "error|refused|timed out" | cut -c1-200)
    info "a mount into a black hole took ${elapsed}s and said: ${said:-$(echo "$out" | head -1)}"
    # rc 124 is OUR timeout, not the mount's verdict, and the two must never be
    # reported as the same thing: the first run said "fails rather than hanging"
    # about a number that was within seconds of the limit it was given.
    if [ "$rc" -eq 124 ]; then
        bad "the mount was still hanging when the suite gave up at ${elapsed}s"
    elif [ "$rc" -ne 0 ]; then
        ok "E4 holds in part: the mount itself gave up after ${elapsed}s (rc=$rc)"
    else
        bad "a mount through a blocked port SUCCEEDED, which nothing explains"
    fi

    hostdocker exec "$CONTAINER" iptables -D INPUT -p tcp --dport "$PORT" -j DROP 2>/dev/null
    sleep 3
    if out=$(timeout 120 docker run --rm -v "$BLACK:/w" alpine:3 cat /w/marker 2>&1); then
        ok "E4 holds: mounting works again once the block is lifted"
    else
        bad "mounting did not recover after the block was lifted: $(echo "$out" | grep -m1 -iE 'error|refused' | cut -c1-160)"
    fi
else
    info "iptables is not available in the workspace image; E4 not measured"
fi

echo
echo "== 8. E5: starting a container with no session at all =="
# The reported failure: `docker compose up` on a container that already exists.
# Creating one goes through /containers/create, which reopens the connection;
# starting one that exists does not create anything.
dockert rm -f nfsres-existing >/dev/null 2>&1
if dockert create --name nfsres-existing -v "$EXIST:/w" alpine:3 cat /w/marker >/dev/null 2>&1; then
    ok "an existing container to start later"
else
    bad "could not create the container"
fi

kill "$CLIENT_PID" 2>/dev/null
wait "$CLIENT_PID" 2>/dev/null
CLIENT_PID=""
sleep 2

# Asked of the workspace's own daemon, so no client is involved and nothing
# reopens anything. This is the daemon doing exactly what it did for the user.
out=$(hostdocker exec "$CONTAINER" docker start -a nfsres-existing 2>&1)
rc=$?
info "starting it with no session: rc=$rc, said: $(echo "$out" | grep -m1 -iE 'error|refused' | cut -c1-200)"
if echo "$out" | grep -q "connection refused"; then
    ok "E5 holds: the reported failure, reproduced -- connection refused on $PORT"
elif [ "$rc" -eq 0 ]; then
    ok "E5 does not hold: it started with no session, so the mount outlived it"
else
    ok "E5 partly: it failed, but not with connection refused"
fi

echo
echo "== 9. E6: and through a client that has to reconnect first =="
CLIENT_LOG="$WORK/up3.log"
"$WORK/remote-docker" remote start --foreground >"$CLIENT_LOG" 2>&1 &
CLIENT_PID=$!
if wait_endpoint "$REMOTE_DOCKER_ENDPOINT" "$CLIENT_PID"; then
    if out=$(timeout 120 docker start -a nfsres-existing 2>&1); then
        ok "E6 holds: starting it through the client works, so the listener is rebound first"
    else
        bad "E6 does not hold: $(echo "$out" | tail -1 | cut -c1-200)"
    fi
else
    bad "the client did not come back for section 9"
fi

echo
echo "== 10. E7: the SSH layer black-holed, with the SAME client process =="
# The other half of what this suite is for. Section 6 restarted the client,
# which changes two things at once: the connection AND the process holding the
# handle cache. This changes only the connection.
#
# If the mount recovers here but not in section 6, then handles are what
# matters and an address that survives is not enough on its own -- which is the
# whole question behind moving the listener to the agent.
SSHBH="$WORK/sshblack"; mkdir -p "$SSHBH"; echo "ssh black hole marker" >"$SSHBH/marker"
dockert rm -f nfsres-ssh >/dev/null 2>&1
if dockert run -d --name nfsres-ssh -v "$SSHBH:/w" alpine:3 sh -c "$WATCH_SH" >/dev/null 2>&1; then
    sleep 3
    if outputs "OK ssh black hole marker" dockert logs nfsres-ssh; then
        ok "a second watcher is reading its mount"
    else
        bad "the second watcher could not read its mount"
    fi

    # The sshd port, not the tunnel port: this breaks the transport UNDER the
    # NFS traffic rather than the NFS traffic itself.
    if hostdocker exec "$CONTAINER" iptables -A INPUT -p tcp --dport 2222 -j DROP 2>/dev/null; then
        mark=$(date +%s)
        # Keepalive is 15s with a 30s wait, so detection is inside 45s.
        info "black-holing the ssh port for 70s"
        sleep 70

        if grep -qiE "keepalive|connection.*(lost|closed)|reconnect" "$CLIENT_LOG" 2>/dev/null; then
            ok "the client noticed the dead transport and said so"
        else
            info "the client's log says nothing about the transport being gone"
        fi

        window=$(dockert logs nfsres-ssh 2>&1 | awk -v t="$mark" '$1 >= t')
        errs=$(echo "$window" | grep -c "ERR")
        info "during the black hole the container logged $(echo "$window" | grep -c .) lines, $errs errors"
        [ "$errs" -gt 0 ] && info "first: $(echo "$window" | grep -m1 ERR)"

        hostdocker exec "$CONTAINER" iptables -D INPUT -p tcp --dport 2222 -j DROP 2>/dev/null
        mark=$(date +%s)

        # A reconnect has to be PROVEN before anything is concluded from the
        # mount: "the handles died" and "it never reconnected" produce the
        # identical symptom, so assuming the reconnect blames the handle cache
        # either way. The client logs a line per connect, and that is the
        # evidence.
        # How LONG, not whether. The first version waited 20s, found nothing
        # working and called it "did not recover on its own" -- but a
        # black-holed socket stays writable until the kernel stops
        # retransmitting, which is minutes, and tearing the old connection down
        # waits on the goroutines riding it. "Not yet" and "never" needed
        # telling apart, and only a clock does that.
        recovered=0
        for _ in $(seq 1 32); do
            if timeout 15 docker ps >/dev/null 2>&1; then
                recovered=$(( $(date +%s) - mark ))
                break
            fi
        done
        if [ "$recovered" -gt 0 ]; then
            ok "a docker command works again ${recovered}s after the block was lifted"
        else
            bad "no docker command worked within 8 minutes of the block being lifted"
        fi

        connects=$(grep -c "connected to" "$CLIENT_LOG" 2>/dev/null || echo 0)
        if [ "$connects" -ge 2 ]; then
            info "the client opened a new connection ($connects in this process)"
        else
            info "the client's original connection resumed; it never had to redial"
        fi

        # Only now look at the mount. The command working says the transport is
        # back; the container reads on its own clock and had none of that time
        # to try again.
        mark=$(date +%s)
        sleep 20
        window=$(dockert logs nfsres-ssh 2>&1 | awk -v t="$mark" '$1 >= t')
        if [ "$recovered" -eq 0 ]; then
            bad "E7 not measured: the transport never came back, so the mount proves nothing"
        elif echo "$window" | grep -q "OK ssh black hole marker"; then
            ok "E7: the mount survives an interrupted transport and reads again"
        else
            bad "E7: the transport came back and the mount did not"
            info "the connection is not what strands a container, so look at the handles"
        fi
        info "last line from the watcher: $(echo "$window" | tail -1)"
    else
        info "iptables unavailable; E7 not measured"
    fi
    dockert rm -f nfsres-ssh >/dev/null 2>&1
else
    bad "could not start the second watcher"
fi

echo
echo "== client log =="
tail -25 "$WORK/up.log" 2>/dev/null | sed 's/^/        /'
echo "== the client under test =="
tail -25 "$CLIENT_LOG" 2>/dev/null | sed 's/^/        /'

summary
