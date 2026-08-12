package session

// What an authentication failure has to say, which is this project's business
// and not the transport's.
//
// core-client/tunnelclient takes a signer and a host key callback and decides
// nothing about either (ADR 0030). Enrolment is the other half of that: the
// workspace grants access by filename, out of band, so the fix for a refusal
// names a file the transport has never heard of. It belongs here, at the one
// place that dials.

import (
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// enrolmentHint is what to add to an authentication failure, and nothing at
// all for any other kind.
//
// The workspace enrols a key by filename, out of band, so "unable to
// authenticate" is nearly always a key that has not been put there yet or a
// file that has just been written and not yet read. Neither the account nor the
// file nor the key is in the error, and all three are needed to fix it.
//
// Matched on x/crypto's wording, which is not a promise it makes. A reworded
// upstream costs the hint and leaves the error, which is the right way round.
func enrolmentHint(err error, user string, signer ssh.Signer) string {
	if err == nil || signer == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		return ""
	}
	return fmt.Sprintf(
		"\n  fix: enrol this key as authorized_keys.d/%s.pub; it is read within a minute\n  key: %s",
		user, ssh.FingerprintSHA256(signer.PublicKey()))
}
