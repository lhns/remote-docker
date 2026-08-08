//go:build !linux

package netns

import (
	"errors"
	"net"
)

// ErrUnsupported is returned everywhere but Linux.
//
// The stubs exist so the whole module still builds and lints on the
// development machine, which by the premise of this project has no Docker and
// no Linux (see CLAUDE.md). They are not a portability claim: network
// namespaces are a Linux facility and the agent only ever runs there.
var ErrUnsupported = errors.New("netns: network namespaces are Linux-only")

func Do(string, func() error) error { return ErrUnsupported }

func Listen(string, string, string) (net.Listener, error) { return nil, ErrUnsupported }

func Dial(string, string, string) (net.Conn, error) { return nil, ErrUnsupported }
