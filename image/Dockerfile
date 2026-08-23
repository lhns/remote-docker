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
FROM golang:1.27-alpine AS build
WORKDIR /src
# The golang images set GOTOOLCHAIN=local, which turns a go.mod that asks for a
# newer Go than the base image into a hard build failure -- and go.mod's
# directive moves on its own, raised by whatever a dependency requires. That is
# what broke this build: a docker/buildx bump took the module to 1.26.3 while
# the tag here still said 1.25, and the error named the toolchain rather than
# the dependency that moved it. `auto` lets the build fetch what go.mod asks
# for instead. The tag above still tracks it, so the fetch is normally a no-op.
ENV GOTOOLCHAIN=auto
# Stamped by the release workflow. Without it a published image reports "dev",
# which makes "which version is this workspace running" unanswerable.
ARG VERSION=dev
# Only the agent's module and the two it requires: the shared one, and space,
# which is the workspace side with no Docker in it. The client's tree --
# docker/cli, buildx, 215 packages -- is not copied and cannot be resolved from
# here, which is the point of ADR 0021: this build could break because of a
# dependency the binary never imported, and now it cannot.
#
# go.work is deliberately NOT copied. It would make this build resolve through
# the workspace instead of through agent/go.mod's own replace, so a missing
# require here would still succeed and only fail somewhere without it.
COPY core/ ./core/
COPY core-agent/ ./core-agent/
COPY agent/ ./agent/
# `go mod download` is deliberately absent. It was here to cache dependencies
# in their own layer; `go build` fetches exactly what this binary needs, which
# after the split is 24 lines of go.sum.
RUN cd agent && CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/remote-dockerd ./cmd/remote-dockerd

FROM docker:29-dind

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
    WORKSPACE_PER_USER_DIND=true \
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
