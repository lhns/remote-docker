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
//
// Keyed on the UID rather than on the name, because that is the identity: the
// uidmap binds it, the reverse-tunnel port comes from it, and the files are
// owned by it. See claim() in unixname.go for the three answers.
func (p *UnixProvisioner) Ensure(name string, uid int, shell string) (string, string, error) {
	prefix := p.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}

	var holder string
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		holder = u.Username
	}

	switch claim(name, prefix, holder) {
	case adoptAccount:
		u, err := user.Lookup(holder)
		if err != nil {
			return "", "", fmt.Errorf("accounts: %s holds uid %d but cannot be looked up: %w", holder, uid, err)
		}
		p.reconcileGroups(holder)
		return holder, u.HomeDir, nil

	case refuseAccount:
		// Named, and not provisioned. Adopting it would hand whoever holds
		// this key the files of a user the workspace never created.
		return "", "", fmt.Errorf(
			"accounts: uid %d belongs to %q, which this workspace did not create, so %q was not provisioned\n"+
				"  fix: move WORKSPACE_UID_BASE to a free range, or remove that user",
			uid, holder, name)
	}

	unix := unixName(prefix, name)

	args := []string{
		"--uid", strconv.Itoa(uid),
		"--create-home",
		"--shell", shell,
	}
	// No default. Which groups an account joins is a deployment decision --
	// with a shared daemon it needs `docker` and with one per account it must
	// NOT have it -- and the caller states it in both modes precisely so the
	// two are visible together. A fallback here was a second copy of that
	// decision, unreachable, and free to disagree with the real one.
	if len(p.Groups) > 0 {
		args = append(args, "--groups", strings.Join(p.Groups, ","))
	}
	args = append(args, unix)
	if out, err := exec.Command("useradd", args...).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("useradd %s: %w: %s", unix, err, out)
	}
	name = unix

	// '*' disables password login. Deliberately NOT '!' (locked): some sshd
	// builds refuse public-key authentication for a locked account, and that
	// failure is silent and baffling. Kept even though this agent does its own
	// authentication, because a deployment may still run sshd alongside.
	if out, err := exec.Command("usermod", "-p", "*", name).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("usermod %s: %w: %s", name, err, out)
	}

	u, err := user.Lookup(name)
	if err != nil {
		return "", "", fmt.Errorf("accounts: %s was created but cannot be looked up: %w", name, err)
	}

	// The workspace mount point, owned by the account so it can mount into it.
	workspaceDir := filepath.Join(u.HomeDir, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return "", "", fmt.Errorf("accounts: creating %s: %w", workspaceDir, err)
	}
	if err := os.Chown(workspaceDir, uid, uid); err != nil {
		return "", "", fmt.Errorf("accounts: owning %s: %w", workspaceDir, err)
	}

	return name, u.HomeDir, nil
}

// reconcileGroups brings an existing account's membership into line: out of
// Revoke, into Groups.
//
// Best effort: a failure that matters shows up as the membership being wrong,
// which the integration suite asserts rather than trusting this.
func (p *UnixProvisioner) reconcileGroups(name string) {
	for _, group := range p.Revoke {
		// gpasswd -d fails on an account that was never a member, which would
		// be logged as a failure to revoke something nobody had.
		if !inGroup(name, group) {
			continue
		}
		if out, err := exec.Command("gpasswd", "-d", name, group).CombinedOutput(); err != nil {
			log.Printf("accounts: could not remove %s from %s: %v: %s", name, group, err, out)
		}
	}

	// Both directions. An account that already exists is the normal case on
	// any workspace that has been used, so anything only applied at creation
	// is applied to nobody: revoking alone left every existing account out of
	// `docker` after a switch BACK to the shared daemon, with `docker ps` in a
	// shell answering "permission denied while trying to connect to the
	// Docker daemon socket".
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
