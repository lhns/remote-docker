//go:build !linux

package accounts

import "fmt"

// Ensure always fails off Linux: the package has to compile on a development
// machine, and failing loudly beats pretending to have provisioned anything.
func (p *UnixProvisioner) Ensure(name string, _ int, _ string) (string, string, error) {
	return "", "", fmt.Errorf("accounts: cannot provision %s: the workspace agent only runs on Linux", name)
}

// Remove fails off Linux, for the reason on Ensure.
func (p *UnixProvisioner) Remove(name string, _ int) error {
	return fmt.Errorf("accounts: cannot remove %s: the workspace agent only runs on Linux", name)
}
