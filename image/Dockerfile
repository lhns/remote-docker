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
# The golang images set GOTOOLCHAIN=local, which turns a go.mod asking for a
# newer Go than this base image into a build failure naming the toolchain
# rather than the dependency that raised the directive. `auto` fetches what
# go.mod asks for; the tag above still tracks it, so that is normally a no-op.
ENV GOTOOLCHAIN=auto
# Stamped by the release workflow. Without it a published image reports "dev",
# which makes "which version is this workspace running" unanswerable.
ARG VERSION=dev
# Only the agent's module and the two it requires: core, and core-agent, which
# is the workspace side with no Docker in it. The client's tree is not copied
# and cannot be resolved from here, so this build cannot break because of a
# dependency the binary never imported (ADR 0021).
#
# go.work is deliberately NOT copied. It would resolve this build through the
# workspace instead of through agent/go.mod's own replace, so a missing require
# would succeed here and fail only where there is no workspace.
COPY core/ ./core/
COPY core-agent/ ./core-agent/
COPY agent/ ./agent/
# No `go mod download` layer: the agent's whole dependency set is 28 lines of
# go.sum, so caching it separately buys nothing.
RUN cd agent && CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/remote-dockerd ./cmd/remote-dockerd

FROM docker:29-dind

# shadow STAYS. The agent shells out to useradd rather than editing
# /etc/passwd, because the shadow tools handle the locking between passwd,
# group and gshadow that hand-editing gets wrong. openssh-client is for a
# developer shell wanting git-over-ssh; the agent is its own SSH server and
# needs no openssh-server, no sudo and no inotify-tools.
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

# tini warns whenever it is not PID 1, and the one dind's entrypoint starts for
# dockerd never is: it runs under the agent. Nothing is wrong when it says so,
# but the message reads like a fault and has been reported as one. This makes
# the inner tini a subreaper so it collects its own subtree, and is a no-op for
# the tini that is already PID 1.
ENV TINI_SUBREAPER=1

# tini reaps the processes the agent forks per session. The agent is the
# entrypoint and `serve` the default, so `command: ["elevate"]` in a Swarm
# deployment selects the other mode without overriding anything else.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/remote-dockerd"]
CMD ["serve"]
