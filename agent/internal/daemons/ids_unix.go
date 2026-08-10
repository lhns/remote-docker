//go:build !windows

package daemons

import (
	"fmt"
	"os/user"
	"strconv"
)

// lookupIDs resolves an account to the uid and gid its socket must belong to.
func lookupIDs(account string) (int, int, error) {
	u, err := user.Lookup(account)
	if err != nil {
		return 0, 0, fmt.Errorf("daemons: looking up %s: %w", account, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("daemons: uid %q for %s: %w", u.Uid, account, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("daemons: gid %q for %s: %w", u.Gid, account, err)
	}
	return uid, gid, nil
}
