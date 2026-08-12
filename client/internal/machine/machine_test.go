package machine

// The whole of the decision-making, which is deliberately all of the part that
// can be tested at all. Nobody working on this has WSL or Hyper-V, so anything
// not covered here is covered by somebody running docs/testing-machines.md.

import (
	"path/filepath"
	"strings"
	"testing"
)

func spec() Spec {
	return Spec{
		Name:    "rd-dev",
		Backend: "wsl",
		Image:   "ghcr.io/lhns/remote-docker-workspace:0.1.0",
		Rootfs: `C:
ootfs.tar`,
		CPUs:     4,
		MemoryMB: 4096,
		Port:     2222,
		Account:  "alice",
	}
}

func TestPlan(t *testing.T) {
	current := spec().Generation()

	for _, tc := range []struct {
		name     string
		observed Observed
		want     Action
	}{
		{"nothing there", Observed{State: Absent}, Create},
		{"there and running and current", Observed{State: Running, Generation: current}, Nothing},
		{"there and stopped and current", Observed{State: Stopped, Generation: current}, Start},

		// The case the generation exists for. A machine built from older
		// settings is not repaired in place: there is no install to repair,
		// because nothing was installed.
		{"built from other settings", Observed{State: Running, Generation: "0000000000000000"}, Recreate},
		{"built from other settings, stopped", Observed{State: Stopped, Generation: "0000000000000000"}, Recreate},

		// A backend that cannot read a generation must not cause a machine to
		// be destroyed. Somebody's containers are in there, and losing them to
		// satisfy our own bookkeeping is the worse failure.
		{"generation unknown, running", Observed{State: Running}, Nothing},
		{"generation unknown, stopped", Observed{State: Stopped}, Start},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Plan(spec(), tc.observed); got != tc.want {
				t.Errorf("Plan(%+v) = %v, want %v", tc.observed, got, tc.want)
			}
		})
	}
}

// Every field has to move the generation, or a setting can be changed and the
// machine will go on being considered current.
func TestEveryFieldChangesTheGeneration(t *testing.T) {
	base := spec().Generation()

	for _, tc := range []struct {
		field  string
		change func(*Spec)
	}{
		{"Name", func(s *Spec) { s.Name = "rd-other" }},
		{"Backend", func(s *Spec) { s.Backend = "hyperv" }},
		{"Image", func(s *Spec) { s.Image = "ghcr.io/lhns/remote-docker-workspace:0.2.0" }},
		{"Rootfs", func(s *Spec) { s.Rootfs = `C:\other.tar` }},
		{"CPUs", func(s *Spec) { s.CPUs = 8 }},
		{"MemoryMB", func(s *Spec) { s.MemoryMB = 8192 }},
		{"Port", func(s *Spec) { s.Port = 2223 }},
		{"Account", func(s *Spec) { s.Account = "bob" }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			changed := spec()
			tc.change(&changed)
			if changed.Generation() == base {
				t.Errorf("changing %s left the generation at %s", tc.field, base)
			}
		})
	}
}

func TestGenerationIsStable(t *testing.T) {
	first, second := spec().Generation(), spec().Generation()
	if first != second {
		t.Errorf("the same spec produced %s and then %s", first, second)
	}
	if got := len(spec().Generation()); got != 16 {
		t.Errorf("generation is %d characters, want 16", got)
	}
}

// The error a caller gets on a platform with no backend has to say what to do
// instead, because "unavailable" is where somebody gives up.
func TestFindWithoutABackend(t *testing.T) {
	if len(Backends()) > 0 {
		t.Skip("this platform has backends; the no-backend message is not reachable here")
	}

	_, err := Find("wsl")
	if err == nil {
		t.Fatal("a backend was found on a platform that has none")
	}
	if !strings.Contains(err.Error(), "remote create") {
		t.Errorf("the error does not say what to do instead:\n%v", err)
	}
}

// One address rule for both backends.
//
// The link-local guard belongs to both, not to Hyper-V alone. A machine whose
// DHCP has not finished reports a 169.254 address, and handing that back gives
// somebody an address to dial that cannot work. Report nothing and let the
// caller keep waiting, which is the truth about a machine that is up and not
// ready.
func TestFirstIPv4(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		in         []string
	}{
		{"an ordinary address", "172.19.4.7", []string{"fe80::1", "172.19.4.7"}},
		{"a prefix length is the machine's, not ours", "172.24.110.158", []string{"172.24.110.158/20"}},
		{"link-local means DHCP has not finished", "", []string{"169.254.12.9", "fe80::1"}},
		{"nothing reported", "", nil},
		{"only IPv6", "", []string{"fe80::215:5dff:fe00:1", "2001:db8::1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstIPv4(tc.in); got != tc.want {
				t.Errorf("firstIPv4(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// One state directory for both backends. They differ in what they put there,
// never in where it goes, and two copies of a path is two answers to what `rm`
// deletes.
func TestStateDir(t *testing.T) {
	t.Setenv("LOCALAPPDATA", filepath.Join("C:", "Users", "x", "AppData", "Local"))
	dir, err := stateDir("dev")
	if err != nil {
		t.Fatalf("stateDir: %v", err)
	}
	if !strings.HasSuffix(dir, filepath.Join("remote-docker", "machines", "dev")) {
		t.Errorf("stateDir = %q", dir)
	}

	// Reported rather than guessed at: a machine built under a path we invented
	// is one `rm` would not find.
	t.Setenv("LOCALAPPDATA", "")
	if _, err := stateDir("dev"); err == nil {
		t.Error("stateDir invented a location with nowhere to put it")
	}
}
