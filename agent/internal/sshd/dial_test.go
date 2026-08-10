package sshd

import (
	"strings"
	"testing"
)

// Reaching a published container port is the whole point of a local forward,
// and the port is not knowable in advance, so anything loopback is allowed.
func TestAllowDialReachesPublishedPorts(t *testing.T) {
	p := newPolicy()

	for _, port := range []uint32{80, 8080, 32768, 49153} {
		if ok, why := p.AllowDial(alice, "127.0.0.1", port); !ok {
			t.Errorf("alice was refused 127.0.0.1:%d: %s", port, why)
		}
	}
}

// The workspace must not become a way to reach the network it sits on.
func TestAllowDialRefusesNonLoopback(t *testing.T) {
	p := newPolicy()

	for _, host := range []string{"10.0.0.5", "example.org", "0.0.0.0", ""} {
		if ok, why := p.AllowDial(alice, host, 80); ok {
			t.Errorf("%q was allowed", host)
		} else if !strings.Contains(why, "loopback") {
			t.Errorf("the refusal for %q does not say why: %s", host, why)
		}
	}
}

// The one that matters, and the reason this function exists.
//
// With one daemon for everybody (ADR 0012) every account shares the agent's
// network namespace, so bob can reach 127.0.0.1:30000 and find alice's NFS
// export listening on it. The export answers AuthFlavorNull, so what he gets
// is read and write access to the directories on her machine.
//
// Binding her port was already refused; dialling it was not.
func TestAllowDialRefusesAnotherAccountsTunnel(t *testing.T) {
	p := newPolicy()

	if !p.Bind(alice, "127.0.0.1", 30000) {
		t.Fatal("alice could not take her own port, so this proves nothing")
	}

	ok, why := p.AllowDial(bob, "127.0.0.1", 30000)
	if ok {
		t.Fatal("bob reached alice's file server")
	}
	if !strings.Contains(why, "file server") {
		t.Errorf("the refusal does not say what he was reaching for: %s", why)
	}

	// Alice's own client dials her own port on reconnect, and must not be
	// caught by the rule protecting her.
	if ok, why := p.AllowDial(alice, "127.0.0.1", 30000); !ok {
		t.Errorf("alice was refused her own port: %s", why)
	}
}

// A port nobody holds is not protected, because there is nothing of ours on
// it. Refusing the whole reverse-tunnel range would refuse published container
// ports too: PortForUID counts up from 30000, and docker publishes from 32768.
func TestAllowDialLeavesUnheldPortsAlone(t *testing.T) {
	p := newPolicy()

	if ok, why := p.AllowDial(bob, "127.0.0.1", 30000); !ok {
		t.Errorf("an unheld port was refused: %s", why)
	}
}

// And a released port stops being protected, or an account that reconnects
// after somebody else's session ended would be refused a port that is now
// just a published one.
func TestAllowDialAfterRelease(t *testing.T) {
	p := newPolicy()

	p.Bind(alice, "127.0.0.1", 30000)
	p.Release(alice, "127.0.0.1", 30000)

	if ok, why := p.AllowDial(bob, "127.0.0.1", 30000); !ok {
		t.Errorf("a released port stayed protected: %s", why)
	}
}
