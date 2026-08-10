package accounts

// The type lives here, untagged, and only its Ensure method is
// platform-specific.
//
// It was declared twice (once per build tag) field for field. Both copies
// were edited on the same day, which is exactly how a pair like that comes to
// disagree: a field added to the Linux one and forgotten on the stub compiles
// everywhere except the machine nobody builds on.

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
	// so changing Groups alone would apply to new accounts only, and on an
	// upgraded workspace every account already exists. With a daemon per
	// account (ADR 0019) that would leave every one of them still in the
	// `docker` group, holding a socket that reaches the PARENT daemon, which
	// can see and control every account's dind. The separation would be a
	// claim rather than a fact, on exactly the workspaces that had users.
	Revoke []string
}
