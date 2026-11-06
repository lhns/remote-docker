package config

import (
	"slices"
	"testing"
)

func TestParseHostPorts(t *testing.T) {
	for in, want := range map[string][]int{
		"":                nil,
		"  ":              nil,
		"3000":            {3000},
		"3000,8931":       {3000, 8931},
		"8931, 3000,3000": {3000, 8931},
		"1,65535":         {1, 65535},
		"3000,,8931,":     {3000, 8931},
	} {
		got, err := ParseHostPorts(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("ParseHostPorts(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

// A bad entry fails the whole setting: dropping it would silently forward a
// shorter list than the one written.
func TestParseHostPortsRefuses(t *testing.T) {
	for _, in := range []string{"0", "65536", "-1", "http", "3000,x", "80.5", "localhost:3000"} {
		if got, err := ParseHostPorts(in); err == nil {
			t.Errorf("ParseHostPorts(%q) = %v, want an error", in, got)
		}
	}
}

func TestHostPortsComeFromFileAndEnvironment(t *testing.T) {
	path := writeConfig(t, `{"host":"h","hostPorts":"3000"}`)

	cfg, err := Resolve(Overrides{}, path)
	if err != nil || cfg.HostPorts != "3000" {
		t.Fatalf("file: HostPorts = %q, %v", cfg.HostPorts, err)
	}

	t.Setenv(EnvHostPorts, "3000,8931")
	if cfg, err = Resolve(Overrides{}, path); err != nil || cfg.HostPorts != "3000,8931" {
		t.Fatalf("environment: HostPorts = %q, %v", cfg.HostPorts, err)
	}
}

// Spelling and order are not a change, so they do not warn on every command.
func TestHostPortsAreComparedByWhatTheyMean(t *testing.T) {
	a := Config{HostPorts: "8931,3000"}.SessionSettings()["host-ports"]
	b := Config{HostPorts: "3000, 8931,3000"}.SessionSettings()["host-ports"]
	if a != "3000,8931" || a != b {
		t.Errorf("host-ports = %q and %q, want both 3000,8931", a, b)
	}
}
