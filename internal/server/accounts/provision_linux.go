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

// Ensure creates the account if it is missing and returns its home directory.
func (p *UnixProvisioner) Ensure(name string, uid int, shell string) (string, error) {
	if u, err := user.Lookup(name); err == nil {
		p.reconcileGroups(name)
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
func (p *UnixProvisioner) reconcileGroups(name string) {
	for _, group := range p.Revoke {
		if !inGroup(name, group) {
			continue
		}
		if out, err := exec.Command("gpasswd", "-d", name, group).CombinedOutput(); err != nil {
			log.Printf("accounts: could not remove %s from %s: %v: %s", name, group, err, out)
		}
	}

	// And BACK IN, which was missing and stranded people.
	//
	// Revoking was added so that switching to a daemon per account took the
	// `docker` group away from accounts that already existed -- otherwise they
	// kept a socket reaching the parent daemon and the separation was a claim
	// rather than a fact. It was written in one direction only, so switching
	// BACK to the shared daemon left every existing account out of the group
	// and unable to reach any daemon at all: `docker ps` in a shell answering
	// "permission denied while trying to connect to the Docker daemon socket".
	//
	// Membership is reconciled both ways now. An account that already exists
	// is the normal case on any workspace that has been used, so anything only
	// applied at creation is, in practice, applied to nobody.
	for _, group := range p.Groups {
		if inGroup(name, group) {
			continue
		}
		if out, err := exec.Command("gpasswd", "-a", name, group).CombinedOutput(); err != nil {
			log.Printf("accounts: could not add %s to %s: %v: %s", name, group, err, out)
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
