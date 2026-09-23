# syntax=docker/dockerfile:1
#
# The remote-docker workspace: a privileged Docker-in-Docker daemon reached
# over SSH, mounting the client's own files as ordinary volumes from an NFS
# export served back through a reverse tunnel.
#
# The build context is the REPOSITORY ROOT, not image/, because the agent is
# built from source here:
#     docker build -f image/Dockerfile .
FROM golang:1.27-alpine AS build
WORKDIR /src
# The golang images set GOTOOLCHAIN=local, which fails on a go.mod asking for a
# newer Go; `auto` fetches it, normally a no-op since the tag tracks go.mod.
ENV GOTOOLCHAIN=auto
# Stamped by the release workflow; otherwise the image reports "dev".
ARG VERSION=dev
# Only the agent's module and the two it requires (ADR 0021). Not go.work, so
# agent/go.mod resolves on its own and a missing require fails here.
COPY core/ ./core/
COPY core-agent/ ./core-agent/
COPY agent/ ./agent/
# No `go mod download` layer: the agent's whole dependency set is 28 lines of
# go.sum, so caching it separately buys nothing.
RUN cd agent && CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/remote-dockerd ./cmd/remote-dockerd

FROM docker:29-dind

# shadow STAYS: the agent shells out to useradd, which gets the locking
# between passwd, group and gshadow right. openssh-client is for cloning over
# ssh in a shell; the agent is its own SSH server.
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

# The tini that dind's entrypoint starts for dockerd runs under the agent, not
# as PID 1, and warns about it in words that read like a fault. As a subreaper
# it does not.
ENV TINI_SUBREAPER=1

# tini reaps what the agent forks per session. `command: ["elevate"]` selects
# the Swarm mode without overriding anything else.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/remote-dockerd"]
CMD ["serve"]
