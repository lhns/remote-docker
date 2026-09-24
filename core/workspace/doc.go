// Package workspace is the names and numbers the remote-docker client and the
// remote-dockerd agent both derive: share ids, export paths, volume names, NFS
// mount options, ownership labels, mount modes, the uid<->port mapping,
// this machine's id, and the workspace-info handshake.
//
// Both binaries import it, so a derivation that drifted between two copies,
// as the uid->port mapping once did, cannot recur. The channel protocols each
// have their own package (ADR 0021), and a type only one side uses does not
// belong here.
package workspace
