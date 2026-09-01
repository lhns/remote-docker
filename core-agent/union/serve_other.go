//go:build !linux

package union

import "errors"

// The agent is Linux-only; these exist so the module still builds on the
// development machine, which has neither Docker nor a mount namespace to enter.
var errUnsupported = errors.New("union: mount namespaces are Linux-only")

// Serve is the child's work, and there is none to do here.
func Serve(Spec) error { return errUnsupported }

// Release is the same.
func Release(Spec) error { return errUnsupported }
