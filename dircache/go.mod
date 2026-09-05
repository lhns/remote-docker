// A cache of a directory tree, kept coherent in both directions.
//
// A module rather than a package because a module is the only thing Go lets
// refuse a dependency (ADR 0021). This one has NONE: not a third-party
// package, and not this repository either. That is the membership test, and it
// is checkable in one command rather than by reading:
//
//	go list -deps ./... | grep -v '^github.com/lhns/remote-docker/dircache' | grep -E '\.[^/]+/'
//
// which must print nothing. There is no go.sum here and nothing for CI to
// cache.
module github.com/lhns/remote-docker/dircache

go 1.26.3
