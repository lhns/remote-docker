package accounts

// Which port serves which of an account's machines.
//
// The uid decides an account's FIRST port (ADR 0003) and cannot decide more:
// the formula tiles the space one port per uid, so there is no second slot to
// derive. An account used from two machines therefore needs an allocation, and
// the agent is the only thing that can make one, since it is the only thing
// that binds. Stability is kept, which is what ADR 0003 was for: a port is
// remembered against the CLIENT (ADR 0029), so the same machine reconnecting
// is offered the same port and the volumes it created still mount.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/lhns/remote-docker/core/workspace"
)

// Ports remembers which port each of an account's machines was given.
type Ports struct {
	// Dir is where the record lives, beside uidmap.
	Dir string

	// Mapping supplies the account's first port, which is still derived.
	Mapping workspace.Mapping

	// Reserved reports whether a uid belongs to an account that exists, so an
	// allocation does not take a port somebody else derives. Nil skips the
	// check, which is right for a test and wrong for a workspace.
	Reserved func(uid int) bool

	// Preferred reports the port a machine's existing state already expects:
	// 0 when it has none, and an error when the question could not be put.
	//
	// This file is a CACHE; the durable record of a port is the volumes built
	// for it, since a volume keeps its port forever and cannot be re-pointed
	// (ADR 0032). So a machine this file has forgotten can still be given the
	// port its volumes need rather than one that makes them all unmountable.
	//
	// A func because answering means asking Docker, and nothing in this module
	// may know Docker exists (ADR 0021). Nil skips the question, which is what
	// a workspace with no daemon of its own wants.
	Preferred func(account, client string) (int, error)

	mu       sync.Mutex
	loaded   bool
	assigned map[assignment]entry
	next     uint64
}

// entry is one assignment. A run's (ADR 0050) is ephemeral: held in memory,
// never written to clientports, and freed by the token it was given.
type entry struct {
	port      int
	ephemeral bool
	token     uint64
}

// assignment names one machine of one account.
type assignment struct {
	account string
	client  string
}

// path is where assignments are persisted, in the same one-line-per-entry shape
// as uidmap so an operator can read both with `cat`.
func (p *Ports) path() string { return filepath.Join(p.Dir, "clientports") }

// For returns the port this account's machine should use, allocating one if
// this is a machine the workspace has not seen.
//
// The account's own uid-derived port goes to whichever machine asks first,
// which keeps every existing deployment on the port it already uses: a
// workspace reached from one machine never allocates anything, and its volumes
// and its `clientports` file both stay as they were.
//
// Preferred runs with NO lock held: it can boot a cold dind under a 90s budget
// (agent/cmd/remote-dockerd/serve.go), and Owns takes the same mutex on every
// account's tcpip-forward check. Its answer therefore crosses the lock
// boundary as a HINT, and decide re-validates it, which is what keeps ADR
// 0032's atomicity: taken, free, allocate and the assignment are one step.
func (p *Ports) For(account string, uid int, client string) (int, error) {
	base, err := p.Mapping.PortForUID(uid)
	if err != nil {
		return 0, err
	}
	// A client we cannot name gets the account's base port, which is what
	// every session did before machines were named.
	if client == "" {
		return base, nil
	}

	key := assignment{account: account, client: client}
	e, known, err := p.entry(key)
	if err != nil {
		return 0, err
	}
	if known {
		return e.port, nil
	}

	// What this machine's volumes already expect. Only reached when the record
	// does not know this machine: an entry that exists was persisted
	// deliberately and is the answer.
	//
	// A question that could not be put is refused rather than answered with the
	// derived port, which another machine may hold and which this machine's
	// volumes were not built for (ADR 0032).
	want := 0
	if p.Preferred != nil {
		if want, err = p.Preferred(account, client); err != nil {
			return 0, fmt.Errorf("accounts: cannot tell which port %s's machine needs: %w", account, err)
		}
	}

	e, err = p.decide(key, base, want, false)
	return e.port, err
}

// ForRun returns an ephemeral run's port, allocating one from the top of the
// range if it has none, and the token that frees it. A run never takes the
// account's derived port, which belongs to its machines.
func (p *Ports) ForRun(account, client string) (int, uint64, error) {
	key := assignment{account: account, client: client}
	if e, known, err := p.entry(key); err != nil || known {
		return e.port, e.token, err
	}
	// After an agent restart a run's volumes still name its port (ADR 0032).
	want := 0
	if p.Preferred != nil {
		var err error
		if want, err = p.Preferred(account, client); err != nil {
			return 0, 0, fmt.Errorf("accounts: cannot tell which port %s's run needs: %w", account, err)
		}
	}
	e, err := p.decide(key, 0, want, true)
	return e.port, e.token, err
}

// Hold gives a run back the port it had before an agent restart, unless
// somebody else has it or a run may not take it.
func (p *Ports) Hold(account, client string, port int) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(); err != nil {
		return 0, false
	}
	key := assignment{account: account, client: client}
	if e, ok := p.assigned[key]; ok {
		if e.ephemeral && e.port == port {
			return e.token, true
		}
		return 0, false
	}
	for _, e := range p.assigned {
		if e.port == port {
			return 0, false
		}
	}
	if !p.free(port, p.memoReserved()) {
		return 0, false
	}
	return p.assign(key, port, true).token, true
}

// Free gives a run's port up, once nothing on the workspace names it any more
// (ADR 0032). Only the token it was given with frees it, so a late call for a
// run since given a port again frees nothing (ADR 0028).
func (p *Ports) Free(account, client string, token uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := assignment{account: account, client: client}
	if e, ok := p.assigned[key]; ok && e.ephemeral && token != 0 && e.token == token {
		delete(p.assigned, key)
		return true
	}
	return false
}

// Lookup returns the port already recorded for this client, allocating
// nothing: an ephemeral run is given one only when it binds (ADR 0050).
func (p *Ports) Lookup(account, client string) (port int, known bool, err error) {
	e, known, err := p.entry(assignment{account: account, client: client})
	return e.port, known, err
}

// entry answers for a client the record already knows, which is every
// ordinary connect.
func (p *Ports) entry(key assignment) (entry, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.load(); err != nil {
		return entry{}, false, err
	}
	e, known := p.assigned[key]
	return e, known, nil
}

// decide chooses this machine's port and records it, in one hold.
//
// want is the hint Preferred gave outside the lock. The new risk that comes
// with computing it there: a machine whose want was handed to somebody else in
// the window is given a different port with nothing said, and its volumes then
// cannot mount. It needs a lost record AND both machines re-deriving the same
// base, and Ports has no logger to say so with.
//
// An ephemeral decision skips base and is not saved.
func (p *Ports) decide(key assignment, base, want int, ephemeral bool) (entry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.load(); err != nil {
		return entry{}, err
	}

	// Another session of this machine may have decided while we were asking
	// Preferred. Its answer is the one on record, and one machine must have one
	// port: a second would leave the volumes built for the first unmountable.
	if e, ok := p.assigned[key]; ok {
		return e, nil
	}

	// One walk of the record, since this machine is not in it: everything
	// assigned belongs to somebody else.
	taken := make(map[int]bool, len(p.assigned))
	for _, e := range p.assigned {
		taken[e.port] = true
	}

	// Reserved is a full account listing per call
	// (agent/cmd/remote-dockerd/serve.go), and free(want) and allocate can meet
	// the same port, so its answers are remembered for this decision. Inside
	// the lock, in ADR 0032's atomic step.
	reserved := p.memoReserved()

	port := 0
	if want != 0 && !taken[want] && p.free(want, reserved) {
		port = want
	}

	if port == 0 && !ephemeral && !taken[base] {
		port = base
	}
	if port == 0 {
		if port = p.allocate(taken, reserved); port == 0 {
			return entry{}, fmt.Errorf("accounts: no free reverse-tunnel port left for %s", key.account)
		}
	}

	e := p.assign(key, port, ephemeral)

	// A record that cannot be written is not fatal: the session works, and the
	// cost is that this machine may be given a different port next time, which
	// costs it its volumes rather than its connection.
	if !ephemeral {
		_ = p.save()
	}
	return e, nil
}

// assign records a port. The caller holds the lock.
func (p *Ports) assign(key assignment, port int, ephemeral bool) entry {
	p.next++
	e := entry{port: port, ephemeral: ephemeral, token: p.next}
	p.assigned[key] = e
	return e
}

// memoReserved wraps Reserved so one decision asks about a uid at most once.
// A nil Reserved answers false, which is the skip its field documents.
func (p *Ports) memoReserved() func(uid int) bool {
	if p.Reserved == nil {
		return func(int) bool { return false }
	}
	seen := map[int]bool{}
	return func(uid int) bool {
		answer, asked := seen[uid]
		if !asked {
			answer = p.Reserved(uid)
			seen[uid] = answer
		}
		return answer
	}
}

// free reports whether a port may be handed to somebody who does not derive it.
//
// The same rule allocate applies: in range, and not derived by an account that
// EXISTS, because that account is entitled to its own port whether or not it
// has ever connected.
func (p *Ports) free(port int, reserved func(int) bool) bool {
	if port < p.Mapping.PortBase || port > workspace.MaxPort {
		return false
	}
	uid, err := p.Mapping.UIDForPort(port)
	return err != nil || !reserved(uid)
}

// allocate picks a free port, counting DOWN from the top of the range.
//
// Down, because the derived ports grow UP from PortBase with the uid and the
// mapping is a bijection over the whole range: every port above the base is
// spoken for by some hypothetical uid, so there is no gap to allocate from.
// Starting at the far end means an allocated port only meets a derived one on a
// workspace with tens of thousands of accounts, and free catches it even then.
//
// Deterministic rather than random, so an operator can predict the range and
// a rerun of the same sequence produces the same file.
func (p *Ports) allocate(taken map[int]bool, reserved func(int) bool) int {
	for port := workspace.MaxPort; port >= p.Mapping.PortBase; port-- {
		if taken[port] {
			continue
		}
		if uid, err := p.Mapping.UIDForPort(port); err == nil && reserved(uid) {
			continue
		}
		return port
	}
	return 0
}

// Owns reports whether an account has been given this port on some machine,
// which is what the forward policy asks instead of doing the arithmetic
// itself.
func (p *Ports) Owns(account string, uid, port int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(); err != nil {
		return false
	}

	// The record first, and it decides. The derived port belongs to this
	// account only while nobody else has been given it: once it is assigned,
	// answering from the formula would say two accounts own one port.
	for k, e := range p.assigned {
		if e.port == port {
			return k.account == account
		}
	}

	base, err := p.Mapping.PortForUID(uid)
	return err == nil && port == base
}

// load reads the record once it has been read successfully. A failed read is
// tried again next time rather than remembered as an empty record, which the
// next save would write back over everybody's assignments.
func (p *Ports) load() error {
	if p.loaded {
		return nil
	}
	p.assigned = map[assignment]entry{}

	err := ReadRecord(p.path(), func(line string) {
		// account:client:port
		parts := strings.Split(line, ":")
		if len(parts) != 3 {
			return
		}
		port, err := strconv.Atoi(parts[2])
		if err != nil || port < 1 || port > workspace.MaxPort {
			return
		}
		p.assigned[assignment{account: parts[0], client: parts[1]}] = entry{port: port}
	})
	if err != nil {
		return fmt.Errorf("accounts: reading clientports: %w", err)
	}
	p.loaded = true
	return nil
}

// save replaces the record.
//
// Sorted, because a map's order is not one: the file is read by people as well
// as by this, and a record that shuffles on every write hides what changed.
func (p *Ports) save() error {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return err
	}

	lines := make([]string, 0, len(p.assigned))
	for k, e := range p.assigned {
		if !e.ephemeral {
			lines = append(lines, fmt.Sprintf("%s:%s:%d", k.account, k.client, e.port))
		}
	}
	sort.Strings(lines)
	return WriteRecord(p.path(), lines, 0o600)
}
