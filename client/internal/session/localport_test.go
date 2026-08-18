package session

import (
	"slices"
	"testing"

	"github.com/lhns/remote-docker/client/internal/ports"
	"github.com/lhns/remote-docker/client/internal/rewrite"
)

// container is one the daemon reports, labelled as a machine created it.
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

func TestTheLocalPortIsTheOneThisMachineAskedFor(t *testing.T) {
	c := container("me", "80/tcp=8080", tcp(32768, 80))

	if got := localPortsFor(c, tcp(32768, 80), "me"); !slices.Equal(got, []int{8080}) {
		t.Errorf("localPortsFor = %v, want [8080]", got)
	}
}

// One container port published twice is published ONCE on the workspace, so
// both numbers go in front of that one publication.
func TestBothNumbersFrontOnePublication(t *testing.T) {
	c := container("me", "80/tcp=8080;9090", tcp(32768, 80))

	if got := localPortsFor(c, tcp(32768, 80), "me"); !slices.Equal(got, []int{8080, 9090}) {
		t.Errorf("localPortsFor = %v, want both numbers", got)
	}
}

// Another machine of this account asked for that number, not this one. Its
// container is forwarded where the daemon published it (ADR 0029).
func TestAnotherMachinesContainerKeepsThePublishedPort(t *testing.T) {
	c := container("them", "80/tcp=8080", tcp(32768, 80))

	if got := localPortsFor(c, tcp(32768, 80), "me"); len(got) != 0 {
		t.Errorf("localPortsFor = %v, want nothing, so the published port is used", got)
	}
}

// Created before this existed, or by something that is not us.
func TestAnUnlabelledContainerKeepsThePublishedPort(t *testing.T) {
	c := ports.Container{ID: "c1", Ports: []ports.Published{tcp(8080, 80)}}

	if got := localPortsFor(c, tcp(8080, 80), "me"); len(got) != 0 {
		t.Errorf("localPortsFor = %v, want nothing", got)
	}
}

// A container port nobody asked about keeps whatever the daemon gave it, which
// is what a mix of asked-for and any-port publications produces.
func TestAPortNobodyAskedForKeepsThePublishedPort(t *testing.T) {
	c := container("me", "80/tcp=8080", tcp(32768, 80), tcp(32769, 443))

	if got := localPortsFor(c, tcp(32769, 443), "me"); len(got) != 0 {
		t.Errorf("localPortsFor = %v, want nothing", got)
	}
}
