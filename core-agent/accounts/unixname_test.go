package accounts

// The unix user behind an account, and who is allowed to be at its uid.
//
// Both are pure, which is deliberate: Ensure itself needs root to do anything
// (which is why the provisioner is an interface at all, ADR 0010), so the
// decisions come out where a test on any machine can reach them.

import (
	"strings"
	"testing"
)

func TestUnixName(t *testing.T) {
	long := strings.Repeat("a", maxNameLength)

	for _, tc := range []struct {
		name    string
		prefix  string
		account string
		want    string
	}{
		{"the ordinary case", "rd-", "alice", "rd-alice"},
		{"no prefix configured", "", "alice", "alice"},

		// SanitizeName already caps the account at maxNameLength, so the
		// prefix has to come out of that budget rather than be added to it.
		{"a name at the limit loses its tail", "rd-", long, "rd-" + strings.Repeat("a", maxNameLength-3)},

		// A prefix longer than a whole name is a configuration error. Keeping
		// the account lets useradd complain with the name in hand, which beats
		// deriving something clever from nothing.
		{"an absurd prefix is left to useradd", strings.Repeat("p", maxNameLength+1), "alice", "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := unixName(tc.prefix, tc.account)
			if got != tc.want {
				t.Errorf("unixName(%q, %q) = %q, want %q", tc.prefix, tc.account, got, tc.want)
			}
			if tc.prefix != "" && len(got) > maxNameLength && len(tc.prefix) < maxNameLength {
				t.Errorf("unixName(%q, %q) = %q, which is %d characters", tc.prefix, tc.account, got, len(got))
			}
		})
	}
}

func TestClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		holder string
		want   action
	}{
		{"nobody holds the uid", "", createAccount},

		// A workspace provisioned before the prefix existed. Adopted exactly
		// as it stands: no rename, no home directory moved, and above all no
		// new uid, which would change the tunnel port and orphan the ownership
		// of everything the account has written.
		{"the bare name, from before the prefix", "alice", adoptAccount},

		{"the prefixed name, as we create them now", "rd-alice", adoptAccount},

		// The row the guard exists for. On a machine that does other work
		// there is no reason uid 10001 is free, and adopting whoever holds it
		// hands an enrolled key somebody else's files.
		{"a stranger", "postgres", refuseAccount},
		{"another workspace account", "rd-bob", refuseAccount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := claim("alice", "rd-", tc.holder); got != tc.want {
				t.Errorf("claim(alice, rd-, %q) = %v, want %v", tc.holder, got, tc.want)
			}
		})
	}
}

// With no prefix configured the bare name is both answers at once, which must
// not stop it being adopted.
func TestClaimWithoutAPrefix(t *testing.T) {
	if got := claim("alice", "", "alice"); got != adoptAccount {
		t.Errorf("claim(alice, \"\", alice) = %v, want adopt", got)
	}
	if got := claim("alice", "", "postgres"); got != refuseAccount {
		t.Errorf("claim(alice, \"\", postgres) = %v, want refuse", got)
	}
}
