//go:build windows

package proxy

import (
	"errors"
	"syscall"
)

// Windows reports a peer that has gone away with its own error numbers, and
// they arrive as text like "The pipe has been ended.", which reads alarming
// in a terminal and means nothing more than "the client hung up".
const (
	errorBrokenPipe       = syscall.Errno(109) // ERROR_BROKEN_PIPE
	errorNoData           = syscall.Errno(232) // ERROR_NO_DATA, a pipe closing
	errorPipeNotConnected = syscall.Errno(233)
	wsaeConnReset         = syscall.Errno(10054)
	wsaeConnAborted       = syscall.Errno(10053)
)

func peerGone(err error) bool {
	return errors.Is(err, errorBrokenPipe) ||
		errors.Is(err, errorNoData) ||
		errors.Is(err, errorPipeNotConnected) ||
		errors.Is(err, wsaeConnReset) ||
		errors.Is(err, wsaeConnAborted)
}
