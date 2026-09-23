package dircache

// The vocabulary this module needs from outside, declared rather than imported
// so it depends on nothing (ADR 0021): what the consumer did, for write-back,
// and what happened here, for invalidation.

// Change is one thing the consumer did to its copy of a share.
type Change struct {
	// Path is within the share: leading slash, forward slashes.
	Path string

	Size int64

	// ModTime is Unix nanoseconds ON THE CONSUMER'S CLOCK, compared with this
	// machine's only for a file both sides changed, after the measured offset.
	ModTime int64

	// Deleted says the consumer removed it.
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
// name. A rename removes the old name; the new one arrives as its own event.
// Missing one leaves a cached copy shadowing a file's absence (ADR 0044).
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
