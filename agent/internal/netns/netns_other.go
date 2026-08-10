//go:build !linux

package netns

import "errors"

// ErrUnsupported is returned everywhere but Linux, for a NAMED namespace.
//
// The stub exists so the whole module still builds and lints on the
// development machine, which by the premise of this project has no Docker and
// no Linux (see CLAUDE.md). It is not a portability claim: network namespaces
// are a Linux facility and the agent only ever runs there.
//
// The empty path is a different question and is answered in netns.go, on every
// platform: "this namespace" needs no system call, so the shared-daemon paths
// through Listen and Dial work here too -- which is what lets them be tested on
// the machine this project is developed on.
var ErrUnsupported = errors.New("netns: network namespaces are Linux-only")

func enter(string, func() error) error { return ErrUnsupported }
