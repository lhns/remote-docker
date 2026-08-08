//go:build !windows

package proxy

import (
	"errors"
	"syscall"
)

func peerGone(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}
