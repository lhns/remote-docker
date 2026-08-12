// The workspace side of a remote-docker deployment, minus Docker.
//
// One unix account per enrolled key, running a function inside another
// process's network namespace, and replaying a client's filesystem changes as
// real syscalls. None of it knows Docker exists, which is the test for being
// here: this module reaches none of dockercli, daemons, supervise or elevate,
// and the agent binary is the glue that does.
//
// Its own module rather than a directory in the agent so that the boundary is
// enforced by the compiler rather than by whoever reviews the next import.
module github.com/lhns/remote-docker/core-agent

go 1.26.3

require (
	github.com/fsnotify/fsnotify v1.10.1
	github.com/gliderlabs/ssh v0.3.8
	github.com/lhns/remote-docker/core v0.0.0
	golang.org/x/crypto v0.55.0
	golang.org/x/sys v0.47.0
)

require github.com/anmitsu/go-shlex v0.0.0-20200514113438-38f4b401e2be // indirect

// The shared module is in this repository, not in a proxy. Same arrangement the
// agent and client modules use, and deliberately not go.work: CI and the image
// build ignore the workspace and build one module at a time, so a missing
// require fails where it is wrong.
replace github.com/lhns/remote-docker/core => ../core
