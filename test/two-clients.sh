#!/usr/bin/env bash
# ONE account, TWO client machines, at the same time (ADR 0029).
#
# Both machines share the daemon, and so containers and images; they must NOT
# share files. Two clients are two state directories with a key each, both in
# `alice`'s one key file: different keys, different client ids.
#
# per-user-dind.sh is two ACCOUNTS; this is two machines of one.
#
# Requires: docker, and a kernel with NFS client support.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
IMAGE=remote-docker-workspace:test
CONTAINER=remote-docker-twoclients
SSH_PORT=22224

ACCOUNT=alice
PC=pc
PHONE=phone

DOCKER_TIMEOUT=180

# shellcheck source=test/lib.sh
. "$REPO/test/lib.sh"

cleanup() { cleanup_suite "${PC_PID:-}" "${PHONE_PID:-}"; }
trap cleanup EXIT

echo "== 1. build =="
if build_image && build_client; then
    ok "image and client build"
else
    bad "build failed"
    exit 1
fi

echo
echo "== 2. two machines, one account =="
mkdir -p "$WORK/keys" "$WORK/wsstate"
for machine in "$PC" "$PHONE"; do
    # genkey rather than enrol: both keys go into ONE account's file below.
    if ! genkey "$WORK/state-$machine"; then
        bad "no key generated for $machine"
        exit 1
    fi
done

cat "$WORK/state-$PC/id_ed25519.pub" "$WORK/state-$PHONE/id_ed25519.pub" \
    >"$WORK/keys/$ACCOUNT.pub"

if [ "$(grep -c . "$WORK/keys/$ACCOUNT.pub")" -eq 2 ]; then
    ok "two keys enrolled as one account"
else
    bad "the key file does not hold two keys"
    exit 1
fi

echo
echo "== 3. start the workspace =="
# The shared daemon: a daemon per account would add a second variable to every
# failure below.
if start_workspace false; then
    ok "workspace container started"
else
    bad "workspace container failed to start"
    exit 1
fi

if wait_provisioned "$ACCOUNT"; then
    ok "the account was provisioned"
else
    bad "the account was never provisioned"
    dump_workspace_log 40
    exit 1
fi
wait_parent_dockerd

echo
echo "== 4. a session on each machine, at the same time =="
mkdir -p "$WORK/project-$PC" "$WORK/project-$PHONE"
echo "from the pc" >"$WORK/project-$PC/marker"
echo "from the phone" >"$WORK/project-$PHONE/marker"

# Both as $ACCOUNT: only the state directory differs.
session() {
    local machine=$1 endpoint=$2 log=$3 dir=$4
    start_session "$WORK/state-$machine" "$ACCOUNT" "$endpoint" "$log" "$dir"
}

PC_SOCK="$WORK/pc.sock"
PHONE_SOCK="$WORK/phone.sock"

PC_PID=$(session "$PC" "$PC_SOCK" "$WORK/pc.log" "$WORK/project-$PC")
PHONE_PID=$(session "$PHONE" "$PHONE_SOCK" "$WORK/phone.log" "$WORK/project-$PHONE")

dpc() { dockerat "$PC_SOCK" "$@"; }
dphone() { dockerat "$PHONE_SOCK" "$@"; }

if wait_endpoint "$PC_SOCK" "$PC_PID" && wait_endpoint "$PHONE_SOCK" "$PHONE_PID"; then
    ok "both machines have a working docker endpoint at the same time"
else
    bad "an endpoint never came up"
    sed 's/^/    pc: /' "$WORK/pc.log" | tail -20
    sed 's/^/    phone: /' "$WORK/phone.log" | tail -20
    dump_workspace_log 40
    exit 1
fi

# With one port per account, the second machine got "tcpip-forward request
# denied by peer".
if grep -q "denied by peer" "$WORK/pc.log" "$WORK/phone.log"; then
    bad "a machine was refused its reverse tunnel"
    grep -h "denied by peer" "$WORK/pc.log" "$WORK/phone.log" | head -3
else
    ok "neither machine was refused its reverse tunnel"
fi

echo
echo "== 5. a port each, remembered =="
# Written by the agent beside uidmap: one line per machine, same account.
ports=$(hostdocker exec "$CONTAINER" cat /etc/workspace/clientports 2>/dev/null)
echo "$ports" | sed 's/^/    /'

if [ "$(echo "$ports" | grep -c "^$ACCOUNT:")" -eq 2 ]; then
    ok "the workspace recorded a port for each machine"
else
    bad "the workspace did not record two ports"
fi
if [ "$(echo "$ports" | awk -F: '{print $3}' | sort -u | grep -c .)" -eq 2 ]; then
    ok "the two machines were given different ports"
else
    bad "both machines were given the same port"
fi

# Pulled first, so "Unable to find image locally" stays out of the output
# assertions read. One daemon serves both machines.
info "pulling the test image"
dpc pull -q alpine:3 >/dev/null 2>&1 || info "could not pre-pull alpine:3"

echo
echo "== 6. each machine mounts ITS OWN files =="
# Both bind their own directory at the same container path. The last line
# only: anything before it is docker talking, not the file.
pc_saw=$(dpc run --rm -v "$WORK/project-$PC:/w" alpine:3 cat /w/marker 2>&1 | tail -1)
phone_saw=$(dphone run --rm -v "$WORK/project-$PHONE:/w" alpine:3 cat /w/marker 2>&1 | tail -1)

if [ "$pc_saw" = "from the pc" ]; then
    ok "the pc's container read the pc's file"
else
    bad "the pc's container read: $pc_saw"
fi
if [ "$phone_saw" = "from the phone" ]; then
    ok "the phone's container read the phone's file"
else
    bad "the phone's container read: $phone_saw"
fi

echo
echo "== 7. one daemon, so both see the same containers =="
dpc run -d --name shared-by-both alpine:3 sleep 300 >/dev/null 2>&1
if outputs '^shared-by-both$' dphone ps --format '{{.Names}}'; then
    ok "the phone sees a container the pc started"
else
    bad "the phone cannot see the pc's container"
fi
dpc rm -f shared-by-both >/dev/null 2>&1

echo
echo "== 7b. both machines publish, and each gets its own number =="
# The port is the client's (ADR 0008), so the workspace refuses neither. These
# two clients share a runner, so each asks for a number of its own and must
# open the one ITS OWN container asked for.
echo "<h1>pc</h1>" >"$WORK/project-$PC/index.html"
echo "<h1>phone</h1>" >"$WORK/project-$PHONE/index.html"

dpc run -d --name pc-web -p 18095:80 -v "$WORK/project-$PC:/usr/share/nginx/html" nginx:alpine >/dev/null 2>&1
dphone run -d --name phone-web -p 18096:80 -v "$WORK/project-$PHONE:/usr/share/nginx/html" nginx:alpine >/dev/null 2>&1

for probe in "18095 pc" "18096 phone"; do
    set -- $probe
    if wait_url "http://127.0.0.1:$1/" "<h1>$2</h1>" 45; then
        ok "$2 reached its own container on the port it asked for"
    else
        bad "$2 never reached 127.0.0.1:$1"
    fi
done

# And the workspace bound neither number, which is what stops the two from
# colliding there in the first place.
if ! outputs ':[0-9]+$' dpc port pc-web 80/tcp; then
    bad "pc-web published nothing: [$LAST_OUTPUT]"
elif outputs ':18095$' dpc port pc-web 80/tcp; then
    bad "the workspace bound 18095 itself"
else
    ok "the workspace published on ports of its own choosing"
fi

dpc rm -f pc-web >/dev/null 2>&1
dphone rm -f phone-web >/dev/null 2>&1

echo
echo "== 8. neither machine collects the other's volumes =="
# A lost volume is silently recreated empty by the daemon, so the container
# gets an empty directory where the project should be.
volumes() { hostdocker exec "$CONTAINER" docker volume ls -q 2>/dev/null | grep '^rd-' | sort; }

before=$(volumes)
echo "$before" | sed 's/^/    /'
if [ "$(echo "$before" | grep -c .)" -ge 2 ]; then
    ok "each machine created a volume of its own"
else
    bad "the two machines did not create two volumes"
fi

# The NAME carries the machine, rd-<client>-, which a count cannot see: two
# machines sharing one name silently rebuild each other's volume.
if [ "$(echo "$before" | grep -c '^rd-[0-9a-f]\{8\}-')" -ge 2 ]; then
    ok "the volume names carry the machine that created them"
else
    bad "a volume does not name the machine that created it"
fi
# The phone's prefix, read off a volume only the phone mounts, so the check
# below is about the phone's volumes and nothing else.
dphone create --name tc-phone-probe -v "$WORK/project-$PHONE:/w" alpine:3 true >/dev/null 2>&1
phone_prefix=$(dphone inspect -f '{{range .Mounts}}{{.Name}}{{end}}' tc-phone-probe 2>&1 | grep -o '^rd-[0-9a-f]\{8\}-')
dphone rm -f tc-phone-probe >/dev/null 2>&1

gc_out=$(
    cd "$WORK/project-$PC" || exit 1
    REMOTE_DOCKER_STATE_DIR="$WORK/state-$PC" \
    REMOTE_DOCKER_HOST=127.0.0.1 \
    REMOTE_DOCKER_PORT=$SSH_PORT \
    REMOTE_DOCKER_USER="$ACCOUNT" \
        "$WORK/remote-docker" remote gc 2>&1
)
gc_status=$?
after=$(volumes)

if [ -z "$phone_prefix" ]; then
    bad "could not tell which volumes are the phone's"
elif [ "$gc_status" -ne 0 ]; then
    bad "the pc's gc failed ($gc_status): $gc_out"
else
    lost=""
    for volume in $(grep "^$phone_prefix" <<<"$before"); do
        grep -qx "$volume" <<<"$after" || lost="$lost $volume"
    done
    if [ -z "$(grep "^$phone_prefix" <<<"$before")" ]; then
        bad "the phone had no volume before the collection: [$before]"
    elif [ -z "$lost" ]; then
        ok "the pc's collection left every one of the phone's volumes"
    else
        bad "the pc's collection removed the phone's volumes:$lost"
    fi
fi

phone_again=$(dphone run --rm -v "$WORK/project-$PHONE:/w" alpine:3 cat /w/marker 2>&1 | tail -1)
if [ "$phone_again" = "from the phone" ]; then
    ok "the phone still mounts its own directory after the pc collected"
else
    bad "the phone's mount broke after the pc collected: $phone_again"
fi

echo
echo "== 9. an ephemeral share on one machine, a write-back one on the other =="
# The write axis across machines: the phone's write comes back to the phone,
# the pc's ephemeral write reaches nobody.
pc_eph="$WORK/project-$PC-ephemeral"
phone_back="$WORK/project-$PHONE-back"
mkdir -p "$pc_eph" "$phone_back"
echo "from the pc" >"$pc_eph/marker"
echo "from the phone" >"$phone_back/marker"

pc_ok=false
phone_ok=false
if out=$(dpc run -d --name tc-pc-eph -v "$pc_eph:/w:read=cached,write=ephemeral" alpine:3 sleep 120 2>&1); then
    pc_ok=true
else
    bad "the pc could not mount an ephemeral share: $(echo "$out" | tail -3)"
fi
if out=$(dphone run -d --name tc-phone-back -v "$phone_back:/w:read=direct,write=back" alpine:3 sleep 120 2>&1); then
    phone_ok=true
else
    bad "the phone could not mount a write-back share: $(echo "$out" | tail -3)"
fi

if [ "$pc_ok" = true ] && [ "$phone_ok" = true ]; then
    if union_is_fuse dpc tc-pc-eph; then
        ok "tc-pc-eph: the share is a union"
    else
        bad "tc-pc-eph: /w is not a fuse mount: [$LAST_OUTPUT]"
    fi
    if union_is_fuse dphone tc-phone-back; then
        ok "tc-phone-back: the share is a union"
    else
        bad "tc-phone-back: /w is not a fuse mount: [$LAST_OUTPUT]"
    fi
    if [ "$(dpc exec tc-pc-eph cat /w/marker 2>&1)" = "from the pc" ] &&
        [ "$(dphone exec tc-phone-back cat /w/marker 2>&1)" = "from the phone" ]; then
        ok "each union serves its own machine's file"
    else
        bad "a union served the wrong machine's file"
    fi

    # Read back inside the container, or the ephemeral check below passes on a
    # write that never happened.
    if ! out=$(dpc exec tc-pc-eph sh -c 'echo "pc wrote this" >/w/out.txt && cat /w/out.txt' 2>&1) ||
        [ "$out" != "pc wrote this" ]; then
        bad "the pc could not write into its ephemeral share: [$out]"
    fi
    if ! out=$(dphone exec tc-phone-back sh -c 'echo "phone wrote this" >/w/out.txt' 2>&1); then
        bad "the phone could not write into its write-back share: [$out]"
    fi
    if phone_got=$(wait_for_content "$phone_back/out.txt" "phone wrote this" 30); then
        ok "the phone's write came back to the phone's directory"
    else
        bad "the phone's write did not come back: [$phone_got]"
    fi
    if pc_got=$(wait_for_content "$pc_eph/out.txt" "pc wrote this" 30); then
        bad "an ephemeral write came back to the pc: [$pc_got]"
    else
        ok "the pc's ephemeral write reached nobody, 30s on"
    fi
else
    info "a corner could not be mounted, so the cross-machine write assertions were skipped"
fi
dpc rm -f tc-pc-eph >/dev/null 2>&1
dphone rm -f tc-phone-back >/dev/null 2>&1

echo
summary
