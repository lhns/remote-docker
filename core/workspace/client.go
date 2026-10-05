package workspace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
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

// EphemeralClientID identifies one client RUN of an ephemeral account (ADR
// 0050): ClientID's shape, so volume names and labels keep their format, but
// derived from the authenticated key AND the run id. The prefix separates it
// from ClientID's hash input.
func EphemeralClientID(publicKeyWire []byte, runID string) string {
	h := sha256.New()
	h.Write([]byte(RunRequest + "\x00"))
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(publicKeyWire))))
	h.Write(publicKeyWire)
	h.Write([]byte(runID))
	return hex.EncodeToString(h.Sum(nil))[:clientIDLen]
}

// RunRequest is the SSH global request a client sends right after the
// handshake, its payload the run id (ADR 0050). An agent that predates it
// replies false with no payload; one that knows it puts the reason for a
// refusal in the payload, and that is how the client tells the two apart.
const RunRequest = "remote-docker-run"

// runIDBytes is a run id's randomness: it is a bearer secret held by one
// process, never derived from anything.
const runIDBytes = 16

// NewRunID mints the id of one client process.
func NewRunID() string {
	b := make([]byte, runIDBytes)
	_, _ = rand.Read(b) // never fails since Go 1.24
	return hex.EncodeToString(b)
}

// ValidRunID reports whether s has the shape NewRunID mints.
func ValidRunID(s string) bool {
	return len(s) == 2*runIDBytes && isLowerHex(s)
}

// ValidClientID reports whether s has the shape ClientID and
// EphemeralClientID produce.
func ValidClientID(s string) bool {
	return len(s) == clientIDLen && isLowerHex(s)
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
