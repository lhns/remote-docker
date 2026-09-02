// A cache of a directory tree, kept coherent in both directions.
//
// Fill a local copy from an authoritative tree in a bounded, useful order; keep
// it coherent when either side changes; carry the consumer's writes back. That
// sentence names no transport and no storage, which is the test for being here:
// this module knows nothing of SSH, Docker, tar, zstd or overlayfs. Those reach
// it through Store, and in this repository they are client/internal/session's
// cache channel and the union the workspace mounts (ADR 0044).
//
// A module rather than a package so the engine can be taken WITHOUT the NFS
// server, the file watcher and the SSH client that core-client carries. It has
// no third-party requires at all, and that is the point of it.
module github.com/lhns/remote-docker/dircache

go 1.26.3

// core carries the change and event types, and nothing else. It has no
// third-party requires of its own, so this module still has none -- which is
// why there is no go.sum here and nothing for CI to cache.
require github.com/lhns/remote-docker/core v0.0.0

// The shared module is in this repository, not in a proxy. Same arrangement the
// other modules use, and deliberately not go.work: CI and the image build
// ignore the workspace and build one module at a time, so a missing require
// fails where it is wrong.
replace github.com/lhns/remote-docker/core => ../core
