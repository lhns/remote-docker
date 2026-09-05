package workspace

import (
	"crypto/sha256"
	"encoding/hex"
)

// clientIDLen is how many hex characters identify a client machine. 8 is 32
// bits, which is plenty to tell one person's machines apart and short enough
// to leave a volume name readable.
const clientIDLen = 8

// ClientID identifies the MACHINE a session runs from, as distinct from the
// account it runs as (ADR 0029).
//
// It is the digest of the public key the workspace has ALREADY AUTHENTICATED,
// never an id the client asserts: an asserted one could claim another
// machine's port or volumes. The argument is the key's wire encoding
// (ssh.PublicKey.Marshal) rather than an ssh.PublicKey, so that this module
// keeps depending on nothing (ADR 0021); both sides hash the same bytes.
func ClientID(publicKeyWire []byte) string {
	sum := sha256.Sum256(publicKeyWire)
	return hex.EncodeToString(sum[:])[:clientIDLen]
}
