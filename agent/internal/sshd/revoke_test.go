package sshd

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core/workspace"
)

// Revocation disconnects, over real SSH connections: the sweep runs inside
// Sync, so each case revokes, syncs, and looks at the connection.

type revokeWorkspace struct {
	addr     string
	store    *accounts.Store
	keysDir  string
	enrolled string
}

func startRevokeWorkspace(t *testing.T) *revokeWorkspace {
	t.Helper()
	root := t.TempDir()
	w := &revokeWorkspace{keysDir: filepath.Join(root, "keys"), enrolled: filepath.Join(root, "enrolled")}
	for _, dir := range []string{w.keysDir, w.enrolled} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w.store = accounts.New([]string{w.keysDir}, w.enrolled, t.TempDir(), workspace.DefaultMapping(), fakeProvisioner{}, nil)

	s, err := New(Config{
		Accounts: w.store,
		Mapping:  workspace.DefaultMapping(),
		Daemons:  daemons.Shared(""),
		HostKeys: []ssh.Signer{newSigner(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.ServeListener(l) }()
	t.Cleanup(func() { _ = s.Close(); _ = l.Close() })
	w.addr = l.Addr().String()
	return w
}

// enrol writes keys as the account's operator file and syncs.
func (w *revokeWorkspace) enrol(t *testing.T, account string, keys ...ssh.Signer) {
	t.Helper()
	var lines []string
	for _, k := range keys {
		lines = append(lines, string(ssh.MarshalAuthorizedKey(k.PublicKey())))
	}
	if err := os.WriteFile(filepath.Join(w.keysDir, account+".pub"), []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	w.sync(t)
}

func (w *revokeWorkspace) sync(t *testing.T) {
	t.Helper()
	if err := w.store.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

func (w *revokeWorkspace) dial(t *testing.T, user string, key ssh.Signer) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", w.addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// open reports whether the workspace still answers on the connection.
func isOpen(c *ssh.Client) bool {
	_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
	return err == nil
}

// assertClosed waits for the workspace to end the connection. Sync has already
// closed it; the client only has to notice.
func assertClosed(t *testing.T, c *ssh.Client, why string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the connection is still open after %s", why)
	}
}

func assertOpen(t *testing.T, c *ssh.Client, why string) {
	t.Helper()
	if !isOpen(c) {
		t.Fatalf("the connection was closed after %s", why)
	}
}

func TestRevokingAKeyFileClosesItsConnections(t *testing.T) {
	w := startRevokeWorkspace(t)
	alice, bob := newSigner(t), newSigner(t)
	w.enrol(t, "alice", alice)
	w.enrol(t, "bob", bob)

	a := w.dial(t, "alice", alice)
	b := w.dial(t, "bob", bob)

	if err := os.Remove(filepath.Join(w.keysDir, "alice.pub")); err != nil {
		t.Fatal(err)
	}
	w.sync(t)

	assertClosed(t, a, "alice's key file was deleted")
	assertOpen(t, b, "alice's key file was deleted")

	if _, err := ssh.Dial("tcp", w.addr, &ssh.ClientConfig{
		User: "alice", Auth: []ssh.AuthMethod{ssh.PublicKeys(alice)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}); err == nil {
		t.Error("alice reconnected with a revoked key")
	}
}

// An emptied file revokes on its second read, and so disconnects then.
func TestAnEmptiedKeyFileClosesOnTheSecondRead(t *testing.T) {
	w := startRevokeWorkspace(t)
	alice := newSigner(t)
	w.enrol(t, "alice", alice)
	a := w.dial(t, "alice", alice)

	if err := os.WriteFile(filepath.Join(w.keysDir, "alice.pub"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w.sync(t)
	assertOpen(t, a, "one read of an empty key file")

	w.sync(t)
	assertClosed(t, a, "two reads of an empty key file")
}

// Only the key that was removed loses its connection; an account that gains a
// key, or loses another one, keeps it.
func TestOnlyTheRevokedKeysConnectionCloses(t *testing.T) {
	w := startRevokeWorkspace(t)
	laptop, desktop := newSigner(t), newSigner(t)
	w.enrol(t, "alice", laptop)
	a := w.dial(t, "alice", laptop)

	w.enrol(t, "alice", laptop, desktop)
	assertOpen(t, a, "alice gained a key")

	d := w.dial(t, "alice", desktop)
	w.enrol(t, "alice", desktop)
	assertClosed(t, a, "the laptop's key was removed")
	assertOpen(t, d, "the laptop's key was removed")
}

// The writer API revokes through the same Sync, so it disconnects too.
func TestRemovingAnEnrolledKeyClosesItsConnection(t *testing.T) {
	w := startRevokeWorkspace(t)
	alice := newSigner(t)
	if err := w.store.CreateAccount("alice", alice.PublicKey(), "alice@laptop"); err != nil {
		t.Fatal(err)
	}
	a := w.dial(t, "alice", alice)

	removed, err := w.store.RemoveKey("alice", ssh.FingerprintSHA256(alice.PublicKey()))
	if err != nil || !removed {
		t.Fatalf("RemoveKey = %v, %v", removed, err)
	}
	assertClosed(t, a, "alice's enrolled key was removed")
}

// A connection that never authenticated is nobody's, and a sweep leaves it be.
func TestASweepIgnoresAConnectionStillAuthenticating(t *testing.T) {
	w := startRevokeWorkspace(t)
	raw, err := net.Dial("tcp", w.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()

	buf := make([]byte, 4)
	if _, err := raw.Read(buf); err != nil { // the server's banner: it is tracked
		t.Fatal(err)
	}
	w.sync(t)
	_ = raw.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := raw.Read(buf); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the sweep closed an unauthenticated connection: %v", err)
	}
}
