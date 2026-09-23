package accounts

import "github.com/lhns/remote-docker/core/workspace"

// maxNameLength is the account-name cap. core/workspace owns it because the
// client derives the same name from the local username (workspace.AccountName).
const maxNameLength = workspace.MaxAccountNameLength

// DefaultPrefix is what a unix account name starts with, container included:
// on a VM (ADR 0025) the unix user sits in the machine's own passwd file, where
// `alice.pub` taking `alice` would be a claim on somebody else's namespace.
const DefaultPrefix = "rd-"

// unixName is the unix user behind an account, truncated so the prefix does
// not push it past maxNameLength. Two long names that collide are caught by
// the uid lookup in Ensure.
func unixName(prefix, account string) string {
	room := maxNameLength - len(prefix)
	if room < 1 {
		// A configuration error: useradd refuses the length, naming it.
		return account
	}
	if len(account) > room {
		account = account[:room]
	}
	return prefix + account
}

// claim says what to do about the unix user, if any, already holding the uid
// this account is mapped to. The uid is the identity (CLAUDE.md, "The unix
// account name is not the account name"): an unprefixed `alice` from an older
// workspace is adopted as it stands, and a stranger is refused, since adopting
// one hands an enrolled key somebody else's files.
func claim(account, prefix, holder string) action {
	switch holder {
	case "":
		return createAccount
	case account, unixName(prefix, account):
		return adoptAccount
	default:
		return refuseAccount
	}
}

// action is what claim decided.
type action int

const (
	createAccount action = iota
	adoptAccount
	refuseAccount
)

// String makes a failing test name the decision rather than an integer.
func (a action) String() string {
	switch a {
	case createAccount:
		return "create"
	case adoptAccount:
		return "adopt"
	case refuseAccount:
		return "refuse"
	default:
		return "unknown"
	}
}
