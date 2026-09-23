package accounts

// UnixProvisioner creates real unix accounts, through useradd: the shadow
// tooling does the locking between passwd, group and gshadow that editing the
// files by hand gets wrong.
//
// Untagged, with only Ensure per platform, so a field cannot exist on one
// platform and not the other.
type UnixProvisioner struct {
	// Groups the account joins, and is reconciled INTO if it already exists.
	// Empty means no --groups; the caller states them in both modes.
	Groups []string

	// Prefix goes in front of the unix user name. Empty means DefaultPrefix;
	// see unixname.go.
	Prefix string

	// Revoke names groups an existing account must NOT be in. Groups alone
	// reaches new accounts only, since Ensure returns early for one that
	// exists, so on an upgraded workspace every account would keep `docker`
	// and with it the PARENT daemon holding every account's dind (ADR 0019).
	Revoke []string
}
