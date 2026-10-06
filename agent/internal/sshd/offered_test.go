package sshd

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core/workspace"
)

// probeSigner runs probe before it signs. A client signs only once the server
// has answered its query for the key, so probe sees the server between
// accepting the offer and seeing a signature.
type probeSigner struct {
	ssh.Signer
	probe func()
}

func (p probeSigner) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	p.probe()
	return p.Signer.Sign(rand, data)
}

// A key the client only offers is nobody's: it is not connected as the
// account and warms nothing until it signs.
func TestAnOfferedKeyIsNotALogin(t *testing.T) {
	keysDir := t.TempDir()
	key := newSigner(t)
	if err := os.WriteFile(filepath.Join(keysDir, "alice.pub"), ssh.MarshalAuthorizedKey(key.PublicKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	store := accounts.New([]string{keysDir}, "", t.TempDir(), workspace.DefaultMapping(), fakeProvisioner{}, nil)
	if err := store.Sync(); err != nil {
		t.Fatal(err)
	}
	targets := twoAccounts()
	s, err := New(Config{Accounts: store, Mapping: workspace.DefaultMapping(), Daemons: targets, HostKeys: []ssh.Signer{newSigner(t)}})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.ServeListener(l) }()
	t.Cleanup(func() { _ = s.Close(); _ = l.Close() })

	warmed := func() int {
		targets.mu.Lock()
		defer targets.mu.Unlock()
		return len(targets.warmed)
	}
	probed := false
	signer := probeSigner{Signer: key, probe: func() {
		probed = true
		if s.Connections()["alice"] > 0 {
			t.Error("an offered key that has not signed is connected as alice")
		}
		if n := warmed(); n != 0 {
			t.Errorf("an offered key that has not signed warmed %d daemon(s)", n)
		}
	}}
	c, err := ssh.Dial("tcp", l.Addr().String(), &ssh.ClientConfig{
		User: "alice", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if !probed {
		t.Fatal("the client never signed")
	}
	if n := s.Connections()["alice"]; n != 1 || warmed() != 1 {
		t.Errorf("after signing: %d connection(s) as alice, warmed %d; want 1, 1", n, warmed())
	}
}
