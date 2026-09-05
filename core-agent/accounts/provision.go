package accounts

// The type is untagged and only its Ensure method is per platform: a copy per
// build tag can gain a field on one side and compile everywhere except the
// machine nobody builds on.

// UnixProvisioner creates real unix accounts.
//
// It shells out to useradd rather than editing /etc/passwd directly. The
// shadow tooling handles the group file, the home directory skeleton and the
// locking between them; reimplementing that to avoid one dependency would be
// trading a well-understood tool for a novel source of corruption.
type UnixProvisioner struct {
	// Groups the account joins, and is reconciled INTO if it already exists.
	// Empty means no --groups; the caller states them in both modes.
	Groups []string

	// Prefix goes in front of the unix user name. Empty means DefaultPrefix;
	// see unixname.go for why the unix name is not the account name.
	Prefix string

	// Revoke names groups an existing account must NOT be in.
	//
	// Needed because Ensure returns early for an account that already exists,
	// so changing Groups alone would apply to new accounts only, and on an
	// upgraded workspace every account already exists. With a daemon per
	// account (ADR 0019) that would leave every one of them still in the
	// `docker` group, holding a socket that reaches the PARENT daemon, which
	// can see and control every account's dind. The separation would be a
	// claim rather than a fact, on exactly the workspaces that had users.
	Revoke []string
}
