package ephemeral

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/lhns/remote-docker/agent/internal/dockercli"
	"github.com/lhns/remote-docker/agent/internal/metrics"
)

func TestRegistryCountsRefusalsAndSweeps(t *testing.T) {
	var reg metrics.Registry
	r, _, c := newRegistry(t)
	r.Refused = reg.Counter("refused_total", "", "reason")
	r.Sweeps = reg.Counter("sweeps_total", "")
	r.SweepSeconds = reg.Histogram("sweep_seconds", "", []float64{1})
	r.Cleanup = func(context.Context, string, string) error { return errors.New("kept") }

	_, a := hostRun(t, r, "0123abcd")
	_, b := hostRun(t, r, "4567cdef")
	if _, err := r.Attach("alice", "89abcdef"); err == nil {
		t.Fatal("a third run was attached")
	}
	a()
	c.advance(time.Minute)
	r.Sweep(t.Context()) // 0123abcd fails its cleanup and stays cleaning
	if _, err := r.Attach("alice", "0123abcd"); err == nil {
		t.Fatal("a run being cleaned was attached")
	}
	b()

	if v := r.Refused.Value(RefusedLimit); v != 1 {
		t.Errorf("refused{limit} = %v, want 1", v)
	}
	if v := r.Refused.Value(RefusedCleaning); v != 1 {
		t.Errorf("refused{cleaning} = %v, want 1", v)
	}
	if v := r.Sweeps.Value(); v != 1 {
		t.Errorf("sweeps = %v, want 1", v)
	}
	if n := r.SweepSeconds.Count(); n != 1 {
		t.Errorf("sweep durations observed = %d, want 1", n)
	}
}

func TestCensus(t *testing.T) {
	r, _, _ := newRegistry(t)
	r.Max = 4
	hostRun(t, r, "0123abcd")
	_, release := hostRun(t, r, "4567cdef")
	release()
	portless, err := r.Attach("alice", "89abcdef")
	if err != nil {
		t.Fatal(err)
	}
	defer portless()

	runs, ports := r.Census()
	want := map[Group]int{{"alice", Live}: 2, {"alice", Grace}: 1}
	if !maps.Equal(runs, want) || ports != 2 {
		t.Errorf("census = %v, %d ports; want %v, 2 ports", runs, ports, want)
	}
}

func TestCleanerCountsWhatItRemovesAndKeeps(t *testing.T) {
	var reg metrics.Registry
	d := &fakeDaemon{
		containers: map[string][]string{runID + "-c1": nil, otherID + "-c2": {"rd-" + runID + "-held"}},
		networks:   []string{"proj-" + runID + "_default"},
		volumes: []dockercli.Volume{
			volume("rd-"+runID+"-free", "alice", runID, true),
			volume("rd-"+runID+"-held", "alice", runID, true),
			volume("rd-"+runID+"-cache", "alice", runID, true),
		},
	}
	c := newCleaner(d, "rd-"+runID+"-cache")
	c.Containers = true
	c.Removed = reg.Counter("removed_total", "", "kind")
	c.Kept = reg.Counter("kept_total", "", "kind", "reason")
	_ = c.Clean(t.Context(), "alice", runID)

	for _, tc := range []struct {
		c      *metrics.Counter
		labels []string
		want   float64
	}{
		{c.Removed, []string{KindContainer}, 1},
		{c.Removed, []string{KindNetwork}, 1},
		{c.Removed, []string{KindVolume}, 1},
		{c.Kept, []string{KindVolume, KeptInUse}, 1},
		{c.Kept, []string{KindVolume, KeptMounted}, 1},
	} {
		if v := tc.c.Value(tc.labels...); v != tc.want {
			t.Errorf("%v = %v, want %v", tc.labels, v, tc.want)
		}
	}

	d = &fakeDaemon{
		containers: map[string][]string{runID + "-c1": nil},
		networks:   []string{"proj-" + runID + "_default"},
		failNet:    errors.New("has active endpoints"),
	}
	c.Docker, c.Unions = d, &fakeUnions{d: d}
	_ = c.Clean(t.Context(), "alice", runID)
	if v := c.Kept.Value(KindNetwork, KeptError); v != 1 {
		t.Errorf("kept{network,error} = %v, want 1", v)
	}
}
