package rewrite

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

type fakeVolumeStore struct {
	volumes []Volume
	inUse   map[string]bool
	removed []string
	failOn  map[string]bool
}

func (f *fakeVolumeStore) ListVolumes(context.Context) ([]Volume, error) { return f.volumes, nil }

func (f *fakeVolumeStore) VolumesInUse(context.Context) (map[string]bool, error) {
	if f.inUse == nil {
		return map[string]bool{}, nil
	}
	return f.inUse, nil
}

func (f *fakeVolumeStore) RemoveVolume(_ context.Context, name string) error {
	if f.failOn[name] {
		return fmt.Errorf("volume %s is in use", name)
	}
	f.removed = append(f.removed, name)
	return nil
}

func managed(name, owner string) Volume {
	return Volume{Name: name, Labels: map[string]string{workspace.ManagedLabel: "share", workspace.OwnerLabel: owner}}
}

func newCollector(store *fakeVolumeStore, owner string) *Collector {
	return &Collector{Volumes: store, Owner: owner}
}

func TestCollectRemovesUnusedManagedVolumes(t *testing.T) {
	store := &fakeVolumeStore{volumes: []Volume{
		managed("rd-0011223344556677", "alice"),
		managed("rd-aabbccddeeff0011", "alice"),
	}}
	c := newCollector(store, "alice")

	removed, err := c.Collect(t.Context())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d, want 2", removed)
	}
}

// The whole risk of garbage collection is deleting somebody's data. Two
// independent checks have to pass, and the prefix alone is not one of them --
// a user is entitled to name a volume "rd-backups".
func TestCollectNeverTouchesVolumesWeDidNotCreate(t *testing.T) {
	store := &fakeVolumeStore{volumes: []Volume{
		{Name: "pgdata"},
		{Name: "rd-backups"},          // our prefix, not our volume
		{Name: "rd-0011223344556677"}, // prefix, no label
		{Name: "node_modules", Labels: map[string]string{workspace.ManagedLabel: "share"}}, // label, no prefix
		managed("rd-aabbccddeeff0011", "alice"),                                            // genuinely ours
	}}
	c := newCollector(store, "alice")

	if _, err := c.Collect(t.Context()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !slices.Equal(store.removed, []string{"rd-aabbccddeeff0011"}) {
		t.Errorf("removed %v, want only rd-aabbccddeeff0011", store.removed)
	}
}

// On a shared daemon another account's share volumes carry the same prefix and
// the same managed label.
func TestCollectLeavesOtherAccountsAlone(t *testing.T) {
	store := &fakeVolumeStore{volumes: []Volume{
		managed("rd-0011223344556677", "alice"),
		managed("rd-aabbccddeeff0011", "bob"),
	}}
	c := newCollector(store, "alice")

	if _, err := c.Collect(t.Context()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !slices.Equal(store.removed, []string{"rd-0011223344556677"}) {
		t.Errorf("removed %v, want only alice's volume", store.removed)
	}
}

func TestCollectKeepsVolumesInUse(t *testing.T) {
	store := &fakeVolumeStore{
		volumes: []Volume{
			managed("rd-0011223344556677", "alice"),
			managed("rd-aabbccddeeff0011", "alice"),
		},
		inUse: map[string]bool{"rd-0011223344556677": true},
	}
	c := newCollector(store, "alice")

	removed, err := c.Collect(t.Context())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d, want 1", removed)
	}
	if slices.Contains(store.removed, "rd-0011223344556677") {
		t.Error("a volume still in use was removed")
	}
}

// A volume claimed by a container between the listing and the removal is a
// race we lose harmlessly and retry next time, not a reason to abort.
func TestCollectSurvivesARemovalFailure(t *testing.T) {
	store := &fakeVolumeStore{
		volumes: []Volume{
			managed("rd-0011223344556677", "alice"),
			managed("rd-aabbccddeeff0011", "alice"),
		},
		failOn: map[string]bool{"rd-0011223344556677": true},
	}
	c := newCollector(store, "alice")

	removed, err := c.Collect(t.Context())
	if err != nil {
		t.Fatalf("a single removal failure aborted collection: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d, want 1", removed)
	}
}

// With no owner set, ownership is not a criterion, but the prefix and label
// still are.
func TestCollectWithoutOwner(t *testing.T) {
	store := &fakeVolumeStore{volumes: []Volume{
		managed("rd-0011223344556677", "alice"),
		managed("rd-aabbccddeeff0011", "bob"),
		{Name: "pgdata"},
	}}
	c := newCollector(store, "")

	removed, err := c.Collect(t.Context())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d, want 2", removed)
	}
	if slices.Contains(store.removed, "pgdata") {
		t.Error("an unmanaged volume was removed")
	}
}

// A machine collects its own volumes and never another machine's.
//
// One account used from two computers labels both the same, so the account is
// not enough to tell them apart. Losing one is not a tidy failure: the daemon
// recreates a missing named volume as an empty local one, so the container
// starts with an empty directory where the project should be.
func TestCollectorLeavesAnotherMachineAlone(t *testing.T) {
	mine := Volume{
		Name:   "rd-aabbccdd-0123456789abcdef",
		Labels: map[string]string{workspace.ManagedLabel: "share", workspace.OwnerLabel: "alice", workspace.ClientLabel: "aabbccdd"},
	}
	theirs := Volume{
		Name:   "rd-11223344-0123456789abcdef",
		Labels: map[string]string{workspace.ManagedLabel: "share", workspace.OwnerLabel: "alice", workspace.ClientLabel: "11223344"},
	}
	unnamed := Volume{
		Name:   "rd-0123456789abcdef",
		Labels: map[string]string{workspace.ManagedLabel: "share", workspace.OwnerLabel: "alice"},
	}

	c := &Collector{Owner: "alice", Client: "aabbccdd"}
	if !c.ours(mine) {
		t.Error("a machine does not recognise its own volume")
	}
	if c.ours(theirs) {
		t.Error("a machine claimed another machine's volume")
	}
	if c.ours(unnamed) {
		t.Error("a volume naming no machine was claimed without --orphans")
	}

	// --orphans widens by exactly the unnamed ones.
	c.Orphans = true
	if !c.ours(unnamed) {
		t.Error("--orphans did not reach a volume naming no machine")
	}
	if c.ours(theirs) {
		t.Error("--orphans reached the other machine's volumes")
	}
}

// A cache volume is never referenced by a container -- a union is bound into
// one by PATH -- so the daemon always calls it unused. Collecting it empties
// the layer under a running container's mount, which is uncommitted work
// vanishing from a directory that still looks mounted (ADR 0044). Only the
// workspace can say, and it is asked.
func TestCollectKeepsACacheVolumeTheWorkspaceHasMounted(t *testing.T) {
	const (
		cache = "rd-aabbccdd-00112233445566ff-cache"
		other = "rd-aabbccdd-ffeeddccbbaa0011"
	)

	for _, c := range []struct {
		name   string
		caches func(context.Context) (map[string]bool, error)
		want   []string
	}{
		{
			name:   "the workspace has a union on it",
			caches: func(context.Context) (map[string]bool, error) { return map[string]bool{cache: true}, nil },
			want:   []string{other},
		},
		{
			name:   "the workspace has no union on it",
			caches: func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil },
			want:   []string{cache, other},
		},
		{
			// Cannot ask means keep: an uncollected cache costs disk, and a
			// collected one that was in use costs somebody's work.
			name:   "there is no channel to ask over",
			caches: nil,
			want:   []string{other},
		},
		{
			name: "the workspace could not answer",
			caches: func(context.Context) (map[string]bool, error) {
				return nil, fmt.Errorf("the cache channel is gone")
			},
			want: []string{other},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeVolumeStore{volumes: []Volume{
				managed(cache, "alice"),
				managed(other, "alice"),
			}}
			collector := newCollector(store, "alice")
			collector.Caches = c.caches

			if _, err := collector.Collect(t.Context()); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			slices.Sort(store.removed)
			if !slices.Equal(store.removed, c.want) {
				t.Errorf("removed %v, want %v", store.removed, c.want)
			}
		})
	}
}
