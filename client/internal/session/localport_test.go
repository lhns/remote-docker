package session

import (
	"testing"

	"github.com/lhns/remote-docker/client/internal/ports"
	"github.com/lhns/remote-docker/client/internal/rewrite"
)

// container is one the daemon reports, labelled as this machine created it.
func container(client, label string, published ...ports.Published) ports.Container {
	return ports.Container{
		ID:     "c1",
		Name:   "web",
		Ports:  published,
		Labels: map[string]string{rewrite.ClientLabel: client, rewrite.PortsLabel: label},
	}
}

func tcp(public, private int) ports.Published {
	return ports.Published{PublicPort: public, PrivatePort: private, Type: "tcp"}
}

func TestLocalPortIsTheOneThisMachineAskedFor(t *testing.T) {
	c := container("me", "80/tcp=8080", tcp(32768, 80))

	if got := localPortFor(c, tcp(32768, 80), "me"); got != 8080 {
		t.Errorf("localPortFor = %d, want 8080", got)
	}
}

// One container port published twice: two assigned ports, two requested
// numbers, matched by counting. Any pairing is correct, so what is asserted is
// that both numbers are used and neither is used twice.
func TestBothPublicationsOfOnePortGetTheirNumber(t *testing.T) {
	c := container("me", "80/tcp=8080;9090", tcp(32769, 80), tcp(32768, 80))

	first := localPortFor(c, tcp(32768, 80), "me")
	second := localPortFor(c, tcp(32769, 80), "me")

	if first == second {
		t.Fatalf("both publications got %d, so one of them has no listener", first)
	}
	for _, got := range []int{first, second} {
		if got != 8080 && got != 9090 {
			t.Errorf("a publication got %d, which nobody asked for", got)
		}
	}
}

// Published more often than it was asked for, which is `-p 8080:80 -p 80`. The
// extra keeps whatever the daemon gave it.
func TestAnExtraPublicationKeepsThePublishedPort(t *testing.T) {
	c := container("me", "80/tcp=8080", tcp(32768, 80), tcp(32769, 80))

	if got := localPortFor(c, tcp(32769, 80), "me"); got != 0 {
		t.Errorf("localPortFor = %d, want the published port", got)
	}
}

// Another machine of this account asked for that number, not this one. Its
// container is forwarded where the daemon published it (ADR 0029).
func TestAnotherMachinesContainerKeepsThePublishedPort(t *testing.T) {
	c := container("them", "80/tcp=8080", tcp(32768, 80))

	if got := localPortFor(c, tcp(32768, 80), "me"); got != 0 {
		t.Errorf("localPortFor = %d, want the published port", got)
	}
}

// Created before this existed, or by something that is not us.
func TestAnUnlabelledContainerKeepsThePublishedPort(t *testing.T) {
	c := ports.Container{ID: "c1", Ports: []ports.Published{tcp(8080, 80)}}

	if got := localPortFor(c, tcp(8080, 80), "me"); got != 0 {
		t.Errorf("localPortFor = %d, want the published port", got)
	}
}
