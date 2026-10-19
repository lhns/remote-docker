package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/prune"
	"github.com/lhns/remote-docker/core/workspace"
)

// Pruning of unused images and build cache (ADR 0056).
const (
	// envPruneAfter is how long an image may go unused before it is removed:
	// a Go duration or whole days, 0 or off for never. Build cache unused that
	// long goes too.
	envPruneAfter = "WORKSPACE_PRUNE_AFTER"

	// envPruneAbove is the disk usage of the daemon's data directory above
	// which images and cache are removed regardless of age, such as 80%.
	envPruneAbove = "WORKSPACE_PRUNE_ABOVE"

	// envPruneContainers also removes containers stopped for longer than
	// envPruneAfter. Never on the parent daemon of a daemon per account.
	envPruneContainers = "WORKSPACE_PRUNE_CONTAINERS"

	// envPruneInterval and envPruneFloor are test hooks: how often the age
	// rules run, and how long an image is protected from the pressure rule.
	envPruneInterval = "WORKSPACE_PRUNE_INTERVAL"
	envPruneFloor    = "WORKSPACE_PRUNE_FLOOR"
)

const (
	defaultPruneAfter    = 7 * 24 * time.Hour
	defaultPruneAbove    = 80.0
	defaultPruneInterval = time.Hour
	defaultPruneFloor    = time.Hour

	// pressureInterval is how often the disk is measured, and how long a build
	// cache must be unused to go under pressure.
	pressureInterval = 5 * time.Minute
	pressureCacheAge = time.Hour

	// Names the daemons that are not an account's, in a record's file name and
	// as the account label of a metric. An account name cannot start with an
	// underscore.
	pruneParent = "_parent"
	pruneShared = "_shared"
)

// Why the bytes were reclaimed, as the metric labels it.
const (
	reasonAge      = "age"
	reasonPressure = "pressure"
)

type pruneConfig struct {
	After      time.Duration // 0: no age pruning
	Above      float64       // percent; 0: no pressure pruning
	Containers bool
	Interval   time.Duration
	Floor      time.Duration
}

func (c pruneConfig) enabled() bool { return c.After > 0 || c.Above > 0 }

// pruneSettings reads the WORKSPACE_PRUNE_* variables. Unset is the default;
// anything unusable refuses the start, naming the variable.
func pruneSettings(after, above, containers, interval, floor string) (pruneConfig, error) {
	cfg := pruneConfig{After: defaultPruneAfter, Above: defaultPruneAbove,
		Interval: defaultPruneInterval, Floor: defaultPruneFloor}

	switch after = strings.TrimSpace(after); after {
	case "":
	case "0", "off":
		cfg.After = 0
	default:
		d, err := parseDuration(after)
		if err != nil || d == 0 {
			return cfg, fmt.Errorf("%s: %q is not a duration such as 36h or 7d, or off", envPruneAfter, after)
		}
		cfg.After = d
	}

	switch above = strings.TrimSpace(above); above {
	case "":
	case "0", "off":
		cfg.Above = 0
	default:
		n, err := strconv.ParseFloat(strings.TrimSuffix(above, "%"), 64)
		if err != nil || n <= 0 || n >= 100 {
			return cfg, fmt.Errorf("%s: %q is not a percentage between 0 and 100, such as 80%%, or off", envPruneAbove, above)
		}
		cfg.Above = n
	}

	var err error
	if cfg.Containers, err = envBool(envPruneContainers, containers, false); err != nil {
		return cfg, err
	}
	if interval != "" {
		d, err := parseDuration(interval)
		if err != nil || d == 0 {
			return cfg, fmt.Errorf("%s: %q is not a positive duration", envPruneInterval, interval)
		}
		cfg.Interval = d
	}
	if floor != "" {
		d, err := parseDuration(floor)
		if err != nil {
			return cfg, fmt.Errorf("%s: %q is not a duration", envPruneFloor, floor)
		}
		cfg.Floor = d
	}
	return cfg, nil
}

// pruneDocker is what the pruner asks of a daemon; dockercli.Pruning.
type pruneDocker interface {
	Images(ctx context.Context, host string) ([]prune.Image, error)
	Containers(ctx context.Context, host string) ([]prune.Container, error)
	CreatedImages(ctx context.Context, host string, since, until time.Time) ([]string, error)
	RemoveImage(ctx context.Context, host, ref string) error
	RemoveContainer(ctx context.Context, host, id string) error
	BuildPrune(ctx context.Context, host string, unusedFor time.Duration) error
	RootDir(ctx context.Context, host string) (string, error)
}

// pruneDaemon is one daemon to prune.
type pruneDaemon struct {
	name string // the account, or pruneParent or pruneShared
	host string // a -H value; empty for the default socket

	// imagesOnly: the parent of a daemon per account, whose containers are the
	// accounts' daemons and whose build cache nothing uses.
	imagesOnly bool
}

// pruner removes what a daemon no longer needs, on the rules in ADR 0056. It
// never touches a volume, never forces a removal and never starts a daemon.
type pruner struct {
	cfg      pruneConfig
	targets  daemons.Targets
	accounts func() []string
	docker   pruneDocker
	measure  func(dir string) (diskUsage, error)
	now      func() time.Time
	dir      string // where the records live
	log      *slog.Logger

	// keep names images the parent daemon must keep even when nothing uses
	// them: the image an account's daemon runs, which with no account left is
	// referenced by nothing and would be pulled again.
	keep []string

	// reclaimed counts bytes freed, by account and reason.
	reclaimed func(bytes float64, account, reason string)

	records      map[string]*prune.Record
	rootDir      string
	lastAge      time.Time
	warned       bool // the disk is above the threshold and we have said so
	unmeasurable bool // the disk could not be measured and we have said so
}

// run measures and sweeps until ctx ends. The first sweep is immediate, so a
// record exists from the start.
func (p *pruner) run(ctx context.Context) {
	tick := pressureInterval
	if p.cfg.Interval < tick {
		tick = p.cfg.Interval
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		p.sweep(ctx, tick)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sweep is one tick: look at every daemon, apply the age rules when they are
// due, then the pressure rules.
func (p *pruner) sweep(ctx context.Context, tick time.Duration) {
	now := p.now()
	ds := p.daemons(ctx)

	states := make([]*pruneState, 0, len(ds))
	for _, d := range ds {
		s, err := p.snapshot(ctx, d, true)
		if err != nil {
			p.log.Debug("could not look at a daemon to prune it", "account", d.name, "err", err)
			continue
		}
		states = append(states, s)
	}

	if p.cfg.After > 0 && (p.lastAge.IsZero() || now.Sub(p.lastAge) >= p.cfg.Interval-tick/2) {
		p.lastAge = now
		for _, s := range states {
			if ctx.Err() != nil {
				return
			}
			p.age(ctx, s)
		}
	}
	if p.cfg.Above > 0 && ctx.Err() == nil {
		p.pressure(ctx, ds)
	}
}

// daemons is every daemon to prune. A daemon per account is found with Lookup,
// which never starts one.
func (p *pruner) daemons(ctx context.Context) []pruneDaemon {
	if p.targets.Mode() != workspace.ModePerAccount {
		return []pruneDaemon{{name: pruneShared}}
	}
	out := []pruneDaemon{{name: pruneParent, imagesOnly: true}}
	for _, account := range p.accounts() {
		if t, ok := p.targets.Lookup(ctx, account); ok {
			out = append(out, pruneDaemon{name: account, host: t.Host})
		}
	}
	return out
}

// pruneState is a daemon as the last look found it.
type pruneState struct {
	d          pruneDaemon
	rec        *prune.Record
	images     []prune.Image
	containers []prune.Container
	protected  map[string]bool
}

// snapshot lists a daemon and notes what is in use in its record, which it
// saves. With events it also reads the create events since the record was last
// swept, so a container that came and went in between still counts.
func (p *pruner) snapshot(ctx context.Context, d pruneDaemon, events bool) (*pruneState, error) {
	images, err := p.docker.Images(ctx, d.host)
	if err != nil {
		return nil, err
	}
	containers, err := p.docker.Containers(ctx, d.host)
	if err != nil {
		return nil, err
	}

	rec, path := p.record(d)
	now := p.now()
	var created []string
	if events && !rec.Swept.IsZero() {
		var eerr error
		if created, eerr = p.docker.CreatedImages(ctx, d.host, rec.Swept, now); eerr != nil {
			p.log.Debug("could not read the container events", "account", d.name, "err", eerr)
		}
	}
	rec.Observe(now, images, containers, created)
	if events {
		rec.Swept = now
	}
	if err := rec.Save(path); err != nil {
		p.log.Warn("could not save the record of when images were used", "path", path, "err", err)
	}

	s := &pruneState{d: d, rec: rec, images: images, containers: containers, protected: map[string]bool{}}
	if d.name == pruneParent {
		for _, name := range p.keep {
			if id, ok := prune.Resolve(images, name); ok {
				s.protected[id] = true
			}
		}
	}
	return s, nil
}

// record is a daemon's record, read once and then kept; an unreadable file
// starts the record afresh, which only delays removal.
func (p *pruner) record(d pruneDaemon) (*prune.Record, string) {
	path := filepath.Join(p.dir, d.name+".json")
	if r, ok := p.records[d.name]; ok {
		return r, path
	}
	r, err := prune.Load(path)
	if err != nil {
		p.log.Warn("ignoring an unreadable record of when images were used; every image counts as used now",
			"account", d.name, "err", err)
	}
	if p.records == nil {
		p.records = map[string]*prune.Record{}
	}
	p.records[d.name] = r
	return r, path
}

// free is the free space on the data directory, ok false when it cannot be
// measured.
func (p *pruner) free(ctx context.Context) (uint64, bool) {
	u, ok := p.usage(ctx)
	return u.Avail, ok
}

// usage measures the parent daemon's data directory, where every account's
// graph volume sits. One warning when it cannot be.
func (p *pruner) usage(ctx context.Context) (diskUsage, bool) {
	if p.rootDir == "" {
		dir, err := p.docker.RootDir(ctx, "")
		if err != nil {
			p.cannotMeasure(err)
			return diskUsage{}, false
		}
		p.rootDir = dir
	}
	u, err := p.measure(p.rootDir)
	if err != nil {
		p.cannotMeasure(err)
		return diskUsage{}, false
	}
	p.unmeasurable = false
	return u, true
}

func (p *pruner) cannotMeasure(err error) {
	if !p.unmeasurable {
		p.log.Warn("cannot measure the disk the daemons use; pruning by usage is skipped and reclaimed space is not counted", "err", err)
	}
	p.unmeasurable = true
}

// age applies the age rules to one daemon.
func (p *pruner) age(ctx context.Context, s *pruneState) {
	before, measured := p.free(ctx)
	now := p.now()
	var images, containers int

	if p.cfg.Containers && !s.d.imagesOnly {
		for _, c := range prune.AgeContainers(s.containers, p.cfg.After, now) {
			if err := p.docker.RemoveContainer(ctx, s.d.host, c.ID); err != nil {
				p.log.Debug("could not remove a stopped container", "account", s.d.name, "container", c.ID, "err", err)
				continue
			}
			containers++
			s.containers = slices.DeleteFunc(s.containers, func(o prune.Container) bool { return o.ID == c.ID })
		}
	}
	for _, c := range prune.AgeImages(s.rec, s.images, s.containers, s.protected, p.cfg.After, now) {
		if p.removeImage(ctx, s.d, c.Image) {
			images++
		}
	}
	if !s.d.imagesOnly {
		if err := p.docker.BuildPrune(ctx, s.d.host, p.cfg.After); err != nil {
			p.log.Debug("could not prune the build cache", "account", s.d.name, "err", err)
		}
	}

	var freed uint64
	if after, ok := p.free(ctx); ok && measured && after > before {
		freed = after - before
		p.reclaimed(float64(freed), s.d.name, reasonAge)
	}
	if images > 0 || containers > 0 || freed > 0 {
		p.log.Info("pruned what had gone unused", "account", s.d.name, "images", images,
			"containers", containers, "reclaimed", freed, "unused-for", p.cfg.After)
	}
}

// removeImage removes an image by each of its references, never forced, so the
// daemon refuses one a container still uses. An image counts as removed only
// when every reference went.
func (p *pruner) removeImage(ctx context.Context, d pruneDaemon, img prune.Image) bool {
	for _, ref := range img.Refs() {
		if err := p.docker.RemoveImage(ctx, d.host, ref); err != nil {
			p.log.Debug("the daemon kept an image", "account", d.name, "image", ref, "err", err)
			return false
		}
	}
	return true
}

// pressure frees space when the data directory is fuller than cfg.Above: first
// build cache unused for an hour, then unused images oldest first, none that
// was in use within the floor, re-measuring after each until it is below.
func (p *pruner) pressure(ctx context.Context, ds []pruneDaemon) {
	u, ok := p.usage(ctx)
	if !ok {
		return
	}
	if u.Percent() <= p.cfg.Above {
		p.warned = false
		return
	}
	if !p.warned {
		p.log.Info("the disk is above the threshold; pruning", "usage", pct(u.Percent()), "threshold", pct(p.cfg.Above))
	}

	type tally struct {
		images int
		bytes  uint64
	}
	done := map[string]*tally{}
	account := func(name string) *tally {
		if done[name] == nil {
			done[name] = &tally{}
		}
		return done[name]
	}
	prev := u.Avail
	credit := func(name string) diskUsage {
		now, ok := p.usage(ctx)
		if ok && now.Avail > prev {
			account(name).bytes += now.Avail - prev
			p.reclaimed(float64(now.Avail-prev), name, reasonPressure)
		}
		if ok {
			prev = now.Avail
		}
		return now
	}

	below := false
	for _, d := range ds {
		if d.imagesOnly {
			continue
		}
		if err := p.docker.BuildPrune(ctx, d.host, pressureCacheAge); err != nil {
			p.log.Debug("could not prune the build cache", "account", d.name, "err", err)
		}
		if now := credit(d.name); now.Percent() <= p.cfg.Above {
			below = true
			break
		}
	}

	if !below {
		type pick struct {
			s *pruneState
			c prune.Candidate
		}
		var picks []pick
		now := p.now()
		for _, d := range ds {
			s, err := p.snapshot(ctx, d, false)
			if err != nil {
				continue
			}
			for _, c := range prune.PressureImages(s.rec, s.images, s.containers, s.protected, p.cfg.Floor, now) {
				picks = append(picks, pick{s, c})
			}
		}
		slices.SortStableFunc(picks, func(a, b pick) int { return a.c.Seen.Compare(b.c.Seen) })

		for _, pk := range picks {
			if ctx.Err() != nil {
				return
			}
			if !p.removeImage(ctx, pk.s.d, pk.c.Image) {
				continue
			}
			account(pk.s.d.name).images++
			if now := credit(pk.s.d.name); now.Percent() <= p.cfg.Above {
				below = true
				break
			}
		}
	}

	for _, name := range slices.Sorted(maps.Keys(done)) {
		if t := done[name]; t.images > 0 || t.bytes > 0 {
			p.log.Info("pruned under disk pressure", "account", name, "images", t.images, "reclaimed", t.bytes)
		}
	}
	if !below && !p.warned {
		p.warned = true
		cur, _ := p.usage(ctx)
		p.log.Warn("the disk is still above the threshold and nothing is left to prune; "+
			"images in use, used in the last hour, or the daemons' volumes hold the rest",
			"usage", pct(cur.Percent()), "threshold", pct(p.cfg.Above))
	}
	if below {
		p.warned = false
	}
}

// diskUsage is a filesystem's fullness as df reports it: used against used
// plus what an unprivileged writer can still have.
type diskUsage struct{ Used, Avail uint64 }

// Percent is how full the filesystem is.
func (u diskUsage) Percent() float64 {
	if u.Used+u.Avail == 0 {
		return 0
	}
	return 100 * float64(u.Used) / float64(u.Used+u.Avail)
}

func pct(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) + "%" }
