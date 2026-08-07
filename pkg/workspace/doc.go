// Package workspace is the contract shared by the remote-docker client and the
// remote-dockerd server agent.
//
// Everything in here is imported by both binaries. That is the entire point:
// the uid->port mapping used to live in two shell scripts (workspace-info and
// workspace-mount), and when those two copies disagreed the client tunnelled
// to one port while the mount read another -- a failure that presented as a
// network fault rather than as the config drift it was. Keeping the formula in
// one function turns that class of bug into a compile error.
//
// Nothing here may depend on client- or server-only concerns. If a type is
// only used on one side, it does not belong in this package.
package workspace
