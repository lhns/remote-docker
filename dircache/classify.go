package dircache

// Telling a write that will work later from one that never will (ADR 0044).

import (
	"errors"
	"strings"
	"syscall"
)

// fault is what a failed write says about trying again.
type fault int

const (
	// faultOther is an error nothing here recognises: that file is left for the
	// next round and the others carry on. A file another program holds open is
	// this, and it is gone a minute later.
	faultOther fault = iota

	// faultSpace is a full disk or an exhausted quota. Every file will fail the
	// same way, so the round stops and tries again after a backoff.
	faultSpace

	// faultName is a path this machine cannot hold however long it waits: a
	// name its filesystem refuses, a path that leaves the share, a link it will
	// not let this account make. It is reported once and not tried again until
	// the file changes.
	faultName
)

// classifyWrite sorts a failed write. The platform decides which errnos mean
// what (classify_windows.go, classify_unix.go).
func classifyWrite(err error) fault {
	if err == nil {
		return faultOther
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch {
		case diskFull(errno):
			return faultSpace
		case unholdableName(errno):
			return faultName
		}
	}
	// os.Root refuses a path that leaves the share with an error it does not
	// export; a link the consumer left pointing out is the usual cause.
	if strings.Contains(err.Error(), "path escapes from parent") {
		return faultName
	}
	var clash *caseClashError
	if errors.As(err, &clash) {
		return faultName
	}
	return faultOther
}

// caseClashError is a file whose name differs only in case from one already
// here, on a filesystem that treats the two as one. Landing it would replace
// the other file with different contents.
type caseClashError struct{ path, existing string }

func (e *caseClashError) Error() string {
	return e.path + " differs only in case from " + e.existing + ", which is already here"
}
