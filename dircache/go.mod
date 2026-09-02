// A cache of a directory tree, kept coherent in both directions.
//
// A module rather than a package because a module is the only thing Go lets
// refuse a dependency. This one has none, and inside core-client it could not
// keep that -- which is the whole reason this file exists (ADR 0021).
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
