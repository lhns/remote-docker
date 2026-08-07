# syntax=docker/dockerfile:1
#
# The remote-docker workspace.
#
# A privileged Docker-in-Docker daemon reached over SSH. The client serves its
# own files over NFS through a reverse tunnel, and this daemon mounts them per
# container as ordinary volumes -- so a bind mount naming a path on the
# client's machine resolves, wherever on that machine it lives.
#
# The build context is the REPOSITORY ROOT, not image/, because the agent is
# built from source here. Build with:
#     docker build -f image/Dockerfile .
FROM golang:1.25-alpine AS build
WORKDIR /src
# Dependencies first, so a source change does not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY pkg/ ./pkg/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
        -o /out/remote-dockerd ./cmd/remote-dockerd

FROM docker:28-dind

RUN apk add --no-cache \
        bash \
        coreutils \
        fuse-overlayfs \
        git \
        inotify-tools \
        nfs-utils \
        openssh-client \
        openssh-server \
        rclone \
        shadow \
        sudo \
        tini \
    && rm -rf /var/cache/apk/*

COPY image/sshd_config       /etc/ssh/sshd_config
COPY image/sudoers.workspace /etc/sudoers.d/workspace
COPY image/bin/              /usr/local/bin/
COPY --from=build /out/remote-dockerd /usr/local/bin/remote-dockerd

RUN chmod 0440 /etc/sudoers.d/workspace \
    && chmod 0755 /usr/local/bin/entrypoint.sh \
                  /usr/local/bin/key-watcher \
                  /usr/local/bin/workspace-info \
                  /usr/local/bin/workspace-mount \
                  /usr/local/bin/workspace-umount \
    && mkdir -p /etc/workspace/authorized_keys.d /etc/workspace/host_keys \
    && visudo -c -f /etc/sudoers.d/workspace

# WORKSPACE_UID_BASE / WORKSPACE_PORT_BASE define the mapping
#   reverse-tunnel port = PORT_BASE + (uid - UID_BASE)
# so every enrolled user gets a private, stable port with no coordination.
ENV WORKSPACE_STATE_DIR=/etc/workspace \
    WORKSPACE_KEYS_DIR=/etc/workspace/authorized_keys.d \
    WORKSPACE_HOSTKEY_DIR=/etc/workspace/host_keys \
    WORKSPACE_UID_BASE=10000 \
    WORKSPACE_PORT_BASE=30000 \
    WORKSPACE_SHELL=/bin/bash \
    WORKSPACE_ENABLE_DIND=true \
    WORKSPACE_DOCKERD_ARGS="" \
    WORKSPACE_KEY_POLL_INTERVAL=60 \
    DOCKER_TLS_CERTDIR=

EXPOSE 2222

# Keep the graph driver on real local disk. On CephFS/NFS-backed appdata,
# overlay2 will not work -- set WORKSPACE_DOCKERD_ARGS=--storage-driver=fuse-overlayfs
VOLUME ["/var/lib/docker"]

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/entrypoint.sh"]
