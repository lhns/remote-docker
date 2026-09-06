# The image the filesystem conformance probe runs in, on both sides: the
# runner's own daemon with a native bind mount (the oracle), and the workspace
# with a share. One image for both, so a difference in the transcripts is the
# mount and never the userland.
#
# The build context is assembled by test/fs-conformance.sh: this file beside
# the fsprobe binary that test/lib.sh's build_probe made. git and GNU tar are
# what the probe's own tar and git groups run: without them those groups
# report `no-tar` / `no-git` on one side, which is a difference in the image
# rather than in the mount. ca-certificates is for git over https, which no
# group uses yet.
FROM debian:stable-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends git tar ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY fsprobe /usr/local/bin/fsprobe

ENTRYPOINT ["/usr/local/bin/fsprobe"]
