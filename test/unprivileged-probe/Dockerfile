# The image every probe pod runs. Built from the docker:dind-rootless image so
# the engine under test is the one the Docker project ships, then given the
# tools the rows need. A pod admitted under `restricted` is non-root with
# no_new_privs and cannot install anything, so nothing is installed at run time.
# A package that does not exist is named in the build log and skipped: a row
# that needs it then records what is missing.
FROM docker:28-dind-rootless
USER root
RUN for p in bash util-linux util-linux-misc shadow-uidmap fuse-overlayfs fuse3 \
        nfs-utils unfs3 podman slirp4netns passt iproute2 attr procps psmisc \
        curl coreutils findutils busybox-extras; do \
        apk add --no-cache "$p" || echo "PROBE-IMAGE: package $p is not available"; \
    done
COPY unprivileged-probe.sh /usr/local/bin/unprivileged-probe.sh
COPY watchprobe pokeprobe /usr/local/bin/
USER rootless
