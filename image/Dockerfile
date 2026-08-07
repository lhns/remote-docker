# syntax=docker/dockerfile:1
#
# docker-ssh-workspace
#
# A Docker-in-Docker daemon you can SSH into, which mounts YOUR machine's
# current directory over a reverse SSH tunnel. Because dockerd and the NFS
# mount live in the SAME mount namespace, ordinary bind mounts in your
# compose files just work -- no path translation, no volume plugin.
#
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

COPY sshd_config       /etc/ssh/sshd_config
COPY sudoers.workspace /etc/sudoers.d/workspace
COPY bin/              /usr/local/bin/

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
