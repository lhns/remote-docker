// Package workspace is the contract shared by the remote-docker client and the
// remote-dockerd server agent.
//
// Everything in here is imported by both binaries, and that is the point. The
// uid->port mapping once lived in two shell scripts, and when they disagreed
// the client tunnelled to one port while the mount read another, which
// presented as a network fault rather than as drift. One function makes that
// class of bug a compile error.
//
// Nothing here may depend on client- or server-only concerns. If a type is
// only used on one side, it does not belong in this package.
package workspace
