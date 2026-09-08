//go:build !linux

package netns

import "errors"

// ErrUnsupported is returned everywhere but Linux, for a NAMED namespace. The
// stub exists so the module still builds on the development machine, and is not
// a portability claim: the agent only runs on Linux.
//
// An EMPTY path is answered in netns.go on every platform, so the shared-daemon
// paths through Listen and Dial are testable here.
var ErrUnsupported = errors.New("netns: network namespaces are Linux-only")

func enter(string, func() error) error { return ErrUnsupported }
