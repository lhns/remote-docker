#!/usr/bin/env bash
# dockerbox -- run Docker on a remote host as if it were local, with the
# current directory mounted into it over a reverse SSH tunnel.
#
# POSIX counterpart of dockerbox.ps1. Same protocol, same server side.
#
#   dockerbox shell                 mount $PWD and open a shell in it
#   dockerbox docker compose up -d  run one docker command against it
#   dockerbox run make test         run any command in the workspace
#   dockerbox mount / umount        manage a persistent background session
#   dockerbox status                show remote + local session state
#   dockerbox enroll                print the public key to be enrolled
#
# Config: $DOCKERBOX_HOST, $DOCKERBOX_SSH_PORT, $DOCKERBOX_USER,
#         or ~/.dockerbox.json
set -euo pipefail

STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/dockerbox"
SESSION_FILE="$STATE_DIR/session"
KEY_PATH="$HOME/.ssh/dockerbox_ed25519"
CONFIG_FILE="$HOME/.dockerbox.json"
CIPHER="${DOCKERBOX_CIPHER:-aes128-gcm@openssh.com}"

step() { printf '\033[36m==> %s\033[0m\n' "$*" >&2; }
info() { printf '\033[90m    %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[31m!!  %s\033[0m\n' "$*" >&2; exit 1; }

mkdir -p "$STATE_DIR"

# ---------------------------------------------------------------- config ---

json_get() {  # json_get <file> <key>  -- tiny reader, avoids a jq dependency
    sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\{0,1\}\([^,\"}]*\)\"\{0,1\}.*/\1/p" "$1" | head -1
}

SERVER=""; SSH_PORT="2222"; SSH_USER="$(id -un)"
if [ -f "$CONFIG_FILE" ]; then
    v=$(json_get "$CONFIG_FILE" Server)  && [ -n "$v" ] && SERVER="$v"
    v=$(json_get "$CONFIG_FILE" SshPort) && [ -n "$v" ] && SSH_PORT="$v"
    v=$(json_get "$CONFIG_FILE" User)    && [ -n "$v" ] && SSH_USER="$v"
fi
SERVER="${DOCKERBOX_HOST:-$SERVER}"
SSH_PORT="${DOCKERBOX_SSH_PORT:-$SSH_PORT}"
SSH_USER="${DOCKERBOX_USER:-$SSH_USER}"

FORWARDS=()
while [ $# -gt 0 ]; do
    case "$1" in
        -L) FORWARDS+=("$2"); shift 2 ;;
        -H|--host) SERVER="$2"; shift 2 ;;
        -p|--port) SSH_PORT="$2"; shift 2 ;;
        -u|--user) SSH_USER="$2"; shift 2 ;;
        --) shift; break ;;
        *) break ;;
    esac
done

CMD="${1:-shell}"; [ $# -gt 0 ] && shift || true

[ -n "$SERVER" ] || die "no workspace host configured; set \$DOCKERBOX_HOST or write $CONFIG_FILE"

# ------------------------------------------------------------------ ssh ----

# Connection multiplexing is a large win here (one handshake instead of one
# per command) but Win32-OpenSSH does not implement it, so only enable it
# where we know it works.
CONTROL_ARGS=()
case "$(uname -s)" in
    Linux|Darwin|FreeBSD)
        CONTROL_ARGS=(-o "ControlMaster=auto"
                      -o "ControlPath=$STATE_DIR/cm-%r@%h:%p"
                      -o "ControlPersist=300")
        ;;
esac

ssh_args() {
    printf '%s\n' \
        -o StrictHostKeyChecking=accept-new \
        -o ServerAliveInterval=15 \
        -o ServerAliveCountMax=3 \
        -o Compression=no \
        -o LogLevel=ERROR \
        -i "$KEY_PATH" \
        -p "$SSH_PORT" \
        -c "$CIPHER" \
        ${CONTROL_ARGS[@]+"${CONTROL_ARGS[@]}"}
}

# Run a shell snippet on the workspace, base64-wrapped so no layer of quoting
# can mangle it.
remote() {
    local script="$1"; shift
    local b64; b64=$(printf '%s' "$script" | base64 | tr -d '\n')
    mapfile -t a < <(ssh_args)
    ssh "${a[@]}" "$@" "$SSH_USER@$SERVER" "echo $b64 | base64 -d | /bin/sh -s"
}

shell_quote() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

# ----------------------------------------------------------------- keys ----

ensure_key() {
    [ -f "$KEY_PATH" ] && return 0
    step "generating a keypair for this machine"
    mkdir -p "$(dirname "$KEY_PATH")"; chmod 700 "$(dirname "$KEY_PATH")"
    ssh-keygen -t ed25519 -N '' -C "dockerbox-$(hostname)-$(id -un)" -f "$KEY_PATH"
    show_enrollment
    exit 0
}

show_enrollment() {
    echo
    echo "Give this to whoever runs the workspace container."
    echo "It must be saved as: authorized_keys.d/$SSH_USER.pub"
    echo "(the filename becomes your unix account name)"
    echo
    cat "$KEY_PATH.pub"
    echo
}

remote_info() {
    remote 'workspace-info' 2>/dev/null \
        || die "cannot reach $SSH_USER@$SERVER:$SSH_PORT -- if this machine is new, run: dockerbox enroll"
}

# -------------------------------------------------------------- session ----

free_port() {
    python3 - <<'EOF' 2>/dev/null || echo $(( (RANDOM % 20000) + 40000 ))
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
EOF
}

ensure_rclone() {
    if command -v rclone >/dev/null 2>&1; then echo rclone; return; fi
    [ -x "$STATE_DIR/rclone" ] && { echo "$STATE_DIR/rclone"; return; }
    die "rclone not found -- install it, or drop the binary at $STATE_DIR/rclone"
}

session_live() {
    [ -f "$SESSION_FILE" ] || return 1
    # shellcheck disable=SC1090
    . "$SESSION_FILE"
    kill -0 "$S_TUNNEL_PID" 2>/dev/null || return 1
    kill -0 "$S_RCLONE_PID" 2>/dev/null || return 1
    [ "$S_SERVER" = "$SERVER" ] && [ "$S_USER" = "$SSH_USER" ]
}

start_session() {
    local path="$1"

    if session_live; then
        # shellcheck disable=SC1090
        . "$SESSION_FILE"
        if [ "$S_PATH" = "$path" ]; then info "reusing session (pid $S_TUNNEL_PID)"; return 0; fi
        info "a session is open for a different directory; replacing it"
        stop_session
    fi

    local rclone; rclone=$(ensure_rclone)
    local remote_port; remote_port=$(remote_info | sed -n 's/^WORKSPACE_NFS_PORT=//p')
    [ -n "$remote_port" ] || die "workspace-info did not report a port"
    local local_port; local_port=$(free_port)

    step "serving $path over NFS on 127.0.0.1:$local_port"
    "$rclone" serve nfs "$path" \
        --addr "127.0.0.1:$local_port" \
        --file-perms 0666 --dir-perms 0777 \
        --nfs-cache-handle-limit 1000000 \
        --log-file "$STATE_DIR/rclone.log" --log-level NOTICE &
    local rclone_pid=$!

    step "opening tunnel to $SSH_USER@$SERVER:$SSH_PORT (remote port $remote_port)"
    mapfile -t a < <(ssh_args)
    ssh "${a[@]}" -N -o ExitOnForwardFailure=yes \
        -R "127.0.0.1:${remote_port}:127.0.0.1:${local_port}" \
        ${FORWARDS[@]+"${FORWARDS[@]/#/-L}"} \
        "$SSH_USER@$SERVER" &
    local tunnel_pid=$!

    local ready=false
    for _ in $(seq 1 20); do
        sleep 0.5
        kill -0 "$tunnel_pid" 2>/dev/null || die "ssh tunnel exited -- is port $remote_port already bound on the workspace?"
        if remote "nc -z 127.0.0.1 $remote_port" >/dev/null 2>&1; then ready=true; break; fi
    done
    [ "$ready" = true ] || die "the reverse tunnel never came up on the workspace side"

    # --force because this is a brand new rclone process: its NFS file handles
    # are freshly generated, so any pre-existing mount is now stale.
    step "mounting on the workspace"
    remote 'sudo workspace-mount --force' || die "remote mount failed"

    cat >"$SESSION_FILE" <<EOF
S_SERVER=$SERVER
S_USER=$SSH_USER
S_PATH=$path
S_LOCAL_PORT=$local_port
S_REMOTE_PORT=$remote_port
S_RCLONE_PID=$rclone_pid
S_TUNNEL_PID=$tunnel_pid
EOF
}

stop_session() {
    [ -f "$SESSION_FILE" ] || return 0
    # shellcheck disable=SC1090
    . "$SESSION_FILE"
    step "closing session"
    remote 'sudo workspace-umount' >/dev/null 2>&1 || true
    kill "$S_TUNNEL_PID" "$S_RCLONE_PID" 2>/dev/null || true
    rm -f "$SESSION_FILE"
}

# ----------------------------------------------------------------- main ----

case "$CMD" in
    key|enroll)
        ensure_key
        show_enrollment
        ;;
    status)
        ensure_key
        remote_info
        if session_live; then
            # shellcheck disable=SC1090
            . "$SESSION_FILE"
            echo "LOCAL_SESSION=$S_PATH (tunnel pid $S_TUNNEL_PID)"
        else
            echo "LOCAL_SESSION=none"
        fi
        ;;
    umount)
        stop_session
        ;;
    mount)
        ensure_key
        start_session "$PWD"
        info "session stays open in the background; close it with: dockerbox umount"
        ;;
    shell)
        ensure_key
        start_session "$PWD"
        mapfile -t a < <(ssh_args)
        ssh "${a[@]}" -t "$SSH_USER@$SERVER" 'cd ~/workspace && exec bash -l' || true
        stop_session
        ;;
    run|docker)
        ensure_key
        start_session "$PWD"
        parts=""
        [ "$CMD" = docker ] && parts="docker"
        for x in "$@"; do parts="$parts $(shell_quote "$x")"; done
        parts="${parts# }"
        [ -n "$parts" ] || die "nothing to run"
        remote "cd ~/workspace && exec $parts" -t
        ;;
    help|-h|--help)
        sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
        ;;
    *)
        die "unknown command: $CMD (try: dockerbox help)"
        ;;
esac
