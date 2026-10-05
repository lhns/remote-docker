package workspace

import "testing"

// Fixed vectors, because both binaries derive this id and a change to the
// derivation renames every ephemeral run's volumes.
func TestEphemeralClientID(t *testing.T) {
	const (
		runA = "00112233445566778899aabbccddeeff"
		runB = "ffeeddccbbaa99887766554433221100"
	)
	alice := []byte("ssh-ed25519 AAAA...alice-ci")
	bob := []byte("ssh-ed25519 AAAA...bob-ci")

	for _, tc := range []struct {
		key  []byte
		run  string
		want string
	}{
		{alice, runA, "42772e76"},
		{alice, runB, "da32e003"},
		{bob, runA, "2d5c0059"},
	} {
		if got := EphemeralClientID(tc.key, tc.run); got != tc.want {
			t.Errorf("EphemeralClientID(%s, %s) = %q, want %q", tc.key, tc.run, got, tc.want)
		}
	}
}

func TestRunIDs(t *testing.T) {
	a, b := NewRunID(), NewRunID()
	if a == b {
		t.Error("two runs minted one id")
	}
	if !ValidRunID(a) {
		t.Errorf("a minted run id %q is not valid", a)
	}
	for _, bad := range []string{"", "00112233", a + "0", "00112233445566778899AABBCCDDEEFF", "../2233445566778899aabbccddeeff"} {
		if ValidRunID(bad) {
			t.Errorf("ValidRunID(%q) = true", bad)
		}
	}
}
