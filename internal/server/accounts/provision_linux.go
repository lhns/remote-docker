//go:build linux

package accounts

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
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
	// Groups the account joins. Empty means the shared-daemon default,
	// {"docker", "workspace"}: "docker" is what gives access to the shared
	// inner daemon, and "workspace" marks the account as ours.
	Groups []string

	// Revoke names groups an existing account must NOT be in.
	//
	// Needed because Ensure returns early for an account that already exists,
	// so changing Groups alone would apply to new accounts only -- and on an
	// upgraded workspace every account already exists. With a daemon per
	// account (ADR 0019) that would leave every one of them still in the
	// `docker` group, holding a socket that reaches the PARENT daemon, which
	// can see and control every account's dind. The separation would be a
	// claim rather than a fact, on exactly the workspaces that had users.
	Revoke []string
}

// Ensure creates the account if it is missing and returns its home directory.
func (p *UnixProvisioner) Ensure(name string, uid int, shell string) (string, error) {
	if u, err := user.Lookup(name); err == nil {
		p.revoke(name)
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

// revoke removes an existing account from groups it must no longer be in.
//
// Best effort and deliberately quiet about the ordinary case: `gpasswd -d`
// fails when the account was never a member, which is what it looks like every
// time after the first. A failure that matters shows up as the group still
// being there, which the integration suite asserts rather than trusting this.
func (p *UnixProvisioner) revoke(name string) {
	for _, group := range p.Revoke {
		if !inGroup(name, group) {
			continue
		}
		if out, err := exec.Command("gpasswd", "-d", name, group).CombinedOutput(); err != nil {
			log.Printf("accounts: could not remove %s from %s: %v: %s", name, group, err, out)
		}
	}
}

// inGroup reports whether an account is a member of a group.
func inGroup(name, group string) bool {
	g, err := user.LookupGroup(group)
	if err != nil {
		return false
	}
	u, err := user.Lookup(name)
	if err != nil {
		return false
	}
	ids, err := u.GroupIds()
	if err != nil {
		return false
	}
	return slices.Contains(ids, g.Gid)
}
