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

# openssh-server, sudo and inotify-tools left with the shell implementation:
# the agent is its own SSH server, is already root, and watches the keys
# directory with fsnotify. rclone was never used here at all -- it belonged to
# the old client, which downloaded its own copy anyway.
#
# shadow STAYS. The agent shells out to useradd rather than editing
# /etc/passwd, because the shadow tools handle the locking between passwd,
# group and gshadow that hand-editing gets wrong.
#
# openssh-client stays too: a developer shell wants git-over-ssh.
RUN apk add --no-cache \
        bash \
        coreutils \
        fuse-overlayfs \
        git \
        nfs-utils \
        openssh-client \
        shadow \
        tini \
    && rm -rf /var/cache/apk/*

COPY --from=build /out/remote-dockerd /usr/local/bin/remote-dockerd

RUN mkdir -p /etc/workspace/authorized_keys.d /etc/workspace/host_keys

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

# tini reaps the processes the agent forks per session. The agent is the
# entrypoint and `serve` the default, so `command: ["elevate"]` in a Swarm
# deployment selects the other mode without overriding anything else.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/remote-dockerd"]
CMD ["serve"]
