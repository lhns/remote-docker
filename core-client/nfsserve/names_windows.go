//go:build windows

package nfsserve

// checkNewName refuses a component the host could create and never delete.
// See ntfsNameError.
func checkNewName(component string) error { return ntfsNameError(component) }
