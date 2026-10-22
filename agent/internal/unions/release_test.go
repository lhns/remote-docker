package unions

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core-agent/union"
	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/core/workspace"
)

type fakeVolumes struct {
	mountpoint string
	sources    map[string]bool
	err        error

	// asked runs while the daemon is being asked which mounts are in use.
	asked func()
}

func (f fakeVolumes) RawMountpoint(context.Context, string, string) (string, error) {
	return f.mountpoint, nil
}

func (f fakeVolumes) MountSources(context.Context, string) (map[string]bool, error) {
	if f.asked != nil {
		f.asked()
	}
	return f.sources, f.err
}

const (
	firstExport  = "/m/aaaa"
	secondExport = "/m/bbbb"
)

func specFor(export string) union.Spec {
	return union.Spec{Export: export, Client: thisClient, Port: 30001, CacheDir: "/var/lib/docker/volumes/v/_data"}
}

// held builds a manager holding two of an account's unions, without starting
// the child that would serve them.
func held(t *testing.T, vols Volumes) *Manager {
	t.Helper()
	m := &Manager{Volumes: vols, shares: map[string]*live{}}

	for _, export := range []string{firstExport, secondExport} {
		done := make(chan struct{})
		close(done)
		m.shares[key("alice", thisClient, export)] = &live{
			spec: specFor(export), cancel: func() {}, done: done,
		}
	}
	return m
}

// The union outlives the channel that asked for it. The cache channel closes
// whenever the connection under it is released (ADR 0015), while the container
// bound to the union keeps running -- so releasing then left every later
// request answering "has no cache; prepare it first", against a container whose
// mount could no longer be repaired.
func TestReleaseAccountKeepsAUnionAContainerHolds(t *testing.T) {
	m := held(t, fakeVolumes{sources: map[string]bool{specFor(firstExport).Merged(): true}})
	m.Release(context.Background(), "alice", thisClient)

	if _, ok := m.shares[key("alice", thisClient, firstExport)]; !ok {
		t.Error("released the union a container is bound to")
	}
	if _, ok := m.shares[key("alice", thisClient, secondExport)]; ok {
		t.Error("kept the union nothing is bound to")
	}
}

// Nothing holding it means it goes, or a workspace accumulates a mount and a
// process for every share it ever served.
func TestReleaseAccountDropsAUnionNobodyHolds(t *testing.T) {
	m := held(t, fakeVolumes{sources: map[string]bool{}})
	m.Release(context.Background(), "alice", thisClient)

	if len(m.shares) != 0 {
		t.Errorf("kept %d unions with no container on them", len(m.shares))
	}
}

// A client replaces a cache channel it no longer trusts while the agent is
// still ending the old one. The old one's end must not release what the new
// one prepares, before any container exists to hold it.
func TestASessionEndingReleasesNothingWhileAnotherOfTheMachineIsOpen(t *testing.T) {
	m := held(t, fakeVolumes{sources: map[string]bool{}})
	endOld := m.Open("alice", thisClient)
	endNew := m.Open("alice", thisClient)

	endOld(context.Background())
	if len(m.shares) != 2 {
		t.Fatalf("the old session's end released %d unions while the new one was open", 2-len(m.shares))
	}

	endNew(context.Background())
	if len(m.shares) != 0 {
		t.Errorf("the last session's end kept %d unions nothing holds", len(m.shares))
	}
}

// A session that opens, prepares and ends while a release asks the daemon: its
// container did not exist when the daemon answered, so that answer cannot keep
// its union, and the union must not go on it.
func TestAUnionPreparedWhileTheDaemonWasAskedIsKept(t *testing.T) {
	var m *Manager
	m = held(t, fakeVolumes{sources: map[string]bool{}, asked: func() {
		m.stamp(m.shares[key("alice", thisClient, firstExport)])
	}})
	m.Release(context.Background(), "alice", thisClient)

	if _, ok := m.shares[key("alice", thisClient, firstExport)]; !ok {
		t.Error("released a union prepared after the daemon was asked")
	}
	if _, ok := m.shares[key("alice", thisClient, secondExport)]; ok {
		t.Error("kept a union nothing holds and nothing prepared since")
	}
}

// One machine's session ending is not another's: the second machine may have
// prepared a union its container has not bound yet.
func TestReleaseLeavesAnotherMachinesUnions(t *testing.T) {
	m := held(t, fakeVolumes{sources: map[string]bool{}})
	done := make(chan struct{})
	close(done)
	other := key("alice", "11223344", firstExport)
	m.shares[other] = &live{spec: specFor(firstExport), cancel: func() {}, done: done}

	m.Release(context.Background(), "alice", thisClient)

	if _, ok := m.shares[other]; !ok || len(m.shares) != 1 {
		t.Errorf("one machine's release left %d unions, want only the other machine's", len(m.shares))
	}
}

// The collector's question is answered from the filesystem as well as from this
// process's record, and the filesystem is the half that matters: a union
// outlives the agent that started it, so after a restart the mounts are serving
// and the manager knows nothing about them. Reporting "none" then is truthful
// and costs somebody the contents of a cache their container is still reading.
func TestMountedCachesIncludesWhatThisProcessDidNotStart(t *testing.T) {
	m := held(t, fakeVolumes{})
	m.shares[key("alice", thisClient, firstExport)].cache = "rd-aabbccdd-aaaa-cache"

	mounted := func(account, client string) []string {
		t.Helper()
		got, err := m.MountedCaches(account, client, Daemon{})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := mounted("alice", thisClient)
	if len(got) != 1 || got[0] != "rd-aabbccdd-aaaa-cache" {
		t.Errorf("MountedCaches = %v, want this process's own record", got)
	}

	if len(mounted("bob", thisClient)) != 0 {
		t.Error("another account's shares were reported")
	}
	if len(mounted("alice", "11223344")) != 0 {
		t.Error("another machine's shares were reported")
	}
}

// Cannot tell means keep. Taking a mount that is in use costs somebody's
// container permanently; keeping one nobody needs costs a process.
func TestReleaseAccountKeepsEverythingWhenTheDaemonCannotAnswer(t *testing.T) {
	m := held(t, fakeVolumes{err: errors.New("the daemon is not up")})
	m.Release(context.Background(), "alice", thisClient)

	if len(m.shares) != 2 {
		t.Errorf("kept %d of 2 unions when the daemon could not be asked", len(m.shares))
	}
}

// A prepare may only name THIS machine's cache volume.
//
// Validate asks whether the name is a managed one, which every machine of an
// account satisfies for every other machine's volumes -- one account's machines
// share a daemon (ADR 0029). Without this a second machine could have the agent
// mount somebody else's cache as the upper of its own union and write into it
// through its own container.
func TestPrepareRefusesAnotherMachinesCache(t *testing.T) {
	const (
		mine   = "aabbccdd"
		theirs = "11223344"
		export = "/m/00112233445566ff"
	)

	ours, err := workspace.CacheVolumeForExport(mine, export)
	if err != nil {
		t.Fatal(err)
	}
	notOurs, err := workspace.CacheVolumeForExport(theirs, export)
	if err != nil {
		t.Fatal(err)
	}
	if ours == notOurs {
		t.Fatal("the two machines derived the same cache volume name")
	}

	m := &Manager{Volumes: fakeVolumes{}, shares: map[string]*live{}}
	req := cache.Request{
		Op: cache.OpPrepare, Export: export, Port: 30001, Cache: notOurs,
	}

	// It is a managed volume, so Validate accepts it: this refusal is the only
	// thing between the two machines.
	if err := req.Validate(); err != nil {
		t.Fatalf("the request itself was refused, so this test proves nothing: %v", err)
	}

	_, err = m.Prepare(context.Background(), "alice", mine, Daemon{}, req)
	if err == nil {
		t.Fatal("the agent agreed to mount another machine's cache")
	}
	if !strings.Contains(err.Error(), "this machine's cache volume") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}
