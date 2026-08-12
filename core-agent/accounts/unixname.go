package accounts

// DefaultPrefix is what a unix account name starts with.
//
// The account name is ours -- it comes from the key file, it is what a client
// logs in as, it owns the reverse-tunnel port. The unix user behind it is not:
// on a VM (ADR 0025) it sits in the machine's own passwd file next to its
// service accounts, and `alice.pub` quietly taking the name `alice` there is a
// claim on somebody else's namespace.
//
// The same default everywhere, container included. Two defaults would be two
// behaviours to keep in step, and the container has nothing to gain from the
// shorter name.
const DefaultPrefix = "rd-"

// unixName is the unix user behind an account.
//
// Truncated so the whole thing fits, prefix included. SanitizeName already
// caps the account at maxNameLength, so without this the prefix would push it
// past what Linux accepts. It moves the cliff where two long names collapse
// into one from 30 characters to 30 minus the prefix; beyond that they were
// already colliding, and the uid lookup in Ensure is what catches it either
// way.
func unixName(prefix, account string) string {
	room := maxNameLength - len(prefix)
	if room < 1 {
		// A prefix longer than a whole name is a configuration error, not a
		// case to be clever about. Keep the account and let useradd complain
		// about the length with the name in hand.
		return account
	}
	if len(account) > room {
		account = account[:room]
	}
	return prefix + account
}

// claim says what to do about the unix user, if any, already holding the uid
// this account is mapped to.
//
// The UID is the identity, not the name. It is what the uidmap binds, what the
// reverse-tunnel port is derived from (ADR 0011), and what owns the files; the
// unix name is a label on top of it. So an existing workspace, whose accounts
// were created before the prefix and are called `alice`, is adopted exactly as
// it stands -- no rename, no home directory moved, no port changed.
//
// A stranger at that uid is the case this exists for. Adopting one would hand
// an enrolled key somebody else's files, and on a machine that does other work
// there is no reason to assume uid 10001 is free.
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
