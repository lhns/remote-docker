//go:build linux

package union

import (
	"strings"

	"github.com/lhns/remote-docker/core-agent/nofollow"
)

// split names p, a path inside the daemon whose root is given, as the trusted
// directory to open and the path below it that is resolved without following a
// link.
//
// With a daemon per account that is the daemon's root (/proc/<pid>/root) and
// all of p: the account is root in that filesystem and may have put a link
// anywhere in it, /run/rd-union included. With the shared daemon the root is
// the agent's own, which the operator made, so the walk starts at Root, the
// directory the agent creates for unions, and only what is below is checked.
func split(root, p string) (trusted, rel string) {
	if root == "" || root == "/" {
		if p == Root || strings.HasPrefix(p, Root+"/") {
			return Root, strings.TrimPrefix(p, Root)
		}
		return "/", p
	}
	return root, p
}

// mountedAt reports whether anything is mounted at p inside the daemon whose
// root is given: whether it is on a different device from its parent. That
// answers from outside the owning namespace too, through /proc/<pid>/root
// (test/union-probe.sh section 12).
//
// A link anywhere in p answers false, never a follow (nofollow.Open): a union's
// directories are made by the agent and are not links.
//
// Never "the path exists": a union's directories outlive it, so a stat calls a
// union that never mounted serving, and the container binds an empty directory
// (CLAUDE.md, "A union that never mounted looks exactly like one that did").
func mountedAt(root, p string) bool {
	trusted, rel := split(root, p)
	self, parent, err := nofollow.Dev(trusted, rel)
	return err == nil && self != parent
}

// listDir lists the directory p inside the daemon whose root is given, without
// following a link.
func listDir(root, p string) ([]string, error) {
	trusted, rel := split(root, p)
	return nofollow.ReadDir(trusted, rel)
}
