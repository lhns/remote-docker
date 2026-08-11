package machine

// The whole of the decision-making, which is deliberately all of the part that
// can be tested at all. Nobody working on this has WSL or Hyper-V, so anything
// not covered here is covered by somebody running docs/testing-machines.md.

import (
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
