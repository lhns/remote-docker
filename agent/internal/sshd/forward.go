package sshd

import (
	"net"
	"strconv"
	"sync"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// Account is what the forward policy needs to know about a session.
type Account interface {
	Name() string
	UID() int

	// Client names the machine the session came from. Empty means a client
	// too old to be named, which gets the uid-derived port.
	Client() string
}

// PortOwner answers which reverse-tunnel ports an account has been given.
//
// An interface rather than the allocator itself, so the policy can be tested
// without a state directory and so the rule reads as a question rather than as
// arithmetic. Nil falls back to the mapping, which is one port per account.
type PortOwner interface {
	Owns(account string, uid, port int) bool
}

// ForwardPolicy decides which loopback ports an account may bind.
//
// This is the whole of ADR 0010's central claim. Under sshd the equivalent
// rule was a permitlisten="127.0.0.1:<port>" option generated into every key's
// authorized_keys entry: a policy of one comparison, implemented as string
// generation into a file format with no schema, and enforced by a component
// that had no idea why the number was what it was.
//
// Here it is the comparison.
type ForwardPolicy struct {
	Mapping workspace.Mapping

	// Ports is what an account has actually been given, which is more than the
	// uid decides once one account is used from two machines (ADR 0029).
	Ports PortOwner

	mu sync.Mutex
	// next is the last token handed out. Pre-incremented, so the first is 1
	// and ZERO IS NEVER A LIVE RESERVATION -- which is what Bind returns when
	// it refuses, so a caller that released without checking cannot match
	// somebody else's entry.
	//
	// A counter rather than anything unguessable: it never leaves this
	// process, and the only code that can present one is the code Bind handed
	// it to.
	next  uint64
	bound map[string]reservation
}

// reservation is one held port, and WHICH session holds it.
//
// The token is the whole point. Keyed by account name, a reservation could be
// released by any session of that account, including one that had just failed
// to bind: a second machine's failed attempt deleted the first machine's live
// reservation, after which AllowDial reported the port as free and, on a shared
// daemon (ADR 0012), another account could reach an NFS export that
// authenticates nobody.
type reservation struct {
	account string
	token   uint64
}

// NewForwardPolicy returns a policy over the given uid/port mapping.
func NewForwardPolicy(mapping workspace.Mapping) *ForwardPolicy {
	return &ForwardPolicy{Mapping: mapping, bound: map[string]reservation{}}
}

// Allow reports whether an account may bind host:port, and why not if it may
// not.
//
// Three rules, in order of what they protect against:
//
//  1. loopback only. A workspace's NFS export is unauthenticated, so anything
//     that can reach the port can read and write the client's files, so it
//     must never be published beyond the container.
//  2. the account's own port, and only that one. This is what stops one user
//     binding another's port before they connect and serving them a
//     filesystem of the attacker's choosing.
//  3. one holder at a time, so a second session cannot displace the first's
//     tunnel and silently take over its mounts. Enforced by Bind rather than
//     here: this rule refuses another ACCOUNT, and Bind refuses anybody at all,
//     including a second session of this one.
func (p *ForwardPolicy) Allow(account Account, host string, port uint32) (bool, string) {
	if !isLoopback(host) {
		return false, "only loopback addresses may be forwarded: the NFS export is unauthenticated"
	}

	// Asked of the allocator rather than computed, because the agent is what
	// hands ports out once an account has more than one machine, and a rule
	// that recomputed the answer would refuse a port the agent itself had just
	// told a client to use.
	if !p.owns(account, int(port)) {
		want, err := p.Mapping.PortForUID(account.UID())
		if err != nil {
			return false, "this account has no reverse-tunnel port"
		}
		return false, "this account may only bind the port it was given, " + strconv.Itoa(want)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if res, taken := p.bound[key(host, port)]; taken && res.account != account.Name() {
		return false, "that port is held by another session"
	}
	return true, ""
}

// AllowDial reports whether an account may open a local forward to host:port,
// and why not if it may not.
//
// The mirror of Allow, and needed for the same reason in the other direction.
// Two rules:
//
//  1. loopback only, so the workspace cannot be used to reach the network it
//     sits on.
//  2. not a port another account currently holds. With one daemon for
//     everybody (ADR 0012) every account shares this network namespace, so
//     another account's reverse-tunnel port is reachable from here, and what
//     answers is their NFS export with AuthFlavorNull: read and write access
//     to the files on their machine. A daemon per account (ADR 0019, the
//     default) puts each tunnel in a namespace of its own, where that address
//     reaches nothing of theirs.
//
// Rule 2 asks the holder rather than the port range on purpose. PortForUID
// maps onto PortBase and upwards, docker publishes host ports from 32768, and
// refusing that range would break the forwarding this exists for. A port
// nobody holds has nothing of ours listening on it anyway.
func (p *ForwardPolicy) AllowDial(account Account, host string, port uint32) (bool, string) {
	if !isLoopback(host) {
		return false, "only loopback addresses may be reached"
	}
	if holder, taken := p.Holder(host, port); taken && holder != account.Name() {
		return false, "that port is another account's file server"
	}
	return true, ""
}

// owns reports whether this account is entitled to a port.
func (p *ForwardPolicy) owns(account Account, port int) bool {
	if p.Ports != nil {
		return p.Ports.Owns(account.Name(), account.UID(), port)
	}
	want, err := p.Mapping.PortForUID(account.UID())
	return err == nil && port == want
}

// Bind records that a session holds a port, returning the token that releases
// it again. It refuses if anybody already holds that port, including another
// session of the same account.
//
// Refusing a second session of one account is not a policy about accounts, it
// is the truth about the port: one listener can hold it, and pretending
// otherwise is what let a failed attempt speak for the session that had
// succeeded. A client whose previous connection is still being torn down is
// refused here rather than allowed to take a port its own live listener is
// using.
func (p *ForwardPolicy) Bind(account Account, host string, port uint32) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := key(host, port)
	if _, taken := p.bound[k]; taken {
		return 0, false
	}
	p.next++
	p.bound[k] = reservation{account: account.Name(), token: p.next}
	return p.next, true
}

// Release gives a port up. Only the session holding it can, so neither a
// session ending nor one failing to bind can release a port another session
// holds.
func (p *ForwardPolicy) Release(token uint64, host string, port uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := key(host, port)
	if res, taken := p.bound[k]; taken && res.token == token {
		delete(p.bound, k)
	}
}

// Holder returns the account currently holding a port, if any.
func (p *ForwardPolicy) Holder(host string, port uint32) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	res, ok := p.bound[key(host, port)]
	return res.account, ok
}

func key(host string, port uint32) string {
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

// isLoopback reports whether an address is the local interface.
//
// The empty string and "*" mean "all interfaces" in the SSH protocol, which is
// exactly what must not happen here.
func isLoopback(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}
