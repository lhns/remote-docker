package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// newKnownHosts is a checker over a fresh known_hosts, and its path.
func newKnownHosts(t *testing.T) (*KnownHosts, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	kh, err := NewKnownHosts(path)
	if err != nil {
		t.Fatalf("NewKnownHosts: %v", err)
	}
	return kh, path
}

func TestKnownHostsTrustsOnFirstUse(t *testing.T) {
	kh, path := newKnownHosts(t)

	key := testHostKey(t)
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}

	if err := kh.Callback()("workspace.example:2222", addr, key); err != nil {
		t.Fatalf("first contact was refused: %v", err)
	}

	// Recorded, so the second connection is checked rather than trusted.
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) == 0 {
		t.Fatal("first contact recorded nothing; every later connection would be trust-on-first-use again")
	}

	if err := kh.Callback()("workspace.example:2222", addr, key); err != nil {
		t.Errorf("the recorded key was not accepted on reconnect: %v", err)
	}
}

// The case that matters. A changed host key is either a rebuilt workspace or
// an interception, and there is no interactive user on the far side of an
// automated tunnel to make that judgement, so it is refused, not prompted.
func TestKnownHostsRefusesAChangedKey(t *testing.T) {
	kh, path := newKnownHosts(t)

	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}
	original := testHostKey(t)
	if err := kh.Callback()("workspace.example:2222", addr, original); err != nil {
		t.Fatalf("first contact: %v", err)
	}

	imposter := testHostKey(t)
	err := kh.Callback()("workspace.example:2222", addr, imposter)
	if err == nil {
		t.Fatal("a changed host key was accepted")
	}
	// The message has to tell the user what to do about it, or they will
	// delete the whole file to make it go away.
	for _, want := range []string{"CHANGED", path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestKnownHostsSeparatesHosts(t *testing.T) {
	kh, _ := newKnownHosts(t)

	addrA := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}
	addrB := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 2222}
	keyA, keyB := testHostKey(t), testHostKey(t)

	if err := kh.Callback()("a.example:2222", addrA, keyA); err != nil {
		t.Fatalf("host a: %v", err)
	}
	if err := kh.Callback()("b.example:2222", addrB, keyB); err != nil {
		t.Fatalf("host b: %v", err)
	}

	// Trusting a for b would make the file worthless.
	if err := kh.Callback()("b.example:2222", addrB, keyA); err == nil {
		t.Error("host b accepted host a's key")
	}
}

func TestNewKnownHostsCreatesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "known_hosts")
	if _, err := NewKnownHosts(path); err != nil {
		t.Fatalf("NewKnownHosts: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("known_hosts was not created: %v", err)
	}
}
