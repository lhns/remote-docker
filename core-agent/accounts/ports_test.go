package accounts

// Which port serves which of an account's machines, tested with no daemon and
// no network.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

func newPorts(t *testing.T) *Ports {
	t.Helper()
	return &Ports{Dir: t.TempDir(), Mapping: workspace.DefaultMapping()}
}

// A workspace reached from one machine allocates nothing and stays on the port
// its uid has always derived. Anything else would renumber every deployment.
func TestTheFirstMachineGetsTheDerivedPort(t *testing.T) {
	p := newPorts(t)

	port, err := p.For("alice", 10001, "aabbccdd")
	if err != nil {
		t.Fatal(err)
	}
	if port != 30001 {
		t.Errorf("the first machine got port %d, want the derived 30001", port)
	}

	// A client too old to name itself gets the same thing.
	old, err := p.For("alice", 10001, "")
	if err != nil {
		t.Fatal(err)
	}
	if old != 30001 {
		t.Errorf("an unnamed client got port %d, want 30001", old)
	}
}

// The second machine of one account gets a port of its own rather than being
// refused the first one's, which is the whole point.
func TestASecondMachineGetsItsOwnPort(t *testing.T) {
	p := newPorts(t)

	pc, err := p.For("alice", 10001, "aabbccdd")
	if err != nil {
		t.Fatal(err)
	}
	phone, err := p.For("alice", 10001, "11223344")
	if err != nil {
		t.Fatal(err)
	}

	if phone == pc {
		t.Fatalf("both machines were given port %d", pc)
	}
	if phone < p.Mapping.PortBase || phone > workspace.MaxPort {
		t.Errorf("the allocated port %d is outside the workspace range", phone)
	}
}

// Stability is the property ADR 0003 says actually mattered: a machine
// reconnecting must get its port back, or the volumes it created name a port
// nothing is listening on.
func TestAPortIsRememberedAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	mapping := workspace.DefaultMapping()

	first := &Ports{Dir: dir, Mapping: mapping}
	pc, err := first.For("alice", 10001, "aabbccdd")
	if err != nil {
		t.Fatal(err)
	}
	phone, err := first.For("alice", 10001, "11223344")
	if err != nil {
		t.Fatal(err)
	}

	// A different process over the same directory, which is what an agent
	// restart is.
	again := &Ports{Dir: dir, Mapping: mapping}
	if got, err := again.For("alice", 10001, "11223344"); err != nil || got != phone {
		t.Errorf("after a restart the phone got %d (err %v), want %d", got, err, phone)
	}
	if got, err := again.For("alice", 10001, "aabbccdd"); err != nil || got != pc {
		t.Errorf("after a restart the pc got %d (err %v), want %d", got, err, pc)
	}
}

// An allocation must not take a port an account that EXISTS derives. It would
// work until that account connected and then take a working tunnel away.
func TestAllocationSkipsAPortSomebodyDerives(t *testing.T) {
	p := newPorts(t)

	// Pretend the account whose uid maps to the top of the range exists.
	reserved, err := p.Mapping.UIDForPort(workspace.MaxPort)
	if err != nil {
		t.Fatal(err)
	}
	p.Reserved = func(uid int) bool { return uid == reserved }

	if _, err := p.For("alice", 10001, "aabbccdd"); err != nil {
		t.Fatal(err)
	}
	second, err := p.For("alice", 10001, "11223344")
	if err != nil {
		t.Fatal(err)
	}
	if second == workspace.MaxPort {
		t.Errorf("the allocation took port %d, which an existing account derives", second)
	}
}

// Owns is what the forward policy asks instead of recomputing, so it has to
// answer for both kinds of port.
func TestOwns(t *testing.T) {
	p := newPorts(t)

	// The pc first, so it takes the derived port and the phone is allocated
	// one, which is the pair this has to answer for.
	if _, err := p.For("alice", 10001, "aabbccdd"); err != nil {
		t.Fatal(err)
	}
	allocated, err := p.For("alice", 10001, "11223344")
	if err != nil {
		t.Fatal(err)
	}

	if !p.Owns("alice", 10001, 30001) {
		t.Error("the account does not own the port its uid derives")
	}
	if !p.Owns("alice", 10001, allocated) {
		t.Errorf("the account does not own the port %d it was allocated", allocated)
	}
	if p.Owns("bob", 10002, allocated) {
		t.Error("another account owns a port allocated to alice")
	}
}

// The record is one line per assignment and sorted, because people read it
// too and a file that shuffles on every write hides what changed.
func TestTheRecordIsReadable(t *testing.T) {
	p := newPorts(t)
	if _, err := p.For("alice", 10001, "aabbccdd"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.For("alice", 10001, "11223344"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(p.Dir, "clientports"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if want := "alice:11223344:"; got[:len(want)] != want {
		t.Errorf("the record does not start with the lowest entry:\n%s", got)
	}
}
