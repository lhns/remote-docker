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
# This suite is the measurement. Every section states the expectation first and
# reports what actually happened, so a wrong expectation is a finding rather
# than a red line -- several of these expectations are predictions nobody has
# checked, and the header is updated when one of them is wrong.
#
# THE EXPECTATIONS, as of writing (unproven; that is the point):
#
#   E1  An idle release closes the listener, so the address a running
#       container's mount points at stops existing while nothing is wrong with
#       the container.
#         MEASURED 2026-08-13: no release happened at all. A mounted container
#         talks to its server about once a second and that traffic keeps the
#         connection leased, so an idle timeout does not fire under a live
#         mount. The section now asks whether a release occurred BEFORE asking
#         what it did, because "everything worked" and "nothing happened" look
#         identical.
#   E2  A container reading across that window fails rather than waiting: the
#       mount is soft, so the kernel gives up and reports EIO.
#         MEASURED: not exercised, for the reason above.
#   E3  A reconnect does NOT heal an established mount. Handles come from
#       go-nfs's in-memory caching handler, so a new client PROCESS mints new
#       ones and everything the kernel still holds is stale.
#         MEASURED: HELD, and then FIXED. "cat: can't open '/w/marker': Stale
#         file handle" after every client restart. The handle that mattered was
#         the SHARE ROOT, which MOUNT returns once and the kernel can never ask
#         for again; ADR 0033 derives it from the export path. This section now
#         asserts the mount survives instead of recording that it does not.
#   E4  A blocked port (a black hole, not a refusal) costs the mount
#       timeo*retrans before it reports anything, and recovers by itself when
#       the block is lifted, because NFSv3 has nothing to renegotiate.
#         MEASURED: recovery HOLDS. The cost does not: it took ~180s, not the
#         ~60s timeo*retrans suggests, because those govern RPCs after a mount
#         and the mount call has retries of its own. `docker run` sits there
#         for three minutes before saying anything.
#   E5  Starting an existing container with no session gets "connection
#       refused" against the port, which is the failure a user reported after
#       `docker compose up` on a container that already existed.
#         MEASURED: HOLDS, word for word. The address exists only while a
#         session is connected, so anything that starts a container without
#         this client fails exactly the way the report described.
#   E6  Starting one THROUGH a session that had to reconnect first does not
#       race: the listener is rebound before the request reaches the daemon.
#         MEASURED: HOLDS.
#   E7  A dropped TRANSPORT is not a dropped process. Black-holing the ssh port
#       breaks the connection while the client and its handle cache live on, so
#       the mount should recover when the transport does.
#         MEASURED: the workspace held the dead session's port reservation and
#         refused the client its reverse forward on every reconnect -- "another
#         session for this account may still be open" -- so nothing worked for
#         the eight minutes the suite would wait. That is the failure this
#         whole investigation started from, and it is now fixed on the agent
#         side; recovery took 16s.
#         E7 then HELD, once the section stopped asking the wrong question
#         (one version demanded a reconnect that never had to happen, and two
#         read the log of a client killed two sections earlier): the client
#         redialled, got its forward, and the container went back to reading
#         through the mount it already had. Before the server moved to the
#         Session that redial would have minted new handles and left it stale,
#         which is E3's failure with a different trigger.
#
# AND WHAT THIS SUITE PROVED ABOUT THE HARNESS, by falling for it:
#
#   `cmd | grep -q` fails the assertion it just matched, when the producer is
#   still writing. Section 3 reported "it could not read its mount at all"
#   while printing four lines of the match underneath. `docker logs` on a busy
#   container is the producer that shows it; `remote ls` finishes writing too
#   fast and survived 5,067 runs. So: `outputs`, never a pipeline.
#
# AND THE ONE NOBODY PREDICTED, which invalidated the first run's sections 7
# to 9 and is the most useful thing here:
#
#   Docker's local driver REFCOUNTS a mount. A volume already mounted by one
#   container is handed to the next as it is -- including when it has gone
#   stale -- so no new container can recover while any container still holds
#   it. That is why `compose down && compose up` cures a broken mount and
#   restarting the session does not: down drops the refcount to zero and
#   unmounts, and up mounts fresh.
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

DOCKER_TIMEOUT=120
dockert() { timeout "$DOCKER_TIMEOUT" docker "$@"; }

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
# than mounted again. So the stale mount the watcher is holding is handed to
# every later container using the same directory, and no fresh container can
# recover while it lives.
#
# The first run of this suite learned that by accident -- sections 7, 8 and 9
# all measured this instead of what they meant to -- which is why everything
# below gets its own directory, and why the watcher is removed here.
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
    # footer, which is what the first run of this suite recorded instead of
    # the error.
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
        # mount, because "the handles died" and "it never reconnected" produce
        # the identical symptom. The client logs a line per connect, so the
        # count is the evidence; the first version of this section assumed the
        # reconnect and would have blamed the handle cache either way.
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
