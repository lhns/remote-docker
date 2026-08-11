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
// its mounts. One listener can hold a port, so this is true of a second session
// of the SAME account too.
func TestOnePortHolderAtATime(t *testing.T) {
	p := newPolicy()

	if _, ok := p.Bind(alice, "127.0.0.1", 30000); !ok {
		t.Fatal("alice could not bind her own port")
	}
	if holder, ok := p.Holder("127.0.0.1", 30000); !ok || holder != "alice" {
		t.Errorf("holder = %q %v, want alice", holder, ok)
	}

	// Alice on a second machine is refused while her first still holds it.
	// Permitting this is what let a failed bind speak for a live one.
	if _, ok := p.Bind(alice, "127.0.0.1", 30000); ok {
		t.Error("a second session of the same account took a held port")
	}

	// And somebody else, for the reason the rule exists.
	if _, ok := p.Bind(bob, "127.0.0.1", 30000); ok {
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
	token, _ := p.Bind(alice, "127.0.0.1", 30000)
	p.Release(token, "127.0.0.1", 30000)

	if _, ok := p.Holder("127.0.0.1", 30000); ok {
		t.Error("the port was still held after release")
	}
	if _, ok := p.Bind(alice, "127.0.0.1", 30000); !ok {
		t.Error("the port could not be rebound after release")
	}
}

// Only the session holding a port may release it, and an account name is not
// a session.
//
// This is the failure it exists for, and it was reached by an ordinary action:
// opening the client on a second machine. The second session's bind fails, its
// failure path releases, and releasing BY NAME deleted the first machine's live
// reservation. AllowDial then reports the port as free, and on a shared daemon
// (ADR 0012) any other account may dial an NFS export that authenticates
// nobody.
func TestAFailedBindDoesNotReleaseTheLiveHolder(t *testing.T) {
	p := newPolicy()
	held, _ := p.Bind(alice, "127.0.0.1", 30000)

	// Alice's second machine: refused, and its failure path releases what it
	// believes it took.
	second, ok := p.Bind(alice, "127.0.0.1", 30000)
	if ok {
		t.Fatal("the second bind was not refused")
	}
	p.Release(second, "127.0.0.1", 30000)

	if holder, ok := p.Holder("127.0.0.1", 30000); !ok || holder != "alice" {
		t.Fatal("the failed second session released the first session's port")
	}
	if ok, _ := p.AllowDial(bob, "127.0.0.1", 30000); ok {
		t.Error("another account was allowed to dial alice's file server")
	}

	// And the real holder can still give it up.
	p.Release(held, "127.0.0.1", 30000)
	if _, ok := p.Holder("127.0.0.1", 30000); ok {
		t.Error("the holder could not release its own port")
	}
}

// A connection ending late must not release the reservation whoever came after
// it now holds.
func TestAStaleTokenReleasesNothing(t *testing.T) {
	p := newPolicy()
	first, _ := p.Bind(alice, "127.0.0.1", 30000)
	p.Release(first, "127.0.0.1", 30000)

	if _, ok := p.Bind(alice, "127.0.0.1", 30000); !ok {
		t.Fatal("the port could not be rebound")
	}
	p.Release(first, "127.0.0.1", 30000)

	if _, ok := p.Holder("127.0.0.1", 30000); !ok {
		t.Error("a token from an earlier reservation released the current one")
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
				token, _ := p.Bind(alice, "127.0.0.1", 30000)
				p.Holder("127.0.0.1", 30000)
				p.Release(token, "127.0.0.1", 30000)
			}
		}()
	}
	for range 20 {
		<-done
	}
}

// A reservation that is never used has to be given back.
//
// Granting the port and opening the listener are two steps, and the second can
// fail, because the account's daemon may not be up yet, and with one per account
// the listener is bound inside it. When it did, the reservation stayed: the
// account's one reverse-tunnel port was held by a forward that did not exist,
// and every retry was refused as "another session for this account may still be
// open", blaming a second session for the first one's failure.
//
// This asserts the policy supports that: releasing without waiting for the
// connection to end, which is what the failure path in forward_tcpip.go does.
func TestAReservationCanBeGivenBackImmediately(t *testing.T) {
	p := newPolicy()

	token, ok := p.Bind(alice, "127.0.0.1", 30000)
	if !ok {
		t.Fatal("the first bind was refused")
	}
	// The listener failed; hand the port straight back.
	p.Release(token, "127.0.0.1", 30000)

	if _, ok := p.Bind(alice, "127.0.0.1", 30000); !ok {
		t.Error("the port is still held after being released; a retry would be refused")
	}
}
