package machine

import (
	"encoding/json"
	"strings"
	"testing"
)

// The drop-in is how the VM's docker daemon, which pulls every image, learns
// of the proxy: present exactly when one is set.
func TestIgnitionProxyDropIn(t *testing.T) {
	dropIns := func(t *testing.T, spec Spec) []string {
		t.Helper()
		raw, err := ignition(spec, "ssh-ed25519 AAAA dev@host")
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Systemd struct {
				Units []struct {
					Name    string
					Dropins []struct{ Name, Contents string }
				}
			}
		}
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, u := range doc.Systemd.Units {
			if u.Name != "docker.service" {
				continue
			}
			for _, d := range u.Dropins {
				out = append(out, d.Name+"\n"+d.Contents)
			}
		}
		return out
	}

	spec := Spec{Name: "dev", Account: "dev", Port: 2222, Image: "example.com/ws:1"}
	if got := dropIns(t, spec); len(got) != 0 {
		t.Errorf("no proxy, but docker.service got a drop-in: %v", got)
	}

	spec.Proxy = "http://proxy.example:3128/a%20b"
	got := dropIns(t, spec)
	if len(got) != 1 {
		t.Fatalf("drop-ins = %v, want one", got)
	}
	for _, want := range []string{
		"http-proxy.conf\n[Service]\n",
		`"HTTP_PROXY=http://proxy.example:3128/a%%20b"`,
		`"HTTPS_PROXY=http://proxy.example:3128/a%%20b"`,
		`"NO_PROXY=localhost,127.0.0.0/8,::1,169.254.0.0/16,172.16.0.0/12"`,
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("drop-in is missing %q:\n%s", want, got[0])
		}
	}
}

// A machine built before Proxy existed must not read as out of date.
func TestEmptyProxyLeavesTheGenerationAlone(t *testing.T) {
	a := Spec{Name: "dev", Backend: "hyperv", Port: 2222, Account: "dev"}
	b := a
	b.Proxy = ""
	if a.Generation() != b.Generation() {
		t.Error("an empty proxy changed the generation")
	}
	b.Proxy = "http://proxy.example:3128"
	if a.Generation() == b.Generation() {
		t.Error("a proxy left the generation alone")
	}
}
