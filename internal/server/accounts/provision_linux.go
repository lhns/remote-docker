//go:build linux

package accounts

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// UnixProvisioner creates real unix accounts.
//
// It shells out to useradd rather than editing /etc/passwd directly. The
// shadow tooling handles the group file, the home directory skeleton and the
// locking between them; reimplementing that to avoid one dependency would be
// trading a well-understood tool for a novel source of corruption.
type UnixProvisioner struct {
	// Groups the account joins. "docker" is what gives access to the inner
	// daemon; "workspace" marks it as ours.
	Groups []string
}

// Ensure creates the account if it is missing and returns its home directory.
func (p *UnixProvisioner) Ensure(name string, uid int, shell string) (string, error) {
	if u, err := user.Lookup(name); err == nil {
		return u.HomeDir, nil
	}

	groups := p.Groups
	if len(groups) == 0 {
		groups = []string{"docker", "workspace"}
	}

	args := []string{
		"--uid", strconv.Itoa(uid),
		"--create-home",
		"--shell", shell,
		"--groups", strings.Join(groups, ","),
		name,
	}
	if out, err := exec.Command("useradd", args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("useradd %s: %w: %s", name, err, out)
	}

	// '*' disables password login. Deliberately NOT '!' (locked): some sshd
	// builds refuse public-key authentication for a locked account, and that
	// failure is silent and baffling. Kept even though this agent does its own
	// authentication, because a deployment may still run sshd alongside.
	if out, err := exec.Command("usermod", "-p", "*", name).CombinedOutput(); err != nil {
		return "", fmt.Errorf("usermod %s: %w: %s", name, err, out)
	}

	u, err := user.Lookup(name)
	if err != nil {
		return "", fmt.Errorf("accounts: %s was created but cannot be looked up: %w", name, err)
	}

	// The workspace mount point, owned by the account so it can mount into it.
	workspaceDir := filepath.Join(u.HomeDir, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return "", fmt.Errorf("accounts: creating %s: %w", workspaceDir, err)
	}
	if err := os.Chown(workspaceDir, uid, uid); err != nil {
		return "", fmt.Errorf("accounts: owning %s: %w", workspaceDir, err)
	}

	return u.HomeDir, nil
}
