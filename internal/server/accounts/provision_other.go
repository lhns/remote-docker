//go:build !linux

package accounts

import "fmt"

// Ensure always fails off Linux.
//
// The agent only ever runs in a Linux container. This exists so the package
// and its callers compile on a development machine, and it fails loudly rather
// than silently pretending to have provisioned anything.
func (p *UnixProvisioner) Ensure(name string, _ int, _ string) (string, error) {
	return "", fmt.Errorf("accounts: cannot provision %s: the workspace agent only runs on Linux", name)
}
