// Provisioning a workspace on this machine, and its lifecycle (ADR 0026).
//
// A WSL distribution or a Hyper-V VM: creating one, locating it, holding it
// open, destroying it, and turning the workspace image into the rootfs one is
// built from. It knows nothing of sessions, exports or Docker's API, which is
// why a machine-backed workspace is an ordinary workspace with a lifecycle
// rather than a second data path.
//
// A module rather than a package because a module is the only thing Go lets
// refuse a dependency (ADR 0021). Third-party requires are legitimate here:
// go-containerregistry pulls the image the rootfs comes from. What it refuses
// is THIS repository, which is the membership test:
//
//	go list -deps ./... | grep 'lhns/remote-docker' | grep -v '/machine'
//
// which must print nothing.
module github.com/lhns/remote-docker/machine

go 1.26.3

require github.com/google/go-containerregistry v0.22.0

require (
	github.com/docker/cli v29.7.2+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.3 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gotest.tools/v3 v3.5.2 // indirect
)
