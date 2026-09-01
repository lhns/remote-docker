package unions

import (
	"testing"
	"time"
)

// The cache is written THROUGH the union, so the fill's own copy of every file
// is in the layer Changes reads. Without this record an idle session is told
// about the whole cached tree every few seconds, in one reply, forever.
func TestAppliedRecord(t *testing.T) {
	var l live
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	l.noteApplied("/pkg/lib.go", 120, at)

	if !l.isApplied("/pkg/lib.go", 120, at) {
		t.Error("what the client wrote was not recognised")
	}

	// A container rewriting it is a change however it differs.
	if l.isApplied("/pkg/lib.go", 121, at) {
		t.Error("a different size counted as the client's own write")
	}
	if l.isApplied("/pkg/lib.go", 120, at.Add(time.Second)) {
		t.Error("a different time counted as the client's own write")
	}

	// A path nothing wrote is a container's doing by definition.
	if l.isApplied("/pkg/new.go", 1, at) {
		t.Error("a path the client never wrote counted as its own")
	}

	// Dropped by an invalidation, so a container recreating it is reported
	// rather than mistaken for the copy that used to be there.
	l.forgetApplied("/pkg/lib.go")
	if l.isApplied("/pkg/lib.go", 120, at) {
		t.Error("a dropped path still counted as the client's own write")
	}
}
