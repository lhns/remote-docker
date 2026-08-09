package sshd

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"

	gssh "github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/internal/server/netns"
)

// The forwarding handlers, replacing gliderlabs' own.
//
// Not because theirs are wrong -- these are near-copies of them -- but because
// both hardcode `net.Listen` and `net.Dial`, and with a daemon per account
// (ADR 0019) the namespace those calls happen in is the whole question.
//
// A reverse forward carries the client's NFS export. It must be reachable from
// inside that account's dockerd, so the containers it starts can mount it, and
// from nowhere else. A local forward reaches a published container port, which
// now exists only inside that account's namespace.

const forwardedTCPChannelType = "forwarded-tcpip"

type remoteForwardRequest struct {
	BindAddr string
	BindPort uint32
}

type remoteForwardSuccess struct {
	BindPort uint32
}

type remoteForwardCancelRequest struct {
	BindAddr string
	BindPort uint32
}

type remoteForwardChannelData struct {
	DestAddr   string
	DestPort   uint32
	OriginAddr string
	OriginPort uint32
}

type localForwardChannelData struct {
	DestAddr string
	DestPort uint32

	OriginAddr string
	OriginPort uint32
}

// forwardedTCP tracks the listeners opened for reverse forwards.
type forwardedTCP struct {
	mu       sync.Mutex
	forwards map[string]net.Listener
}

// listenFor binds the reverse forward where this account's containers can
// reach it.
//
// One listener, in one namespace, chosen by mode -- NOT one in each.
//
// With a daemon per account, the ONLY thing that needs to reach the client's
// NFS export is that account's dockerd, and binding in the agent's namespace
// as well would put an unauthenticated NFS export in the namespace every
// account's shell runs in. The plan for this work called for a dual bind to
// keep the agent's own `~/workspace` mount working; ADR 0018 deleted that
// mount, so the second listener would now have no user and a real cost.
//
// With no Manager, this is exactly what it always was: the shared dockerd
// lives in the agent's namespace, so binding there is what makes the export
// reachable at all.
func (s *Server) listenFor(ctx context.Context, account sessionAccount, addr string) (net.Listener, error) {
	if s.cfg.Daemons == nil {
		return net.Listen("tcp", addr)
	}

	// Ensure rather than Lookup: the client asks for this forward moments
	// after authenticating, and Warm may still be booting the daemon. Waiting
	// here is right -- there is nothing to bind into until it exists.
	d, err := s.cfg.Daemons.Ensure(ctx, account.Name())
	if err != nil {
		return nil, err
	}
	return netns.Listen(d.NetNSPath(), "tcp", addr)
}

// handleForwardRequest answers tcpip-forward and cancel-tcpip-forward.
//
// A near-copy of gliderlabs' ForwardedTCPHandler, differing in one line: the
// listener comes from listenFor rather than from net.Listen.
func (s *Server) handleForwardRequest(ctx gssh.Context, _ *gssh.Server, req *gossh.Request) (bool, []byte) {
	f := &s.tcpip
	f.mu.Lock()
	if f.forwards == nil {
		f.forwards = make(map[string]net.Listener)
	}
	f.mu.Unlock()

	conn, ok := ctx.Value(gssh.ContextKeyConn).(*gossh.ServerConn)
	if !ok {
		return false, nil
	}

	switch req.Type {
	case "tcpip-forward":
		var payload remoteForwardRequest
		if err := gossh.Unmarshal(req.Payload, &payload); err != nil {
			s.logf("unparseable tcpip-forward request: %v", err)
			return false, []byte{}
		}
		if !s.allowReverseForward(ctx, payload.BindAddr, payload.BindPort) {
			return false, []byte("port forwarding is disabled")
		}

		account, ok := accountFor(ctx)
		if !ok {
			return false, []byte{}
		}

		addr := net.JoinHostPort(payload.BindAddr, strconv.Itoa(int(payload.BindPort)))
		ln, err := s.listenFor(ctx, account, addr)
		if err != nil {
			// The RESERVATION has to go with the failure.
			//
			// allowReverseForward above does not only permit -- it BINDS the
			// port and arms a release for when the connection ends. So a
			// listen that fails here left the account's one reverse-tunnel
			// port reserved by a forward that does not exist, and every retry
			// was refused with "another session for this account may still be
			// open" -- blaming a second session for the first one's failure.
			s.forward.Release(account, payload.BindAddr, payload.BindPort)
			s.logf("could not bind %s for %s: %v", addr, account.Name(), err)
			return false, []byte{}
		}

		_, portStr, _ := net.SplitHostPort(ln.Addr().String())
		port, _ := strconv.Atoi(portStr)

		f.mu.Lock()
		f.forwards[addr] = ln
		f.mu.Unlock()

		go func() {
			<-ctx.Done()
			f.mu.Lock()
			ln, ok := f.forwards[addr]
			f.mu.Unlock()
			if ok {
				_ = ln.Close()
			}
		}()

		go s.serveForwarded(conn, ln, payload.BindAddr, uint32(port), addr)
		return true, gossh.Marshal(&remoteForwardSuccess{BindPort: uint32(port)})

	case "cancel-tcpip-forward":
		var payload remoteForwardCancelRequest
		if err := gossh.Unmarshal(req.Payload, &payload); err != nil {
			return false, []byte{}
		}
		addr := net.JoinHostPort(payload.BindAddr, strconv.Itoa(int(payload.BindPort)))
		f.mu.Lock()
		ln, ok := f.forwards[addr]
		f.mu.Unlock()
		if ok {
			_ = ln.Close()
		}
		return true, nil

	default:
		return false, nil
	}
}

// serveForwarded turns each accepted connection into a forwarded-tcpip channel.
func (s *Server) serveForwarded(conn *gossh.ServerConn, ln net.Listener, bindAddr string, port uint32, key string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}

		originAddr, originPortStr, _ := net.SplitHostPort(c.RemoteAddr().String())
		originPort, _ := strconv.Atoi(originPortStr)
		payload := gossh.Marshal(&remoteForwardChannelData{
			DestAddr:   bindAddr,
			DestPort:   port,
			OriginAddr: originAddr,
			OriginPort: uint32(originPort),
		})

		go func() {
			ch, reqs, err := conn.OpenChannel(forwardedTCPChannelType, payload)
			if err != nil {
				s.logf("forwarded connection refused by the client: %v", err)
				_ = c.Close()
				return
			}
			go gossh.DiscardRequests(reqs)
			bridge(ch, c)
		}()
	}

	s.tcpip.mu.Lock()
	delete(s.tcpip.forwards, key)
	s.tcpip.mu.Unlock()
}

// directTCPIP answers `ssh -L`, dialling inside the account's own namespace.
//
// A near-copy of gliderlabs' DirectTCPIPHandler, differing in the dial.
//
// The meaning of "loopback" strengthens for free here. It used to mean the
// agent's own loopback -- which carries the agent's own services, including
// the SSH port -- and now means the account's dind's loopback, where nothing
// of ours listens. Two accounts publishing 8080 also stop colliding, because
// those are two different namespaces.
func (s *Server) directTCPIP(_ *gssh.Server, _ *gossh.ServerConn, newChan gossh.NewChannel, ctx gssh.Context) {
	var d localForwardChannelData
	if err := gossh.Unmarshal(newChan.ExtraData(), &d); err != nil {
		_ = newChan.Reject(gossh.ConnectionFailed, "error parsing forward data: "+err.Error())
		return
	}

	if !s.allowLocalForward(ctx, d.DestAddr, d.DestPort) {
		_ = newChan.Reject(gossh.Prohibited, "port forwarding is disabled")
		return
	}

	// Resolved here rather than inside dialFor, so the one case that must
	// never be treated as permission -- a forward on a connection with no
	// authenticated account -- is refused where the context still is.
	account, ok := accountFor(ctx)
	if !ok {
		_ = newChan.Reject(gossh.Prohibited, errNoAccount.Error())
		return
	}

	dest := net.JoinHostPort(d.DestAddr, strconv.FormatInt(int64(d.DestPort), 10))

	conn, err := s.dialFor(ctx, account, dest)
	if err != nil {
		_ = newChan.Reject(gossh.ConnectionFailed, err.Error())
		return
	}

	ch, reqs, err := newChan.Accept()
	if err != nil {
		_ = conn.Close()
		return
	}
	go gossh.DiscardRequests(reqs)
	bridge(ch, conn)
}

// dialFor connects to dest from wherever this account's containers publish.
func (s *Server) dialFor(ctx context.Context, account sessionAccount, dest string) (net.Conn, error) {
	if s.cfg.Daemons == nil {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", dest)
	}

	d, err := s.cfg.Daemons.Ensure(ctx, account.Name())
	if err != nil {
		return nil, err
	}
	return netns.Dial(d.NetNSPath(), "tcp", dest)
}

// bridge copies both ways and closes both ends when either finishes.
func bridge(ch gossh.Channel, conn net.Conn) {
	go func() {
		defer func() { _ = ch.Close() }()
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(ch, conn)
	}()
	go func() {
		defer func() { _ = ch.Close() }()
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, ch)
	}()
}
