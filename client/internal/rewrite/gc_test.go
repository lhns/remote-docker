package rewrite

import (
	"context"
	"fmt"
	"slices"
	"testing"
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
	return Volume{Name: name, Labels: map[string]string{ManagedLabel: "share", OwnerLabel: owner}}
}

func newCollector(store *fakeVolumeStore, owner string) *Collector {
	return &Collector{Volumes: store, Remover: store, InUse: store, Owner: owner}
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
		{Name: "node_modules", Labels: map[string]string{ManagedLabel: "share"}}, // label, no prefix
		managed("rd-aabbccddeeff0011", "alice"),                                  // genuinely ours
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
