#!/bin/sh
# Start dockerd (dind), the authorized-key watcher, and sshd.
set -eu

: "${WORKSPACE_STATE_DIR:=/etc/workspace}"
: "${WORKSPACE_KEYS_DIR:=$WORKSPACE_STATE_DIR/authorized_keys.d}"
: "${WORKSPACE_HOSTKEY_DIR:=$WORKSPACE_STATE_DIR/host_keys}"
: "${WORKSPACE_UID_BASE:=10000}"
: "${WORKSPACE_PORT_BASE:=30000}"
: "${WORKSPACE_SHELL:=/bin/bash}"
: "${WORKSPACE_ENABLE_DIND:=true}"
: "${WORKSPACE_DOCKERD_ARGS:=}"
: "${WORKSPACE_KEY_POLL_INTERVAL:=60}"

log() { echo "[entrypoint] $*" >&2; }

mkdir -p "$WORKSPACE_STATE_DIR" "$WORKSPACE_KEYS_DIR" "$WORKSPACE_HOSTKEY_DIR" /var/run/sshd

# Persist the settings where sudo/ssh non-login sessions can read them.
# workspace-info and workspace-mount both source this file, so the uid->port
# mapping can never disagree between the two.
cat >"$WORKSPACE_STATE_DIR/config" <<EOF
WORKSPACE_STATE_DIR=$WORKSPACE_STATE_DIR
WORKSPACE_KEYS_DIR=$WORKSPACE_KEYS_DIR
WORKSPACE_UID_BASE=$WORKSPACE_UID_BASE
WORKSPACE_PORT_BASE=$WORKSPACE_PORT_BASE
WORKSPACE_SHELL=$WORKSPACE_SHELL
EOF
chmod 0644 "$WORKSPACE_STATE_DIR/config"

# --- host keys -------------------------------------------------------------
# Kept on the state volume so clients do not get a host-key-changed warning
# every time the container is recreated.
for type in ed25519 rsa; do
    key="$WORKSPACE_HOSTKEY_DIR/ssh_host_${type}_key"
    if [ ! -f "$key" ]; then
        log "generating $type host key"
        ssh-keygen -q -t "$type" -N '' -C "docker-ssh-workspace" -f "$key"
    fi
    chmod 0600 "$key"
    chmod 0644 "$key.pub" 2>/dev/null || true
done

# --- groups ----------------------------------------------------------------
getent group docker    >/dev/null 2>&1 || addgroup -S docker
getent group workspace >/dev/null 2>&1 || addgroup -S workspace

# --- dockerd ---------------------------------------------------------------
DOCKERD_PID=
if [ "$WORKSPACE_ENABLE_DIND" = "true" ]; then
    log "starting dockerd"
    # dockerd-entrypoint.sh prepends `dockerd` when the first arg starts with -.
    # shellcheck disable=SC2086
    dockerd-entrypoint.sh --group docker $WORKSPACE_DOCKERD_ARGS &
    DOCKERD_PID=$!

    # Wait for the socket so the first `docker ps` after login does not fail.
    i=0
    while [ $i -lt 60 ]; do
        [ -S /var/run/docker.sock ] && break
        i=$((i + 1))
        sleep 1
    done
    if [ -S /var/run/docker.sock ]; then
        chgrp docker /var/run/docker.sock 2>/dev/null || true
        chmod 0660 /var/run/docker.sock 2>/dev/null || true
        log "dockerd ready"
    else
        log "WARNING: dockerd socket did not appear within 60s"
    fi
fi

# --- key watcher -----------------------------------------------------------
log "starting key watcher on $WORKSPACE_KEYS_DIR"
/usr/local/bin/key-watcher &
WATCHER_PID=$!

terminate() {
    log "shutting down"
    if [ -n "$WATCHER_PID" ]; then kill "$WATCHER_PID" 2>/dev/null || true; fi
    if [ -n "$DOCKERD_PID" ]; then
        kill -TERM "$DOCKERD_PID" 2>/dev/null || true
        wait "$DOCKERD_PID" 2>/dev/null || true
    fi
    exit 0
}
trap terminate TERM INT

log "starting sshd on port 2222"
/usr/sbin/sshd -D -e &
SSHD_PID=$!
wait "$SSHD_PID"
