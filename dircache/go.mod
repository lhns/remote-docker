// A cache of a directory tree, kept coherent in both directions.
//
// Its own module rather than a package in core-client so the engine can be
// taken without that module's seven third-party requires. It has none, and
// keeping it so is the whole reason this file is separate (ADR 0021).
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
