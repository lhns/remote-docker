// Package ephemeral keeps the runs of ephemeral accounts (ADR 0050): which
// are live, which are in their grace period, and which are being cleaned up.
//
// A run is keyed on its derived client id. The run id itself is never stored:
// with the key it is what takes a run over.
package ephemeral

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core/logx"
)

const (
	// DefaultMax is how many runs an account may have, in any state.
	DefaultMax = 8

	// DefaultGrace is how long a run outlives its last connection. Longer than
	// the ~60s it takes to notice a dead peer, over TCP or a WebSocket.
	DefaultGrace = 2 * time.Minute
)

// State is where a run is in its life.
type State int

const (
	// Live runs have a connection.
	Live State = iota
	// Grace runs have none, and are reattached by the next one.
	Grace
	// Cleaning runs have expired and refuse connections until they are gone.
	Cleaning
)

func (s State) String() string {
	switch s {
	case Live:
		return "live"
	case Grace:
		return "grace"
	case Cleaning:
		return "cleaning"
	}
	return "state" + strconv.Itoa(int(s))
}

// Ports allocates and frees a run's port; *accounts.Ports.
type Ports interface {
	ForRun(account, client string) (int, uint64, error)
	Hold(account, client string, port int) (uint64, bool)
	Free(account, client string, token uint64) bool
}

// Registry holds every ephemeral account's runs.
type Registry struct {
	Ports Ports

	// Max is the runs per account, live, grace or cleaning; 0 is DefaultMax.
	Max int

	// Grace is how long a run outlives its last connection; 0 is DefaultGrace.
	Grace time.Duration

	// Cleanup removes what an expired run left on the workspace. An error keeps
	// the run, and its port, for the next sweep: the port is never freed while
	// something still names it (ADR 0032). Nil has nothing to remove.
	Cleanup func(ctx context.Context, account, client string) error

	// Dir holds the record of runs, so an agent restart can reattach them.
	// Empty keeps none.
	Dir string

	Log *slog.Logger

	// now is the clock; nil is time.Now.
	now func() time.Time

	mu   sync.Mutex
	runs map[key]*run

	// sweeping keeps two sweeps from cleaning one run.
	sweeping sync.Mutex
}

type key struct{ account, client string }

type run struct {
	state    State
	conns    int
	port     int
	token    uint64
	lastSeen time.Time

	// allocating counts Port calls waiting on the allocator, which keep a
	// portless run from ending under them.
	allocating int
}

// Attach counts a connection that named a run, creating the run if it is new,
// and returns what to call when that connection ends. A new run beyond Max is
// refused, and so is one being cleaned up.
func (r *Registry) Attach(account, client string) (release func(), err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	k := key{account, client}
	ru := r.runs[k]
	switch {
	case ru == nil:
		if n := r.count(account); n >= r.max() {
			return nil, fmt.Errorf("ephemeral: account %s has %d clients\n"+
				"  fix: raise WORKSPACE_EPHEMERAL_MAX_CLIENTS or wait", account, n)
		}
		ru = &run{}
		if r.runs == nil {
			r.runs = map[key]*run{}
		}
		r.runs[k] = ru
	case ru.state == Cleaning:
		return nil, fmt.Errorf("ephemeral: run %s of account %s has expired and is being cleaned up\n"+
			"  fix: retry in a moment", client, account)
	case ru.state == Grace:
		r.log().Info("a run reattached", "account", account, "client", client, "port", ru.port)
	}
	ru.state = Live
	ru.conns++
	ru.lastSeen = r.clock()
	r.save()

	var once sync.Once
	return func() { once.Do(func() { r.detach(k, ru) }) }, nil
}

// detach counts a connection ending. The last one starts the grace period, or
// ends a run that never bound a port, since nothing can name it.
func (r *Registry) detach(k key, ru *run) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs[k] != ru {
		return
	}
	ru.conns--
	ru.lastSeen = r.clock()
	if ru.conns > 0 {
		return
	}
	if ru.port == 0 && ru.allocating == 0 {
		delete(r.runs, k)
	} else {
		ru.state = Grace
		r.log().Info("a run lost its last connection", "account", k.account, "client", k.client, "grace", r.grace())
	}
	r.save()
}

// Port returns an attached run's port, allocating it on first use.
func (r *Registry) Port(account, client string) (int, error) {
	k := key{account, client}
	r.mu.Lock()
	ru := r.runs[k]
	if ru == nil || ru.state != Live {
		r.mu.Unlock()
		return 0, fmt.Errorf("ephemeral: run %s of account %s is not attached", client, account)
	}
	if ru.port != 0 {
		defer r.mu.Unlock()
		return ru.port, nil
	}
	ru.allocating++
	r.mu.Unlock()

	// Unlocked: the allocator may ask a daemon which port the run's volumes
	// name, which can take a cold boot.
	port, token, err := r.Ports.ForRun(account, client)

	r.mu.Lock()
	defer r.mu.Unlock()
	ru.allocating--
	switch {
	case r.runs[k] != ru || ru.state == Cleaning:
		// Expired while it waited.
		if err == nil {
			r.Ports.Free(account, client, token)
		}
		return 0, fmt.Errorf("ephemeral: run %s of account %s expired", client, account)
	case err != nil:
		if ru.conns == 0 && ru.port == 0 && ru.allocating == 0 {
			delete(r.runs, k)
		}
		return 0, err
	case ru.port == 0:
		ru.port, ru.token = port, token
		r.save()
	}
	return ru.port, nil
}

// Sweep expires every run whose grace has run out, and cleans every expired
// run: Cleanup, then the port.
func (r *Registry) Sweep(ctx context.Context) {
	r.sweeping.Lock()
	defer r.sweeping.Unlock()

	type due struct {
		k     key
		ru    *run
		port  int
		token uint64
	}
	var expired []due
	r.mu.Lock()
	now := r.clock()
	for k, ru := range r.runs {
		if ru.state == Grace && now.Sub(ru.lastSeen) >= r.grace() {
			ru.state = Cleaning
			r.log().Info("a run expired", "account", k.account, "client", k.client, "port", ru.port)
		}
		if ru.state == Cleaning {
			expired = append(expired, due{k, ru, ru.port, ru.token})
		}
	}
	r.save()
	r.mu.Unlock()

	for _, d := range expired {
		if r.Cleanup != nil {
			if err := r.Cleanup(ctx, d.k.account, d.k.client); err != nil {
				r.log().Warn("keeping an expired run", "account", d.k.account, "client", d.k.client, "err", err)
				continue
			}
		}
		r.Ports.Free(d.k.account, d.k.client, d.token)

		r.mu.Lock()
		if r.runs[d.k] == d.ru {
			delete(r.runs, d.k)
		}
		r.save()
		r.mu.Unlock()
		r.log().Info("a run is gone", "account", d.k.account, "client", d.k.client, "port", d.port)
	}
}

// Run sweeps until ctx ends.
func (r *Registry) Run(ctx context.Context) {
	every := max(r.grace()/4, time.Second)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.Sweep(ctx)
		}
	}
}

// count is an account's runs, in any state. The caller holds mu.
func (r *Registry) count(account string) int {
	n := 0
	for k := range r.runs {
		if k.account == account {
			n++
		}
	}
	return n
}

func (r *Registry) max() int {
	if r.Max > 0 {
		return r.Max
	}
	return DefaultMax
}

func (r *Registry) grace() time.Duration {
	if r.Grace > 0 {
		return r.Grace
	}
	return DefaultGrace
}

func (r *Registry) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Registry) log() *slog.Logger { return logx.Or(r.Log) }

// The record: account:client:port:state:last-seen, one run per line, readable
// with `cat`. A cache: a run's volumes name its port anyway (ADR 0032).

func (r *Registry) path() string { return filepath.Join(r.Dir, "ephemeral-runs") }

// save writes the record. The caller holds mu. Only runs with a port are
// written, because only they leave anything to come back to.
func (r *Registry) save() {
	if r.Dir == "" {
		return
	}
	var lines []string
	for k, ru := range r.runs {
		if ru.port != 0 {
			lines = append(lines, fmt.Sprintf("%s:%s:%d:%s:%d",
				k.account, k.client, ru.port, ru.state, ru.lastSeen.Unix()))
		}
	}
	sort.Strings(lines)
	if err := accounts.WriteRecord(r.path(), lines, 0o600); err != nil {
		r.log().Warn("could not record the ephemeral runs", "err", err)
	}
}

// Restore reads the record left by the previous agent, before serving. Every
// run has lost its connections: a live one starts its grace now, one already
// in grace keeps its deadline, and the next sweep cleans what has expired.
func (r *Registry) Restore() error {
	if r.Dir == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = map[key]*run{}
	}

	now := r.clock()
	err := accounts.ReadRecord(r.path(), func(line string) {
		f := strings.Split(line, ":")
		if len(f) != 5 {
			return
		}
		port, err := strconv.Atoi(f[2])
		if err != nil {
			return
		}
		secs, err := strconv.ParseInt(f[4], 10, 64)
		if err != nil {
			return
		}
		ru := &run{port: port, state: Grace, lastSeen: time.Unix(secs, 0)}
		switch f[3] {
		case Live.String():
			ru.lastSeen = now
		case Cleaning.String():
			ru.state = Cleaning
		}

		token, ok := r.Ports.Hold(f[0], f[1], port)
		if !ok {
			// Somebody has the port now, so the run cannot come back on it.
			r.log().Warn("a run's port is taken; expiring it", "account", f[0], "client", f[1], "port", port)
			ru.state = Cleaning
		}
		ru.token = token
		r.runs[key{f[0], f[1]}] = ru
	})
	if err != nil {
		return fmt.Errorf("ephemeral: reading %s: %w", r.path(), err)
	}
	if len(r.runs) > 0 {
		r.log().Info("restored ephemeral runs", "count", len(r.runs))
	}
	r.save()
	return nil
}
