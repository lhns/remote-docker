package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/prune"
	"github.com/lhns/remote-docker/core/workspace"
)

func TestPruneSettings(t *testing.T) {
	cfg, err := pruneSettings("", "", "", "", "")
	if err != nil || cfg.After != 7*24*time.Hour || cfg.Above != 80 || cfg.Containers ||
		cfg.Interval != time.Hour || cfg.Floor != time.Hour {
		t.Errorf("defaults = %+v, %v", cfg, err)
	}
	if !cfg.enabled() {
		t.Error("pruning is not on by default")
	}

	cfg, err = pruneSettings("36h", "90%", "true", "5s", "1s")
	if err != nil || cfg.After != 36*time.Hour || cfg.Above != 90 || !cfg.Containers ||
		cfg.Interval != 5*time.Second || cfg.Floor != time.Second {
		t.Errorf("set = %+v, %v", cfg, err)
	}

	for _, off := range []string{"0", "off"} {
		cfg, err = pruneSettings(off, off, "", "", "")
		if err != nil || cfg.After != 0 || cfg.Above != 0 || cfg.enabled() {
			t.Errorf("%q = %+v, %v; want both rules off", off, cfg, err)
		}
	}

	for _, bad := range [][5]string{
		{"garbage"}, {"-1d"}, {"7"}, {"", "garbage"}, {"", "100%"}, {"", "-5"},
		{"", "", "maybe"}, {"", "", "", "0"}, {"", "", "", "soon"}, {"", "", "", "", "soon"},
	} {
		if cfg, err := pruneSettings(bad[0], bad[1], bad[2], bad[3], bad[4]); err == nil {
			t.Errorf("%q was accepted: %+v", bad, cfg)
		}
	}
}

// fakeDocker is a set of daemons keyed by host, and the removals asked of them.
type fakeDocker struct {
	images     map[string][]prune.Image
	containers map[string][]prune.Container
	refuse     map[string]bool // refs the daemon refuses to remove
	removed    []string        // host/ref
	rmContr    []string
	builds     []string // host
	used       *diskModel
}

// diskModel is a disk each removed image gives back some space to.
type diskModel struct {
	total, used uint64
	perImage    uint64
}

func (f *fakeDocker) Images(_ context.Context, host string) ([]prune.Image, error) {
	return slices.Clone(f.images[host]), nil
}

func (f *fakeDocker) Containers(_ context.Context, host string) ([]prune.Container, error) {
	return slices.Clone(f.containers[host]), nil
}

func (f *fakeDocker) CreatedImages(context.Context, string, time.Time, time.Time) ([]string, error) {
	return nil, nil
}

func (f *fakeDocker) RemoveImage(_ context.Context, host, ref string) error {
	if f.refuse[ref] {
		return errors.New("image is being used by stopped container")
	}
	f.removed = append(f.removed, host+"/"+ref)
	f.images[host] = slices.DeleteFunc(f.images[host], func(i prune.Image) bool {
		return slices.Contains(i.Refs(), ref)
	})
	if f.used != nil {
		f.used.used -= f.used.perImage
	}
	return nil
}

func (f *fakeDocker) RemoveContainer(_ context.Context, host, id string) error {
	f.rmContr = append(f.rmContr, host+"/"+id)
	return nil
}

func (f *fakeDocker) BuildPrune(_ context.Context, host string, _ time.Duration) error {
	f.builds = append(f.builds, host)
	return nil
}

func (f *fakeDocker) RootDir(context.Context, string) (string, error) { return "/var/lib/docker", nil }

type fakeTargets struct {
	daemons.Targets
	mode  string
	hosts map[string]string // account -> host; absent means not running
}

func (f fakeTargets) Mode() string { return f.mode }

func (f fakeTargets) Lookup(_ context.Context, account string) (daemons.Target, bool) {
	h, ok := f.hosts[account]
	return daemons.Target{Host: h}, ok
}

var base = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

type harness struct {
	p       *pruner
	d       *fakeDocker
	clock   *time.Time
	metrics map[string]float64
}

func newHarness(t *testing.T, mode string, cfg pruneConfig, d *fakeDocker, hosts map[string]string) *harness {
	t.Helper()
	clock := base
	h := &harness{d: d, clock: &clock, metrics: map[string]float64{}}
	h.p = &pruner{
		cfg:       cfg,
		targets:   fakeTargets{mode: mode, hosts: hosts},
		accounts:  func() []string { return []string{"alice", "bob"} },
		docker:    d,
		now:       func() time.Time { return *h.clock },
		dir:       filepath.Join(t.TempDir(), "prune"),
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		keep:      []string{"docker:28-dind"},
		reclaimed: func(b float64, account, reason string) { h.metrics[account+"/"+reason] += b },
		measure: func(string) (diskUsage, error) {
			if d.used == nil {
				return diskUsage{Used: 10, Avail: 90}, nil
			}
			return diskUsage{Used: d.used.used, Avail: d.used.total - d.used.used}, nil
		},
	}
	return h
}

func (h *harness) advance(d time.Duration) { *h.clock = h.clock.Add(d) }

func TestAgePerAccount(t *testing.T) {
	d := &fakeDocker{
		images: map[string][]prune.Image{
			"": {
				{ID: "sha256:dind", Tags: []string{"docker:28-dind"}},
				{ID: "sha256:oldparent", Tags: []string{"old:1"}},
			},
			"unix://alice": {
				{ID: "sha256:a1", Tags: []string{"alpine:3", "alpine:latest"}},
				{ID: "sha256:a2", Tags: []string{"kept:1"}},
				{ID: "sha256:a3", Tags: []string{"stopped:1"}},
			},
		},
		containers: map[string][]prune.Container{
			"": {{ID: "acct", Name: "/rd-dind-alice", ImageID: "sha256:other", Status: "running"}},
			"unix://alice": {
				{ID: "c1", Name: "/old", ImageID: "sha256:a3", Status: "exited", Finished: base.Add(-30 * 24 * time.Hour)},
				{ID: "c2", Name: "/web", ImageID: "sha256:a2", Status: "running"},
			},
		},
		refuse: map[string]bool{},
	}
	cfg := pruneConfig{After: 7 * 24 * time.Hour, Interval: time.Hour, Floor: time.Hour}
	h := newHarness(t, workspace.ModePerAccount, cfg, d, map[string]string{"alice": "unix://alice"})

	h.p.sweep(context.Background(), time.Hour) // meets everything: nothing is old yet
	if len(d.removed) != 0 {
		t.Fatalf("removed %v on first sight", d.removed)
	}

	h.advance(8 * 24 * time.Hour)
	h.p.sweep(context.Background(), time.Hour)

	want := []string{"/old:1", "unix://alice/alpine:3", "unix://alice/alpine:latest"}
	slices.Sort(want)
	got := slices.Clone(d.removed)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("removed %v, want %v (the dind image, used and protected images stay)", got, want)
	}
	if len(d.rmContr) != 0 {
		t.Errorf("containers were removed with the option off: %v", d.rmContr)
	}
	// Build cache: alice's daemon only, never the parent's.
	if len(d.builds) == 0 || slices.ContainsFunc(d.builds, func(h string) bool { return h != "unix://alice" }) {
		t.Errorf("build cache pruned on %v, want only alice's daemon", d.builds)
	}
}

func TestAgeKeepsWhatDockerRefuses(t *testing.T) {
	d := &fakeDocker{
		images:     map[string][]prune.Image{"": {{ID: "sha256:x", Tags: []string{"x:1", "x:2"}}}},
		containers: map[string][]prune.Container{},
		refuse:     map[string]bool{"x:2": true},
	}
	h := newHarness(t, workspace.ModeShared, pruneConfig{After: time.Hour, Interval: time.Hour}, d, nil)
	h.p.sweep(context.Background(), time.Hour)
	h.advance(2 * time.Hour)
	h.p.sweep(context.Background(), time.Hour)
	// The first tag went and the second was refused: no -f to force it.
	if !slices.Equal(d.removed, []string{"/x:1"}) {
		t.Errorf("removed %v", d.removed)
	}
}

func TestAgeSharedWithContainers(t *testing.T) {
	d := &fakeDocker{
		images: map[string][]prune.Image{"": {}},
		containers: map[string][]prune.Container{"": {
			{ID: "old", Name: "/old", Status: "exited", Finished: base.Add(-10 * 24 * time.Hour)},
			{ID: "run", Name: "/run", Status: "running", Created: base.Add(-10 * 24 * time.Hour)},
		}},
	}
	cfg := pruneConfig{After: 7 * 24 * time.Hour, Interval: time.Hour, Containers: true}
	h := newHarness(t, workspace.ModeShared, cfg, d, nil)
	h.p.sweep(context.Background(), time.Hour)
	if !slices.Equal(d.rmContr, []string{"/old"}) {
		t.Errorf("removed containers %v, want only the stopped one", d.rmContr)
	}
	if !slices.Equal(d.builds, []string{""}) {
		t.Errorf("build cache pruned on %v", d.builds)
	}
}

func TestParentNeverLosesContainers(t *testing.T) {
	d := &fakeDocker{
		images: map[string][]prune.Image{"": {}},
		containers: map[string][]prune.Container{"": {
			{ID: "acct", Name: "/rd-dind-alice", Status: "exited", Finished: base.Add(-10 * 24 * time.Hour)},
		}},
	}
	cfg := pruneConfig{After: 7 * 24 * time.Hour, Interval: time.Hour, Containers: true}
	h := newHarness(t, workspace.ModePerAccount, cfg, d, nil)
	h.p.sweep(context.Background(), time.Hour)
	if len(d.rmContr) != 0 || len(d.builds) != 0 {
		t.Errorf("the parent daemon lost containers %v or cache %v", d.rmContr, d.builds)
	}
}

func TestPressure(t *testing.T) {
	var imgs []prune.Image
	for _, id := range []string{"a", "b", "c", "d"} {
		imgs = append(imgs, prune.Image{ID: "sha256:" + id, Tags: []string{id + ":1"}})
	}
	d := &fakeDocker{
		images:     map[string][]prune.Image{"": imgs},
		containers: map[string][]prune.Container{},
		// 95% full; each image frees 5 points, so two images reach 85 and a
		// third reaches 80.
		used: &diskModel{total: 100, used: 95, perImage: 5},
	}
	cfg := pruneConfig{Above: 82, Interval: time.Hour, Floor: time.Hour}
	h := newHarness(t, workspace.ModeShared, cfg, d, nil)

	// Meet the images, then make them differ in age: a oldest, d inside the floor.
	h.p.sweep(context.Background(), time.Hour)
	rec, _ := h.p.record(pruneDaemon{name: pruneShared})
	rec.Seen["sha256:a"] = base.Add(-5 * time.Hour)
	rec.Seen["sha256:b"] = base.Add(-3 * time.Hour)
	rec.Seen["sha256:c"] = base.Add(-2 * time.Hour)
	rec.Seen["sha256:d"] = base.Add(-10 * time.Minute)
	d.removed = nil
	d.used.used = 95

	h.p.sweep(context.Background(), time.Hour)

	// 95 -> 90 -> 85 -> 80: stops once below 82, so c goes and d stays.
	if want := []string{"/a:1", "/b:1", "/c:1"}; !slices.Equal(d.removed, want) {
		t.Errorf("removed %v, want %v", d.removed, want)
	}
	if got := h.metrics["_shared/pressure"]; got != 15 {
		t.Errorf("reclaimed under pressure = %v, want 15", got)
	}
	if len(d.builds) == 0 {
		t.Error("build cache was not tried before images")
	}
}

func TestPressureFloorHolds(t *testing.T) {
	d := &fakeDocker{
		images:     map[string][]prune.Image{"": {{ID: "sha256:new", Tags: []string{"new:1"}}}},
		containers: map[string][]prune.Container{},
		used:       &diskModel{total: 100, used: 95, perImage: 5},
	}
	h := newHarness(t, workspace.ModeShared, pruneConfig{Above: 80, Interval: time.Hour, Floor: time.Hour}, d, nil)
	var warnings []string
	h.p.log = slog.New(slog.NewTextHandler(writerFunc(func(s string) { warnings = append(warnings, s) }), &slog.HandlerOptions{Level: slog.LevelWarn}))

	h.p.sweep(context.Background(), time.Hour)
	h.advance(5 * time.Minute)
	h.p.sweep(context.Background(), time.Hour)

	if len(d.removed) != 0 {
		t.Errorf("an image seen within the floor was removed: %v", d.removed)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "still above") {
		t.Errorf("want one warning per crossing, got %v", warnings)
	}
}

type writerFunc func(string)

func (w writerFunc) Write(p []byte) (int, error) { w(string(p)); return len(p), nil }
