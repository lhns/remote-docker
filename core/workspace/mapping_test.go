package workspace

import "testing"

func TestPortForUID(t *testing.T) {
	m := DefaultMapping()

	tests := []struct {
		name    string
		uid     int
		want    int
		wantErr bool
	}{
		{"first enrolled user", 10000, 30000, false},
		{"second enrolled user", 10001, 30001, false},
		{"far along the range", 12345, 32345, false},
		{"one below the base", 9999, 0, true},
		{"root", 0, 0, true},
		{"negative", -1, 0, true},
		{"overflows the port range", 10000 + (MaxPort - 30000) + 1, 0, true},
		{"last uid that still fits", 10000 + (MaxPort - 30000), MaxPort, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := m.PortForUID(tt.uid)
			if (err != nil) != tt.wantErr {
				t.Fatalf("PortForUID(%d) error = %v, wantErr %v", tt.uid, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("PortForUID(%d) = %d, want %d", tt.uid, got, tt.want)
			}
		})
	}
}

// The agent answers "whose port is this?" with UIDForPort and "which port is
// theirs?" with PortForUID. If those two ever disagree, a user is refused
// their own tunnel or handed someone else's, so assert they are inverses
// across the whole range rather than at a handful of points.
func TestUIDForPortIsInverseOfPortForUID(t *testing.T) {
	for _, m := range []Mapping{
		DefaultMapping(),
		{UIDBase: 1000, PortBase: 20000},
		{UIDBase: 0, PortBase: 1},
	} {
		for uid := m.UIDBase; uid < m.UIDBase+2000; uid++ {
			port, err := m.PortForUID(uid)
			if err != nil {
				t.Fatalf("%+v PortForUID(%d): %v", m, uid, err)
			}
			back, err := m.UIDForPort(port)
			if err != nil {
				t.Fatalf("%+v UIDForPort(%d): %v", m, port, err)
			}
			if back != uid {
				t.Fatalf("%+v round trip: uid %d -> port %d -> uid %d", m, uid, port, back)
			}
		}
	}
}

func TestUIDForPortRejectsOutOfRange(t *testing.T) {
	m := DefaultMapping()
	for _, port := range []int{0, 1, 1024, 29999, MaxPort + 1} {
		if _, err := m.UIDForPort(port); err == nil {
			t.Errorf("UIDForPort(%d) = nil error, want an error", port)
		}
	}
}

// OwnsPort is the entire port-ownership policy. The case that matters is the
// negative one: user A must not be able to bind user B's port, which is the
// cross-user NFS hijack the sshd-based server could not prevent without
// correctly generated permitlisten strings.
func TestOwnsPort(t *testing.T) {
	m := DefaultMapping()

	if !m.OwnsPort(10000, 30000) {
		t.Error("uid 10000 should own port 30000")
	}
	if m.OwnsPort(10000, 30001) {
		t.Error("uid 10000 must not own uid 10001's port")
	}
	if m.OwnsPort(10001, 30000) {
		t.Error("uid 10001 must not own uid 10000's port")
	}
	if m.OwnsPort(10000, 22) {
		t.Error("uid 10000 must not own a port outside the workspace range")
	}
	if m.OwnsPort(0, 30000) {
		t.Error("a non-workspace uid must own no port at all")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		m       Mapping
		wantErr bool
	}{
		{"default", DefaultMapping(), false},
		{"negative uid base", Mapping{UIDBase: -1, PortBase: 30000}, true},
		{"zero port base", Mapping{UIDBase: 10000, PortBase: 0}, true},
		{"port base above the maximum", Mapping{UIDBase: 10000, PortBase: MaxPort + 1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.m.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
