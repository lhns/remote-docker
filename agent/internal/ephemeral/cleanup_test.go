package ephemeral

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/dockercli"
	"github.com/lhns/remote-docker/agent/internal/unions"
	"github.com/lhns/remote-docker/core/workspace"
)

const (
	runID   = "0123abcd"
	otherID = "4567cdef"
)

// fakeDaemon answers like a daemon holding a few objects, and returns every
// volume whatever the filter, so the cleaner's own checks are what is tested.
type fakeDaemon struct {
	containers map[string][]string // id -> volumes it names
	networks   []string
	volumes    []dockercli.Volume
	failList   error
	failNet    error

	calls []string
}

func (f *fakeDaemon) Containers(_ context.Context, _, _, client string) ([]string, error) {
	if f.failList != nil {
		return nil, f.failList
	}
	var ids []string
	for id := range f.containers {
		if strings.HasPrefix(id, client) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func (f *fakeDaemon) RemoveContainers(_ context.Context, _ string, ids []string) error {
	for _, id := range ids {
		delete(f.containers, id)
	}
	f.calls = append(f.calls, "rm containers")
	return nil
}

func (f *fakeDaemon) Networks(context.Context, string, string) ([]string, error) {
	return f.networks, nil
}

func (f *fakeDaemon) RemoveNetwork(_ context.Context, _, name string) error {
	if f.failNet != nil {
		return f.failNet
	}
	f.calls = append(f.calls, "rm network "+name)
	return nil
}

func (f *fakeDaemon) Volumes(context.Context, string, string) ([]dockercli.Volume, error) {
	if f.failList != nil {
		return nil, f.failList
	}
	return f.volumes, nil
}

func (f *fakeDaemon) VolumesInUse(context.Context, string) (map[string]bool, error) {
	in := map[string]bool{}
	for _, vols := range f.containers {
		for _, v := range vols {
			in[v] = true
		}
	}
	return in, nil
}

func (f *fakeDaemon) RemoveVolume(_ context.Context, _, name string) error {
	f.calls = append(f.calls, "rm volume "+name)
	return nil
}

type fakeUnions struct {
	d       *fakeDaemon
	mounted []string
}

func (u *fakeUnions) Release(context.Context, string, string) {
	u.d.calls = append(u.d.calls, "release unions")
}

func (u *fakeUnions) MountedCaches(string, string, unions.Daemon) []string { return u.mounted }

type targets struct {
	daemons.Targets
	err error
}

func (t targets) Ensure(context.Context, string) (daemons.Target, error) {
	return daemons.Target{}, t.err
}

func volume(name, owner, client string, managed bool) dockercli.Volume {
	labels := map[string]string{workspace.OwnerLabel: owner, workspace.ClientLabel: client}
	if managed {
		labels[workspace.ManagedLabel] = workspace.ManagedShare
	}
	return dockercli.Volume{Name: name, Labels: labels}
}

func newCleaner(d *fakeDaemon, mounted ...string) *Cleaner {
	return &Cleaner{Targets: targets{}, Docker: d, Unions: &fakeUnions{d: d, mounted: mounted}}
}

func removed(d *fakeDaemon) []string {
	var out []string
	for _, c := range d.calls {
		if v, ok := strings.CutPrefix(c, "rm volume "); ok {
			out = append(out, v)
		}
	}
	return out
}

// Only a volume with our prefix, our label, this account and this client is
// removed, and only when nothing holds it.
func TestCleanupRemovesOnlyTheRunsUnheldVolumes(t *testing.T) {
	d := &fakeDaemon{
		containers: map[string][]string{otherID + "-c1": {"rd-" + runID + "-held"}},
		volumes: []dockercli.Volume{
			volume("rd-"+runID+"-free", "alice", runID, true),
			volume("rd-"+runID+"-held", "alice", runID, true),
			volume("rd-"+runID+"-cache-cache", "alice", runID, true),
			volume("backups", "alice", runID, true),                  // not our prefix
			volume("rd-"+runID+"-unlabelled", "alice", runID, false), // not our label
			volume("rd-"+otherID+"-x", "alice", otherID, true),       // another run
			volume("rd-"+runID+"-bobs", "bob", runID, true),          // another account
		},
	}
	err := newCleaner(d, "rd-"+runID+"-cache-cache").Clean(t.Context(), "alice", runID)

	if got, want := removed(d), []string{"rd-" + runID + "-free"}; !slices.Equal(got, want) {
		t.Errorf("removed %v, want %v", got, want)
	}
	if err == nil || !strings.Contains(err.Error(), "kept 2") {
		t.Errorf("err = %v, want one saying two were kept", err)
	}
}

func TestCleanupSucceedsWhenNothingIsKept(t *testing.T) {
	d := &fakeDaemon{volumes: []dockercli.Volume{volume("rd-"+runID+"-a", "alice", runID, true)}}
	if err := newCleaner(d).Clean(t.Context(), "alice", runID); err != nil {
		t.Errorf("err = %v with nothing held", err)
	}
}

// Containers go first, then networks, then unions, then volumes, and only when
// asked for.
func TestCleanupRemovesContainersOnlyWhenAsked(t *testing.T) {
	setup := func() *fakeDaemon {
		return &fakeDaemon{
			containers: map[string][]string{runID + "-c1": {"rd-" + runID + "-a"}},
			networks:   []string{"proj-" + runID + "_default"},
			volumes:    []dockercli.Volume{volume("rd-"+runID+"-a", "alice", runID, true)},
		}
	}

	d := setup()
	if err := newCleaner(d).Clean(t.Context(), "alice", runID); err == nil {
		t.Error("a volume a container names was not reported kept")
	}
	if want := []string{"release unions"}; !slices.Equal(d.calls, want) {
		t.Errorf("without CLEANUP_CONTAINERS: %v, want %v", d.calls, want)
	}

	d = setup()
	c := newCleaner(d)
	c.Containers = true
	if err := c.Clean(t.Context(), "alice", runID); err != nil {
		t.Fatal(err)
	}
	want := []string{"rm containers", "rm network proj-" + runID + "_default", "release unions", "rm volume rd-" + runID + "-a"}
	if !slices.Equal(d.calls, want) {
		t.Errorf("with CLEANUP_CONTAINERS: %v, want %v", d.calls, want)
	}
}

// Cannot tell means keep: nothing is removed, and the error keeps the run.
func TestCleanupKeepsEverythingWhenItCannotTell(t *testing.T) {
	d := &fakeDaemon{
		failList: errors.New("daemon not answering"),
		volumes:  []dockercli.Volume{volume("rd-"+runID+"-a", "alice", runID, true)},
	}
	c := newCleaner(d)
	c.Containers = true
	if err := c.Clean(t.Context(), "alice", runID); err == nil || len(d.calls) != 0 {
		t.Errorf("err = %v, calls %v; want an error and nothing done", err, d.calls)
	}

	d = &fakeDaemon{volumes: []dockercli.Volume{volume("rd-"+runID+"-a", "alice", runID, true)}}
	c = newCleaner(d)
	c.Targets = targets{err: errors.New("the daemon would not start")}
	if err := c.Clean(t.Context(), "alice", runID); err == nil || len(d.calls) != 0 {
		t.Errorf("err = %v, calls %v; want an error and nothing done", err, d.calls)
	}
}

func TestCleanupKeepsANetworkTheDaemonRefuses(t *testing.T) {
	d := &fakeDaemon{networks: []string{"proj-" + runID + "_default"}, failNet: errors.New("has active endpoints")}
	c := newCleaner(d)
	c.Containers = true
	if err := c.Clean(t.Context(), "alice", runID); err == nil {
		t.Error("a network the daemon kept was not reported")
	}
}
