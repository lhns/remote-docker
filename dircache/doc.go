// Package dircache keeps a local cache of a directory tree coherent in both
// directions: prefetch what the consumer reads, within a budget, invalidate
// what changes here, and carry the consumer's writes back.
//
// The test for what belongs here is that the sentence above names no transport
// and no storage. Both reach it through Store, and in this repository they are
// an SSH channel to a workspace and the upper layer of a fuse-overlayfs union
// over an NFS mount (ADR 0044). None of that is visible from in here.
//
// What is NOT a policy decision, and so is not here: the wire format, the tar
// and its codec, the frame bound, the mount. A second Store would supply its
// own and need none of this module's answers changed.
//
// It depends on NOTHING: no third-party package, and not this repository
// either (go.mod says how that is checked). The change and event types it
// needs are declared in types.go rather than imported, and a caller whose own
// types differ converts at the boundary (client/internal/session/cacheobserver.go
// is the one in here). That cost is two field-for-field copies, and it is what
// the module is worth.
//
// The cache is allowed to be incomplete at every moment: a file it does not
// hold is served from the authoritative tree underneath, correctly, just
// slowly. A budget that ran out, a walk still running and a file skipped for
// being excluded are all the same state as a file the prefetch has not reached
// yet, so nothing here can make a share wrong, only slower.
package dircache
