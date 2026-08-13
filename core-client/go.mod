// The user's own machine, minus Docker.
//
// Serving files over NFSv3 from a virtual export namespace, watching
// directories for changes on three platforms, and this machine's SSH identity.
// None of it knows Docker exists, which is the test for being here (ADR 0031);
// the client binary is the glue that does.
//
// Named for the place rather than a role, because the roles invert: for the
// Docker API this machine is the client, and for NFS it is the SERVER while the
// workspace is the client.
module github.com/lhns/remote-docker/core-client

go 1.26.3

require (
	github.com/coder/websocket v1.8.15
	github.com/fsnotify/fsnotify v1.10.1
	github.com/gliderlabs/ssh v0.3.8
	github.com/go-git/go-billy/v5 v5.9.1
	github.com/lhns/remote-docker/core v0.0.0
	github.com/willscott/go-nfs v0.0.4
	github.com/willscott/go-nfs-client v0.0.0-20240104095149-b44639837b00
	golang.org/x/crypto v0.55.0
)

require (
	github.com/anmitsu/go-shlex v0.0.0-20200514113438-38f4b401e2be // indirect
	github.com/cyphar/filepath-securejoin v0.6.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/rasky/go-xdr v0.0.0-20170124162913-1a41d1a06c93 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

// The shared module is in this repository, not in a proxy. Same arrangement the
// other modules use, and deliberately not go.work: CI and the image build
// ignore the workspace and build one module at a time, so a missing require
// fails where it is wrong.
replace github.com/lhns/remote-docker/core => ../core
