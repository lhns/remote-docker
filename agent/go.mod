// The workspace agent.
//
// Its own module, and the reason for ADR 0021: it compiles against seven
// third-party modules while the client's tree is 215 packages. Sharing one
// go.mod meant a docker/buildx bump raising the Go directive past the image's
// pinned toolchain and breaking a binary that imports no buildx.
module github.com/lhns/remote-docker/agent

go 1.26.3

require (
	github.com/creack/pty v1.1.24
	github.com/gliderlabs/ssh v0.3.8
	github.com/klauspost/compress v1.20.0
	github.com/lhns/remote-docker/core v0.0.0
	github.com/lhns/remote-docker/core-agent v0.0.0
	github.com/spf13/cobra v1.10.2
	golang.org/x/crypto v0.55.0
)

require (
	github.com/anmitsu/go-shlex v0.0.0-20200514113438-38f4b401e2be // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/lhns/remote-docker/core => ../core

replace github.com/lhns/remote-docker/core-agent => ../core-agent
