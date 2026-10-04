package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
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

// The private half is what the agent loads and the public half is what the
// record pins, so they have to be one key, in a format the agent parses.
func TestNewHostKeyHalvesMatch(t *testing.T) {
	hk, err := NewHostKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(hk.Private)
	if err != nil {
		t.Fatalf("the agent could not load the private half: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hk.Public))
	if err != nil {
		t.Fatalf("the public half %q does not parse: %v", hk.Public, err)
	}
	if ssh.FingerprintSHA256(pub) != ssh.FingerprintSHA256(signer.PublicKey()) {
		t.Errorf("public half %s is not the private half's %s",
			ssh.FingerprintSHA256(pub), ssh.FingerprintSHA256(signer.PublicKey()))
	}
	if again, _ := NewHostKey(); again.Public == hk.Public {
		t.Error("two host keys came out the same")
	}
}

// A rebuilt machine at the address an old one used: the key is all that is
// checked.
func TestPinnedHostKeyIgnoresTheAddress(t *testing.T) {
	hk, err := NewHostKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(hk.Private)
	if err != nil {
		t.Fatal(err)
	}
	refused := func(offered ssh.PublicKey) error {
		return fmt.Errorf("refused %s", ssh.FingerprintSHA256(offered))
	}
	check, err := PinnedHostKey(hk.Public, refused)
	if err != nil {
		t.Fatal(err)
	}

	for _, addr := range []string{"172.18.12.79:2222", "172.24.110.158:2200"} {
		remote, _ := net.ResolveTCPAddr("tcp", addr)
		if err := check(addr, remote, signer.PublicKey()); err != nil {
			t.Errorf("the pinned key at %s was refused: %v", addr, err)
		}
	}

	other := testHostKey(t)
	err = check("172.18.12.79:2222", nil, other)
	if err == nil || !strings.Contains(err.Error(), ssh.FingerprintSHA256(other)) {
		t.Errorf("another key = %v, want mismatch's error naming it", err)
	}
}

func TestPinnedHostKeyRefusesAnUnreadableRecord(t *testing.T) {
	if _, err := PinnedHostKey("not a key", func(ssh.PublicKey) error { return nil }); err == nil {
		t.Error("an unreadable record was accepted, which pins nothing")
	}
}
