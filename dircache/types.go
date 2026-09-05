package dircache

// The vocabulary this module needs from the outside, and the whole of it.
//
// Declared here rather than imported so the module depends on nothing at all
// (ADR 0021). These are the two directions a cache has to hear about: what the
// CONSUMER did to the cache, which write-back reads, and what happened HERE,
// which invalidation reads. Both are plain data.
//
// A caller whose own types differ converts at the boundary. That conversion is
// field-for-field and belongs to the caller, because only the caller knows
// which of its events mean a path is gone.

// Change is one thing the consumer did to its copy of a share.
type Change struct {
	// Path is within the share: leading slash, forward slashes.
	Path string

	Size int64

	// ModTime is when the consumer wrote it, in Unix nanoseconds ON THE
	// CONSUMER'S CLOCK. Compared against this machine's own only for a file
	// both sides changed, and only after the measured offset is applied. Two
	// clocks that were never set together.
	ModTime int64

	// Deleted says the consumer removed it. Distinguishable from a file that
	// was never cached, which is why write-back can act on it.
	Deleted bool
}

// Op is what happened to a path on this machine. A bitset: one save can be
// several, and an editor writing in place yields OpCreate|OpWrite.
type Op uint8

const (
	OpCreate Op = 1 << iota
	OpWrite
	OpRemove
	OpRename
	OpAttrib
)

// Gone reports whether an op means the path is no longer there under that
// name.
//
// A rename is a removal of the old name, which is the half this module sees:
// the new name arrives as its own event. Getting this wrong leaves a cached
// copy shadowing a file's absence, which is the one failure a cache must not
// have (ADR 0044).
func (o Op) Gone() bool { return o&(OpRemove|OpRename) != 0 }

// Event is one change seen on this machine.
type Event struct {
	// Share is which cache this concerns, in whatever names the caller gave
	// Attach. This module never parses one.
	Share string

	// Path is within the share: leading slash, forward slashes however the
	// local OS spells them. The share root itself is "/".
	Path string

	// Op is the merged operation set. Zero is invalid.
	Op Op

	// Dir says the path is a directory. A directory is not cached in its own
	// right, so these are dropped.
	Dir bool
}

// Notice says the change source could not report everything it saw.
//
// Answered with a reconcile rather than a log line: the dropped events may
// have been deletions, and a cached copy of a file that is gone shadows its
// absence for as long as it sits there.
type Notice struct {
	// Reason is for a person reading a log. Empty is allowed.
	Reason string

	// Dropped is how many events were lost, or 0 when that is not known.
	Dropped int
}
