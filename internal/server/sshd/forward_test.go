package sshd

import (
	"strings"
	"testing"

	"github.com/lhns/remote-docker/pkg/workspace"
)

type account struct {
	name string
	uid  int
}

func (a account) Name() string { return a.name }
func (a account) UID() int     { return a.uid }

func newPolicy() *ForwardPolicy {
	return NewForwardPolicy(workspace.DefaultMapping())
}

var (
	alice = account{"alice", 10000} // port 30000
	bob   = account{"bob", 10001}   // port 30001
)

func TestAllowOwnPort(t *testing.T) {
	p := newPolicy()

	if ok, why := p.Allow(alice, "127.0.0.1", 30000); !ok {
		t.Errorf("alice was refused her own port: %s", why)
	}
	if ok, why := p.Allow(bob, "127.0.0.1", 30001); !ok {
		t.Errorf("bob was refused his own port: %s", why)
	}
}

// The attack this whole design exists to prevent: binding another account's
// port before they connect, so that when they do, they mount a filesystem of
// the attacker's choosing.
//
// Under sshd this was a permitlisten string generated into every key's
// authorized_keys. Here it is a comparison, and this is the test that says so.
func TestRefusesAnotherAccountsPort(t *testing.T) {
	p := newPolicy()

	ok, why := p.Allow(alice, "127.0.0.1", 30001)
	if ok {
		t.Fatal("alice was allowed to bind bob's port")
	}
	// The message has to name the port she may have, or the refusal is a
	// mystery to somebody with a legitimate misconfiguration.
	if !strings.Contains(why, "30000") {
		t.Errorf("refusal %q does not say which port alice may bind", why)
	}

	if ok, _ := p.Allow(bob, "127.0.0.1", 30000); ok {
		t.Error("bob was allowed to bind alice's port")
	}
}

// The NFS export is unauthenticated: anything that can reach the port can read
// and write the client's files. It must never leave the container.
func TestRefusesNonLoopbackAddresses(t *testing.T) {
	p := newPolicy()

	for _, host := range []string{
		"",         // all interfaces, in the SSH protocol
		"*",        // likewise
		"0.0.0.0",  // explicitly all
		"::",       // all, v6
		"10.0.0.5", // a real interface
		"example.com",
	} {
		if ok, _ := p.Allow(alice, host, 30000); ok {
			t.Errorf("a forward was allowed on %q, which is not loopback", host)
		}
	}

	for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
		if ok, why := p.Allow(alice, host, 30000); !ok {
			t.Errorf("loopback address %q was refused: %s", host, why)
		}
	}
}

// A second session must not displace the first's tunnel and silently take over
// its mounts.
func TestOnePortHolderAtATime(t *testing.T) {
	p := newPolicy()

	if !p.Bind(alice, "127.0.0.1", 30000) {
		t.Fatal("alice could not bind her own port")
	}
	if holder, ok := p.Holder("127.0.0.1", 30000); !ok || holder != "alice" {
		t.Errorf("holder = %q %v, want alice", holder, ok)
	}

	// Alice reconnecting is fine -- same account, and refusing would strand
	// her after a dropped connection.
	if !p.Bind(alice, "127.0.0.1", 30000) {
		t.Error("alice could not rebind her own port")
	}

	// Somebody else is not.
	if p.Bind(bob, "127.0.0.1", 30000) {
		t.Error("bob took a port alice holds")
	}
	if ok, why := p.Allow(bob, "127.0.0.1", 30000); ok {
		t.Error("Allow permitted what Bind refuses")
	} else if why == "" {
		t.Error("a refusal came with no explanation")
	}
}

func TestReleaseFreesThePort(t *testing.T) {
	p := newPolicy()
	p.Bind(alice, "127.0.0.1", 30000)
	p.Release(alice, "127.0.0.1", 30000)

	if _, ok := p.Holder("127.0.0.1", 30000); ok {
		t.Error("the port was still held after release")
	}
	if !p.Bind(alice, "127.0.0.1", 30000) {
		t.Error("the port could not be rebound after release")
	}
}

// Only the holder may release, so a session ending cannot free a port another
// session has since taken.
func TestReleaseOnlyByTheHolder(t *testing.T) {
	p := newPolicy()
	p.Bind(alice, "127.0.0.1", 30000)

	p.Release(bob, "127.0.0.1", 30000)
	if holder, ok := p.Holder("127.0.0.1", 30000); !ok || holder != "alice" {
		t.Error("bob released a port held by alice")
	}
}

// An account outside the workspace range has no port at all, rather than
// mapping to something below the base that may belong to the system.
func TestAccountWithNoPort(t *testing.T) {
	p := newPolicy()
	root := account{"root", 0}

	if ok, why := p.Allow(root, "127.0.0.1", 30000); ok {
		t.Error("an account outside the workspace uid range was allowed a port")
	} else if !strings.Contains(why, "no reverse-tunnel port") {
		t.Errorf("refusal %q does not explain why", why)
	}
}

// The policy is consulted from every connection, so it must hold up
// concurrently.
func TestPolicyIsSafeUnderConcurrency(t *testing.T) {
	p := newPolicy()

	done := make(chan struct{})
	for range 20 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 100 {
				p.Allow(alice, "127.0.0.1", 30000)
				p.Bind(alice, "127.0.0.1", 30000)
				p.Holder("127.0.0.1", 30000)
				p.Release(alice, "127.0.0.1", 30000)
			}
		}()
	}
	for range 20 {
		<-done
	}
}
