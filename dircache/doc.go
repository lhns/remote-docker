// Package dircache keeps a local cache of a directory tree coherent in both
// directions: fill it from an authoritative tree in a bounded, useful order,
// invalidate what changes here, and carry the consumer's writes back.
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
// The one tie to this repository is core/workspace, for the change and event
// types. They are plain structs with no dependencies, and duplicating them
// would mean converting at every boundary; if this module is ever taken
// elsewhere, that is the seam to cut.
package dircache
