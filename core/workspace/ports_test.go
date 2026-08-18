package workspace

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestRequestedPortsRoundTrip(t *testing.T) {
	in := RequestedPorts{}
	in.Add(ContainerPort(80, "tcp"), 8080)
	in.Add(ContainerPort(80, "tcp"), 9090)
	in.Add(ContainerPort(443, "TCP"), 8443)

	// Sorted throughout, so the same command twice produces the same label.
	if got, want := in.String(), "443/tcp=8443,80/tcp=8080;9090"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}

	back := ParseRequestedPorts(in.String())
	if !reflect.DeepEqual(back["80/tcp"], []int{8080, 9090}) || !reflect.DeepEqual(back["443/tcp"], []int{8443}) {
		t.Fatalf("ParseRequestedPorts = %+v", back)
	}
}

// One container port published twice is the case the list exists for. Which
// requested number lands in front of which assigned port does not matter, so
// the caller counts rather than looking anything up.
func TestRequestedPortsAtCounts(t *testing.T) {
	r := ParseRequestedPorts("80/tcp=8080;9090")

	if got := r.At("80/tcp", 0); got != 8080 {
		t.Errorf("At(0) = %d, want 8080", got)
	}
	if got := r.At("80/tcp", 1); got != 9090 {
		t.Errorf("At(1) = %d, want 9090", got)
	}
	// A container port published more often than it was asked for, which is
	// `-p 8080:80 -p 80`: the extra keeps whatever the daemon gave it.
	if got := r.At("80/tcp", 2); got != 0 {
		t.Errorf("At(2) = %d, want 0", got)
	}
	if got := r.At("53/udp", 0); got != 0 {
		t.Errorf("a container port nobody asked for answered %d", got)
	}
}

func TestRequestedPortsEmpty(t *testing.T) {
	if got := (RequestedPorts{}).String(); got != "" {
		t.Errorf("an empty map rendered %q", got)
	}
	if got := ParseRequestedPorts(""); got != nil {
		t.Errorf("an empty label parsed to %+v", got)
	}
}

// One unreadable element costs that element. This is read while deciding which
// local port to open, and failing the whole label would cost a container every
// forward it has because of one bad field.
func TestParseRequestedPortsSkipsWhatItCannotRead(t *testing.T) {
	got := ParseRequestedPorts("80/tcp=8080;nonsense;9090,nokey,443/tcp=,8/tcp=0,10/tcp=99999")

	if !reflect.DeepEqual(got["80/tcp"], []int{8080, 9090}) {
		t.Errorf("ParseRequestedPorts = %+v, want the readable elements of 80/tcp", got)
	}
	if len(got) != 1 {
		t.Errorf("unreadable entries produced %+v", got)
	}
}

// A label decides how many local sockets this machine opens, and on a shared
// daemon (ADR 0012) anybody enrolled can write one. The bound is what stops a
// container asking for thousands.
func TestParseRequestedPortsIsBounded(t *testing.T) {
	numbers := make([]string, 0, MaxRequestedPorts+100)
	for i := range MaxRequestedPorts + 100 {
		numbers = append(numbers, strconv.Itoa(1+i%MaxPort))
	}
	label := "80/tcp=" + strings.Join(numbers, ";")

	got := ParseRequestedPorts(label)
	if len(got["80/tcp"]) != MaxRequestedPorts {
		t.Errorf("a label asking for %d ports produced %d", len(numbers), len(got["80/tcp"]))
	}
}
