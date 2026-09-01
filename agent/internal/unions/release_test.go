package unions

import (
	"context"
	"errors"
	"testing"

	"github.com/lhns/remote-docker/core-agent/union"
)

type fakeVolumes struct {
	sources map[string]bool
	err     error
}

func (fakeVolumes) RawMountpoint(context.Context, string, string) (string, error) {
	return "", nil
}

func (f fakeVolumes) MountSources(context.Context, string) (map[string]bool, error) {
	return f.sources, f.err
}

const (
	firstExport  = "/m/aaaa"
	secondExport = "/m/bbbb"
)

func specFor(export string) union.Spec {
	return union.Spec{Export: export, Port: 30001, CacheDir: "/var/lib/docker/volumes/v/_data"}
}

// held builds a manager holding two of an account's unions, without starting
// the child that would serve them.
func held(t *testing.T, vols Volumes) *Manager {
	t.Helper()
	m := &Manager{Volumes: vols, shares: map[string]*live{}}

	for _, export := range []string{firstExport, secondExport} {
		done := make(chan struct{})
		close(done)
		m.shares[key("alice", export)] = &live{
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
	m.ReleaseAccount(context.Background(), "alice")

	if _, ok := m.shares[key("alice", firstExport)]; !ok {
		t.Error("released the union a container is bound to")
	}
	if _, ok := m.shares[key("alice", secondExport)]; ok {
		t.Error("kept the union nothing is bound to")
	}
}

// Nothing holding it means it goes, or a workspace accumulates a mount and a
// process for every share it ever served.
func TestReleaseAccountDropsAUnionNobodyHolds(t *testing.T) {
	m := held(t, fakeVolumes{sources: map[string]bool{}})
	m.ReleaseAccount(context.Background(), "alice")

	if len(m.shares) != 0 {
		t.Errorf("kept %d unions with no container on them", len(m.shares))
	}
}

// The collector's question is answered from the filesystem as well as from this
// process's record, and the filesystem is the half that matters: a union
// outlives the agent that started it, so after a restart the mounts are serving
// and the manager knows nothing about them. Reporting "none" then is truthful
// and costs somebody the contents of a cache their container is still reading.
func TestMountedCachesIncludesWhatThisProcessDidNotStart(t *testing.T) {
	m := held(t, fakeVolumes{})
	m.shares[key("alice", firstExport)].cache = "rd-aabbccdd-aaaa-cache"

	got := m.MountedCaches("alice", "", Daemon{})
	if len(got) != 1 || got[0] != "rd-aabbccdd-aaaa-cache" {
		t.Errorf("MountedCaches = %v, want this process's own record", got)
	}

	// With no client digest nothing can be named for a mount found on disk,
	// because a cache volume's name is per machine (ADR 0029) -- so the record
	// is all there is, and the scan is skipped rather than guessed at.
	if len(m.MountedCaches("bob", "", Daemon{})) != 0 {
		t.Error("another account's shares were reported")
	}
}

// Cannot tell means keep. Taking a mount that is in use costs somebody's
// container permanently; keeping one nobody needs costs a process.
func TestReleaseAccountKeepsEverythingWhenTheDaemonCannotAnswer(t *testing.T) {
	m := held(t, fakeVolumes{err: errors.New("the daemon is not up")})
	m.ReleaseAccount(context.Background(), "alice")

	if len(m.shares) != 2 {
		t.Errorf("kept %d of 2 unions when the daemon could not be asked", len(m.shares))
	}
}
