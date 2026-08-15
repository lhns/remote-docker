package accounts

// Which port serves which of an account's machines, tested with no daemon and
// no network.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// A machine the record has forgotten is given the port its volumes need.
//
// This is the whole point of Preferred. Losing the record and handing out a
// fresh port leaves every volume that machine created unmountable, and the
// failure is "connection refused" against a port nothing explains.
func TestForPrefersThePortTheVolumesNeed(t *testing.T) {
	p := &Ports{
		Dir:     t.TempDir(),
		Mapping: workspace.Mapping{UIDBase: 10000, PortBase: 30000},
		// Nothing recorded: the record was lost, which is the case.
		Preferred: func(account, client string) (int, error) {
			if account == "alice" && client == "laptop" {
				return 39998, nil
			}
			return 0, nil
		},
	}

	got, err := p.For("alice", 10005, "laptop")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if got != 39998 {
		t.Errorf("For = %d, want the port alice's laptop volumes name, 39998", got)
	}

	// And it is remembered, so the question is asked once.
	again, err := p.For("alice", 10005, "laptop")
	if err != nil || again != 39998 {
		t.Errorf("For = %d, %v on the second ask, want 39998", again, err)
	}
}

// A port another machine already holds is not taken, however much this one
// wants it: one listener holds a port, and moving it would only relocate the
// failure.
func TestForWillNotTakeAnotherMachinesPort(t *testing.T) {
	p := &Ports{
		Dir:       t.TempDir(),
		Mapping:   workspace.Mapping{UIDBase: 10000, PortBase: 30000},
		Preferred: func(string, string) (int, error) { return 39998, nil },
	}

	// The first machine is given 39998 by asking for it.
	first, err := p.For("alice", 10005, "laptop")
	if err != nil || first != 39998 {
		t.Fatalf("setting up: got %d, %v", first, err)
	}

	second, err := p.For("alice", 10005, "desktop")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if second == 39998 {
		t.Error("two machines of one account were given the same port")
	}
}

// A port an account that EXISTS derives is not taken either. That account is
// entitled to it whether or not it has ever connected, so handing it out works
// until they connect and then takes a working tunnel away.
func TestForWillNotTakeAPortAnAccountDerives(t *testing.T) {
	mapping := workspace.Mapping{UIDBase: 10000, PortBase: 30000}
	bobs, err := mapping.PortForUID(10007)
	if err != nil {
		t.Fatal(err)
	}

	p := &Ports{
		Dir:       t.TempDir(),
		Mapping:   mapping,
		Reserved:  func(uid int) bool { return uid == 10007 },
		Preferred: func(string, string) (int, error) { return bobs, nil },
	}

	got, err := p.For("alice", 10005, "laptop")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if got == bobs {
		t.Errorf("alice was given %d, which bob's uid derives", got)
	}
}

// Nothing to prefer, or nobody to ask, leaves the answer exactly as it was.
// That is what makes a broken or absent daemon cost a rebuild rather than a
// session.
func TestForWithoutPreferredIsUnchanged(t *testing.T) {
	mapping := workspace.Mapping{UIDBase: 10000, PortBase: 30000}
	want, err := mapping.PortForUID(10005)
	if err != nil {
		t.Fatal(err)
	}

	for name, prefer := range map[string]func(string, string) (int, error){
		"no hook":     nil,
		"hook says 0": func(string, string) (int, error) { return 0, nil },
	} {
		p := &Ports{Dir: t.TempDir(), Mapping: mapping, Preferred: prefer}
		got, err := p.For("alice", 10005, "laptop")
		if err != nil {
			t.Fatalf("%s: For: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: For = %d, want the derived port %d", name, got, want)
		}
	}
}

// A machine whose daemon could not be asked is refused, rather than quietly
// given the port its uid derives.
//
// Measured on a real workspace: while an account's daemon crash-looped, its
// machine was forwarded 65534 in one session and asked for 30000 in the next.
// 30000 is the derived port, which another machine is likeliest to hold, so
// the forward was refused; the volumes built for 65534 would not have mounted
// either.
func TestForRefusesWhenTheDaemonCannotBeAsked(t *testing.T) {
	p := &Ports{
		Dir:     t.TempDir(),
		Mapping: workspace.Mapping{UIDBase: 10000, PortBase: 30000},
		Preferred: func(string, string) (int, error) {
			return 0, errors.New("dind: container exited with code 1")
		},
	}

	got, err := p.For("alice", 10000, "laptop")
	if err == nil {
		t.Fatalf("For = %d with no error; a port was chosen while its daemon was down", got)
	}
	if !strings.Contains(err.Error(), "alice") {
		t.Errorf("the error does not name the account: %v", err)
	}
}

// The other zero, which must keep working: a machine that HAS no volumes is an
// ordinary new machine and gets a port allocated as one. Both answers were the
// same value before, which is the defect above.
func TestForAllocatesForAMachineWithNoVolumes(t *testing.T) {
	mapping := workspace.Mapping{UIDBase: 10000, PortBase: 30000}
	want, err := mapping.PortForUID(10000)
	if err != nil {
		t.Fatal(err)
	}

	p := &Ports{
		Dir:       t.TempDir(),
		Mapping:   mapping,
		Preferred: func(string, string) (int, error) { return 0, nil },
	}

	got, err := p.For("alice", 10000, "laptop")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if got != want {
		t.Errorf("For = %d, want the derived port %d", got, want)
	}
}
