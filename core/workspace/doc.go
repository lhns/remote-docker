// Package workspace is the names and numbers the remote-docker client and the
// remote-dockerd agent both derive: share ids, export paths, volume names, NFS
// mount options, ownership labels, mount modes, the uid<->port mapping,
// this machine's id, and the workspace-info handshake.
//
// Everything in here is imported by both binaries, and that is the point. The
// uid->port mapping once lived in two shell scripts, and when they disagreed
// the client tunnelled to one port while the mount read another, which
// presented as a network fault rather than as drift. One function makes that
// class of bug a compile error.
//
// The channel PROTOCOLS are not here. Each one holds its whole agreement --
// name, version, frames, payload -- in a package of its own: core/notify,
// core/cache, and core/tunnel for the transport itself (ADR 0021).
//
// Nothing here may depend on client- or server-only concerns. If a type is
// only used on one side, it does not belong in this package.
package workspace
