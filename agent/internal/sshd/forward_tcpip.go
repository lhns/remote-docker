package sshd

// This workspace's answers to the forwarding machinery's questions.
//
// The protocol is in core-agent/tunnelserver; what is here is everything it
// deliberately does not know: which account a connection belongs to, whether
// that account owns the port, and which network namespace the forward has to
// live in for the right containers to reach it and no others.
//
// A reverse forward carries the client's NFS export. It must be reachable from
// inside that account's dockerd, so the containers it starts can mount it, and
// from nowhere else. A local forward reaches a published container port, which
// with a daemon per account (ADR 0019) exists only inside that namespace.

import (
	"context"
	"net"

	gssh "github.com/gliderlabs/ssh"

	"github.com/lhns/remote-docker/core-agent/netns"
)

// reversePolicy implements tunnel server.Reverse.
type reversePolicy struct{ s *Server }

// localPolicy implements tunnel server.Local.
type localPolicy struct{ s *Server }

// Allow gates `ssh -R`, which is how the client's NFS export reaches the
// workspace. This is where ADR 0010's claim is enforced.
//
// It reserves as well as permits, and returns the token that releases the
// reservation again. A reservation belongs to this session and nothing else may
// give it up: releasing by account name meant a second machine's FAILED bind
// deleted the first machine's live reservation, after which AllowDial reported
// the port as free to every other account on a shared daemon.
func (p reversePolicy) Allow(ctx gssh.Context, host string, port uint32) (uint64, bool) {
	s := p.s
	account, ok := accountFor(ctx)
	if !ok {
		return 0, false
	}

	allowed, why := s.forward.Allow(account, host, port)
	if !allowed {
		s.log().Warn("refused a reverse forward", "host", host, "port", port, "account", account.Name(), "why", why)
		return 0, false
	}
	token, ok := s.forward.Bind(account, host, port)
	if !ok {
		holder, _ := s.forward.Holder(host, port)
		s.log().Warn("refused a reverse forward: the port is already held",
			"host", host, "port", port, "account", account.Name(), "holder", holder)
		return 0, false
	}

	// Released when the connection ends, so a dropped client does not keep its
	// port reserved forever. By token, so a connection ending late cannot
	// release the reservation whoever came after it now holds.
	go func() {
		<-ctx.Done()
		s.forward.Release(token, host, port)
	}()

	s.log().Info("forwarding", "account", account.Name(), "host", host, "port", port)
	return token, true
}

func (p reversePolicy) Release(token uint64, host string, port uint32) {
	p.s.forward.Release(token, host, port)
}

// Listen binds the reverse forward where this account's containers can reach it.
//
// One listener, in one namespace, chosen by mode. NOT one in each.
//
// With a daemon per account, the ONLY thing that needs to reach the client's
// NFS export is that account's dockerd, and binding in the agent's namespace as
// well would put an unauthenticated NFS export in the namespace every account's
// shell runs in. The plan for this work called for a dual bind to keep the
// agent's own `~/workspace` mount working; ADR 0018 deleted that mount, so the
// second listener would now have no user and a real cost.
//
// With no Manager, this is exactly what it always was: the shared dockerd lives
// in the agent's namespace, so binding there is what makes the export reachable
// at all.
func (p reversePolicy) Listen(ctx gssh.Context, addr string) (net.Listener, error) {
	account, ok := accountFor(ctx)
	if !ok {
		return nil, errNoAccount
	}
	return p.s.listenFor(ctx, account, addr)
}

// listenFor is Listen with the account already resolved, which is the shape the
// namespace choice can actually be tested in: a gssh.Context cannot be built
// without a connection, and what these tests assert is which namespace was
// asked for.
func (s *Server) listenFor(ctx context.Context, account sessionAccount, addr string) (net.Listener, error) {
	// Ensure rather than Lookup: the client asks for this forward moments after
	// authenticating, and Warm may still be booting the daemon. Waiting here is
	// right, because there is nothing to bind into until it exists.
	target, err := s.cfg.Daemons.Ensure(ctx, account.Name())
	if err != nil {
		return nil, err
	}
	// An empty path is the agent's own namespace, where the shared daemon
	// lives, so this one line serves both modes. See netns.Do.
	return netns.Listen(target.NetNSPath, "tcp", addr)
}

// AllowDial gates `ssh -L`, which the client uses to reach published container
// ports.
//
// The rules are ForwardPolicy.AllowDial, beside the ones for binding, because
// they are the same question asked in the other direction. A connection with no
// authenticated account is refused here, which is the one case that must never
// be mistaken for permission.
func (p localPolicy) AllowDial(ctx gssh.Context, host string, port uint32) bool {
	account, ok := accountFor(ctx)
	if !ok {
		return false
	}
	if ok, why := p.s.forward.AllowDial(account, host, port); !ok {
		p.s.log().Warn("refused a local forward: "+why,
			"host", host, "port", port, "account", account.Name())
		return false
	}
	return true
}

// Dial connects to dest from wherever this account's containers publish.
//
// "loopback" here is the account's own dind's loopback, not the agent's. That
// is what a local forward should be able to reach and it is where nothing of
// ours listens, whereas the agent's carries the agent's own services including
// the SSH port. It also means two accounts can publish 8080 at once without
// colliding, because those are two namespaces.
func (p localPolicy) Dial(ctx gssh.Context, dest string) (net.Conn, error) {
	account, ok := accountFor(ctx)
	if !ok {
		return nil, errNoAccount
	}
	return p.s.dialFor(ctx, account, dest)
}

func (s *Server) dialFor(ctx context.Context, account sessionAccount, dest string) (net.Conn, error) {
	return s.dialForNetwork(ctx, account, "tcp", dest)
}

// DialUDP is Dial for datagrams, which reach a published UDP port (ADR 0038).
//
// The same namespace and the same policy: only the network differs, and a
// connected UDP socket is a net.Conn whose reads and writes are whole
// datagrams, so nothing above has to reassemble anything.
func (p localPolicy) DialUDP(ctx gssh.Context, dest string) (net.Conn, error) {
	account, ok := accountFor(ctx)
	if !ok {
		return nil, errNoAccount
	}
	return p.s.dialForNetwork(ctx, account, "udp", dest)
}

func (s *Server) dialForNetwork(ctx context.Context, account sessionAccount, network, dest string) (net.Conn, error) {
	target, err := s.cfg.Daemons.Ensure(ctx, account.Name())
	if err != nil {
		return nil, err
	}
	return netns.Dial(target.NetNSPath, network, dest)
}
