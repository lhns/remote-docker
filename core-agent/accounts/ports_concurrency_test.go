package accounts

// What the port record must keep doing while Preferred is out asking Docker.
// That question boots a cold dind under a 90s budget
// (agent/cmd/remote-dockerd/serve.go), and Owns is asked on every
// tcpip-forward.

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lhns/remote-docker/core/workspace"
)

// blockingPreferred parks until it is released, which is what a cold daemon
// looks like from here.
type blockingPreferred struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingPreferred) For(string, string) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return 0, nil
}

// One machine waiting on a cold daemon must not stop anyone else being told
// which port they own, or another account being given one.
func TestPreferredDoesNotBlockOwnsOrOtherAccounts(t *testing.T) {
	p := newPorts(t)
	// alice's derived port is taken, so her second machine reaches Preferred.
	if _, err := p.For("alice", 10001, "aabbccdd"); err != nil {
		t.Fatal(err)
	}
	// bob is already on record, which is what every ordinary connect is.
	if _, err := p.For("bob", 10002, "99887766"); err != nil {
		t.Fatal(err)
	}

	block := &blockingPreferred{entered: make(chan struct{}), release: make(chan struct{})}
	p.Preferred = block.For

	asked := make(chan error, 1)
	go func() {
		_, err := p.For("alice", 10001, "eeff0011")
		asked <- err
	}()
	<-block.entered

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Owns("alice", 10001, 30001)
		if _, err := p.For("bob", 10002, "99887766"); err != nil {
			t.Error(err)
		}
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Owns and another account's For did not return while Preferred was blocked; one cold daemon stalls every forward on the workspace")
	}

	close(block.release)
	if err := <-asked; err != nil {
		t.Fatal(err)
	}
}

// Two sessions of one machine arriving together get one port and write one
// line: Preferred is computed outside the lock, so the loser's answer has to be
// discarded by the re-check rather than overwrite the winner's.
func TestConcurrentForOneMachineAgreeOnOnePort(t *testing.T) {
	p := newPorts(t)
	if _, err := p.For("alice", 10001, "aabbccdd"); err != nil {
		t.Fatal(err)
	}

	// Slow enough that both callers are inside Preferred before either decides.
	p.Preferred = func(string, string) (int, error) {
		time.Sleep(50 * time.Millisecond)
		return 0, nil
	}

	var wg sync.WaitGroup
	got := make([]int, 2)
	errs := make([]error, 2)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = p.For("alice", 10001, "eeff0011")
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got[0] != got[1] {
		t.Errorf("one machine was given two ports, %d and %d; the volumes built for the loser can never mount", got[0], got[1])
	}

	data, err := os.ReadFile(p.path())
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "alice:eeff0011:") {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("clientports has %d lines for alice's second machine, want 1:\n%s", lines, data)
	}
}

// Reserved is a full store.List() per call, so one decision must not ask it
// about a uid twice: free() checks the preferred port and allocate walks over
// the same one again on its way down.
func TestReservedIsAskedAboutAUIDOnce(t *testing.T) {
	mapping := workspace.Mapping{UIDBase: 10000, PortBase: 30000}
	top, err := mapping.UIDForPort(workspace.MaxPort)
	if err != nil {
		t.Fatal(err)
	}

	asked := map[int]int{}
	p := &Ports{
		Dir:      t.TempDir(),
		Mapping:  mapping,
		Reserved: func(uid int) bool { asked[uid]++; return uid == top },
		// The top of the range is what this machine's volumes want and what
		// allocate reaches first, so both halves of the decision meet it.
		Preferred: func(string, string) (int, error) { return workspace.MaxPort, nil },
	}

	// Take alice's derived port, so her second machine has to allocate.
	if _, err := p.For("alice", 10001, "aabbccdd"); err != nil {
		t.Fatal(err)
	}
	before := asked[top]
	if _, err := p.For("alice", 10001, "eeff0011"); err != nil {
		t.Fatal(err)
	}

	if n := asked[top] - before; n != 1 {
		t.Errorf("Reserved was asked about uid %d %d times in one decision, want 1", top, n)
	}
}
