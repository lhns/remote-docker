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

	mu    sync.Mutex
	bound map[string]string // "host:port" -> account that holds it
}

// NewForwardPolicy returns a policy over the given uid/port mapping.
func NewForwardPolicy(mapping workspace.Mapping) *ForwardPolicy {
	return &ForwardPolicy{Mapping: mapping, bound: map[string]string{}}
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
//     tunnel and silently take over its mounts.
func (p *ForwardPolicy) Allow(account Account, host string, port uint32) (bool, string) {
	if !isLoopback(host) {
		return false, "only loopback addresses may be forwarded: the NFS export is unauthenticated"
	}

	want, err := p.Mapping.PortForUID(account.UID())
	if err != nil {
		return false, "this account has no reverse-tunnel port"
	}
	if int(port) != want {
		return false, "this account may only bind port " + strconv.Itoa(want)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if holder, taken := p.bound[key(host, port)]; taken && holder != account.Name() {
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

// Bind records that an account holds a port. It returns false if somebody else
// already does.
func (p *ForwardPolicy) Bind(account Account, host string, port uint32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := key(host, port)
	if holder, taken := p.bound[k]; taken && holder != account.Name() {
		return false
	}
	p.bound[k] = account.Name()
	return true
}

// Release gives a port up. Only the holder can, so a session ending cannot
// release a port another session has since taken.
func (p *ForwardPolicy) Release(account Account, host string, port uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := key(host, port)
	if p.bound[k] == account.Name() {
		delete(p.bound, k)
	}
}

// Holder returns the account currently holding a port, if any.
func (p *ForwardPolicy) Holder(host string, port uint32) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	holder, ok := p.bound[key(host, port)]
	return holder, ok
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
