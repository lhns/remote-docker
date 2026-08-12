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
// account it runs as.
//
// The account is the identity and the machine is the client. Both of somebody's
// machines share one account, one daemon and therefore one set of containers
// and images, which is the point: start something on one and watch it from the
// other. What they cannot share is files, because those are on one machine, so
// the export and the volumes backing it are per client.
//
// It is the digest of the public key the workspace has ALREADY AUTHENTICATED,
// which is what makes it the right identifier rather than merely a workable
// one:
//
//   - stable per machine by construction, because the key is created once per
//     machine and every later session reuses it, and it changes exactly when
//     that machine's enrolment changes;
//   - authenticated rather than asserted. An id the client sent would have to
//     be taken on trust, and a client could then claim another machine's port
//     or another machine's volumes;
//   - durable with no new state to keep, lose or migrate.
//
// The argument is the key's wire encoding (ssh.PublicKey.Marshal) rather than
// an ssh.PublicKey, so that this module keeps depending on nothing (ADR 0021).
// Both sides hash the same bytes.
//
// One consequence, which is handled where it surfaces rather than here: the
// same key copied to two machines makes them one client. That is a
// configuration directory somebody has synced, and it collides exactly as two
// sessions on one machine would.
func ClientID(publicKeyWire []byte) string {
	sum := sha256.Sum256(publicKeyWire)
	return hex.EncodeToString(sum[:])[:clientIDLen]
}
