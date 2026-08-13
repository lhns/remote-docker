package accounts

// Which port serves which of an account's machines.
//
// The uid decides an account's FIRST port (ADR 0003) and cannot decide any
// more than that: the formula tiles the space one port per uid, so there is no
// second slot to derive. An account used from two machines therefore needs an
// allocation, and the agent is the only thing that can make one, since it is
// the only thing that binds.
//
// Stability is what ADR 0003 says actually mattered, and it is kept: a port is
// remembered against the CLIENT (ADR 0029), so the same machine reconnecting is
// offered the same port and the volumes it created still mount. What is given
// up is the property that no coordination is needed, which the agent pays for
// with this file.

import (
	"bufio"
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

	// Preferred reports the port a machine's existing state already expects,
	// and 0 when there is none or it cannot be asked.
	//
	// This file is a CACHE. The durable record of a port is the volumes that
	// were built for it, because a volume keeps the port it was created with
	// forever and cannot be re-pointed. So a machine this file has forgotten
	// can still be given the port its volumes need, instead of a new one that
	// makes every one of them unmountable.
	//
	// A func because finding that out means asking Docker, and nothing in this
	// module may know Docker exists (ADR 0031). Nil skips the question, which
	// is what a workspace with no daemon of its own wants.
	Preferred func(account, client string) int

	mu       sync.Mutex
	loaded   bool
	assigned map[assignment]int
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

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.load(); err != nil {
		return 0, err
	}

	key := assignment{account: account, client: client}
	if port, ok := p.assigned[key]; ok {
		return port, nil
	}

	// One walk of the record, since this machine is not in it: everything
	// assigned belongs to somebody else.
	taken := make(map[int]bool, len(p.assigned))
	for _, v := range p.assigned {
		taken[v] = true
	}

	// What this machine's volumes already expect, before anything is chosen for
	// it. Only reached when the record does not know this machine: an entry
	// that exists was persisted deliberately and is the answer.
	port := 0
	if want := p.preferred(account, client); want != 0 && !taken[want] && p.free(want) {
		port = want
	}

	if port == 0 {
		port = base
		if taken[base] {
			if port = p.allocate(taken); port == 0 {
				return 0, fmt.Errorf("accounts: no free reverse-tunnel port left for %s", account)
			}
		}
	}

	p.assigned[key] = port

	// A record that cannot be written is not fatal: the session works, and the
	// cost is that this machine may be given a different port next time, which
	// costs it its volumes rather than its connection.
	_ = p.save()
	return port, nil
}

// preferred asks what this machine's existing state expects, and answers 0
// when nothing does.
//
// Never fatal and never retried. A daemon that is slow, absent or broken means
// only that the machine gets a port chosen the way it always was, which is a
// working session with volumes to rebuild rather than no session at all.
func (p *Ports) preferred(account, client string) int {
	if p.Preferred == nil {
		return 0
	}
	return p.Preferred(account, client)
}

// free reports whether a port may be handed to somebody who does not derive it.
//
// The same rule allocate applies: in range, and not derived by an account that
// EXISTS, because that account is entitled to its own port whether or not it
// has ever connected.
func (p *Ports) free(port int) bool {
	if port < p.Mapping.PortBase || port > workspace.MaxPort {
		return false
	}
	if p.Reserved == nil {
		return true
	}
	uid, err := p.Mapping.UIDForPort(port)
	return err != nil || !p.Reserved(uid)
}

// allocate picks a free port, counting DOWN from the top of the range.
//
// Down, because the derived ports grow UP from PortBase with the uid, and the
// mapping is a bijection over the whole range: every port above the base is
// spoken for by some hypothetical uid, so there is no gap to allocate from.
// Starting at the far end means an allocated port only meets a derived one
// once a workspace has tens of thousands of accounts, and Reserved catches it
// even then.
//
// Deterministic rather than random, so an operator can predict the range and
// a rerun of the same sequence produces the same file.
func (p *Ports) allocate(taken map[int]bool) int {
	for port := workspace.MaxPort; port >= p.Mapping.PortBase; port-- {
		if taken[port] {
			continue
		}
		// Skip a port an account that EXISTS derives, because that account is
		// entitled to it whether or not it has ever connected. Handing it out
		// would work until they did, and then take a working tunnel away from
		// somebody.
		if p.Reserved != nil {
			if uid, err := p.Mapping.UIDForPort(port); err == nil && p.Reserved(uid) {
				continue
			}
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
	for k, v := range p.assigned {
		if v == port {
			return k.account == account
		}
	}

	base, err := p.Mapping.PortForUID(uid)
	return err == nil && port == base
}

// load reads the record once.
func (p *Ports) load() error {
	if p.loaded {
		return nil
	}
	p.assigned = map[assignment]int{}
	p.loaded = true

	f, err := os.Open(p.path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("accounts: reading clientports: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// account:client:port
		parts := strings.Split(line, ":")
		if len(parts) != 3 {
			continue
		}
		port, err := strconv.Atoi(parts[2])
		if err != nil || port < 1 || port > workspace.MaxPort {
			continue
		}
		p.assigned[assignment{account: parts[0], client: parts[1]}] = port
	}
	return scanner.Err()
}

// save replaces the record atomically.
//
// Sorted, because a map's order is not one: the file is read by people as well
// as by this, and a record that shuffles on every write hides what changed.
func (p *Ports) save() error {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return err
	}

	lines := make([]string, 0, len(p.assigned))
	for k, v := range p.assigned {
		lines = append(lines, fmt.Sprintf("%s:%s:%d", k.account, k.client, v))
	}
	sort.Strings(lines)

	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}

	tmp, err := os.CreateTemp(p.Dir, ".clientports-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, p.path())
}
