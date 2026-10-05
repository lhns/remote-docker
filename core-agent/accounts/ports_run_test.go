package accounts

// An ephemeral run's port (ADR 0050): allocated like a second machine's, held
// in memory only, and freed by its token.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

func TestARunIsNeverInClientports(t *testing.T) {
	p := newPorts(t)
	run, _, err := p.ForRun("alice", "0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	if run == 30001 {
		t.Error("a run took the account's derived port")
	}
	// A machine's assignment writes the record; the run must not be in it.
	if _, err := p.For("bob", 10002, "aabbccdd"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(p.Dir, "clientports"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "0123abcd") {
		t.Errorf("clientports holds the run:\n%s", data)
	}
	if !p.Owns("alice", 10001, run) || p.Owns("bob", 10002, run) {
		t.Errorf("port %d: alice owns it %v, bob %v; want alice only", run, p.Owns("alice", 10001, run), p.Owns("bob", 10002, run))
	}
}

func TestFreeReleasesARunsPort(t *testing.T) {
	p := newPorts(t)
	port, token, err := p.ForRun("alice", "0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	if again, _, _ := p.ForRun("alice", "0123abcd"); again != port {
		t.Errorf("the run was given %d and then %d", port, again)
	}
	if !p.Free("alice", "0123abcd", token) {
		t.Fatal("Free released nothing")
	}
	if p.Owns("alice", 10001, port) {
		t.Error("alice still owns a freed port")
	}
	if next, _, _ := p.ForRun("alice", "4567cdef"); next != port {
		t.Errorf("the next run got %d, want the freed %d", next, port)
	}
}

// ADR 0028's late release: a token from before the run was given its port
// again frees nothing.
func TestALateFreeLeavesTheSuccessor(t *testing.T) {
	p := newPorts(t)
	_, stale, err := p.ForRun("alice", "0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	p.Free("alice", "0123abcd", stale)
	port, _, err := p.ForRun("alice", "0123abcd")
	if err != nil {
		t.Fatal(err)
	}

	if p.Free("alice", "0123abcd", stale) || p.Free("alice", "0123abcd", 0) {
		t.Error("a stale or zero token freed the run's port")
	}
	if !p.Owns("alice", 10001, port) {
		t.Error("the run lost its port to a late Free")
	}
}

// A machine's port is never freed, whatever the token.
func TestFreeLeavesAMachine(t *testing.T) {
	p := newPorts(t)
	port, err := p.For("alice", 10001, "aabbccdd")
	if err != nil {
		t.Fatal(err)
	}
	for token := uint64(0); token < 4; token++ {
		if p.Free("alice", "aabbccdd", token) {
			t.Fatalf("token %d freed a machine's port", token)
		}
	}
	if !p.Owns("alice", 10001, port) {
		t.Error("alice lost her machine's port")
	}
}

// After an agent restart a run is given back the port it had, unless that is
// somebody else's now.
func TestHoldRestoresARunsPort(t *testing.T) {
	p := newPorts(t)
	if _, ok := p.Hold("alice", "0123abcd", workspace.MaxPort); !ok {
		t.Fatal("Hold refused a free port")
	}
	if port, _, _ := p.ForRun("alice", "0123abcd"); port != workspace.MaxPort {
		t.Errorf("the run was given %d, want the held %d", port, workspace.MaxPort)
	}

	bob, err := p.For("bob", 10002, "aabbccdd")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Hold("alice", "4567cdef", bob); ok {
		t.Error("Hold gave a run bob's port")
	}
}

// A purged account's machines are dropped from the record, and nobody else's.
func TestForgetDropsOneAccountsMachines(t *testing.T) {
	p := newPorts(t)
	for _, c := range []string{"aabbccdd", "11223344"} {
		if _, err := p.For("bob", 10002, c); err != nil {
			t.Fatal(err)
		}
	}
	alice, err := p.For("alice", 10001, "aabbccdd")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Forget("bob"); err != nil {
		t.Fatal(err)
	}

	reread := &Ports{Dir: p.Dir, Mapping: p.Mapping}
	for _, c := range []string{"aabbccdd", "11223344"} {
		if port, known, err := reread.Lookup("bob", c); err != nil || known {
			t.Errorf("bob's machine %s is still recorded at %d (%v)", c, port, err)
		}
	}
	if port, known, _ := reread.Lookup("alice", "aabbccdd"); !known || port != alice {
		t.Errorf("alice's machine: %d %v, want %d", port, known, alice)
	}
}
