package tunnelclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// A peer that accepts and never speaks must not hold Dial forever. The timeout
// and the context covered the TCP connect only, so a wedged agent or a proxy
// that answers nothing left the session connecting with nothing on screen.
func TestDialGivesUpOnASilentPeer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	host, port := hostPort(t, l.Addr().String())

	done := make(chan error, 1)
	go func() {
		_, err := Dial(t.Context(), Config{
			Host: host, Port: port, User: "tester",
			Signer: signer, HostKey: ssh.InsecureIgnoreHostKey(),
			Timeout: 200 * time.Millisecond,
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Dial succeeded against a peer that never spoke")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Dial with a 200ms timeout was still waiting on the SSH handshake after 5s")
	}
}
