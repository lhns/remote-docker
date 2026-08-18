package workspace

import "testing"

func TestRequestedPortsRoundTrip(t *testing.T) {
	in := RequestedPorts{
		ContainerPort(80, "tcp"):  8080,
		ContainerPort(443, "TCP"): 8443,
	}

	// Sorted, so the same command twice produces the same label.
	if got, want := in.String(), "443/tcp=8443,80/tcp=8080"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}

	back := ParseRequestedPorts(in.String())
	if len(back) != 2 || back["80/tcp"] != 8080 || back["443/tcp"] != 8443 {
		t.Fatalf("ParseRequestedPorts = %+v", back)
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

// One unreadable entry costs that entry. This is read while deciding which
// local port to open, and failing the whole label would cost a container every
// forward it has because of one bad field.
func TestParseRequestedPortsSkipsWhatItCannotRead(t *testing.T) {
	got := ParseRequestedPorts("80/tcp=8080,nonsense,443/tcp=,8/tcp=notanumber,9/tcp=0,10/tcp=99999")
	if len(got) != 1 || got["80/tcp"] != 8080 {
		t.Errorf("ParseRequestedPorts = %+v, want only the readable entry", got)
	}
}
