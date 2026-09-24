package session

import (
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// enrolmentHint names the account, key file and key an authentication failure
// needs to be fixed, and is empty for any other error. The transport decides
// none of this (ADR 0021). Matched on x/crypto's wording: a reword upstream
// loses only the hint.
func enrolmentHint(err error, user string, signer ssh.Signer) string {
	if err == nil || signer == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		return ""
	}
	return fmt.Sprintf(
		"\n  fix: enrol this key as authorized_keys.d/%s.pub; it is read within a minute\n  key: %s",
		user, ssh.FingerprintSHA256(signer.PublicKey()))
}
