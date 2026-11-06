package session

// Local ports a container can reach as host.docker.internal (ADR 0057).
//
// For each port in the host-ports setting the workspace is asked to listen on
// its daemon's default bridge gateway, and every connection that arrives there
// is carried back here to localhost on the same number. Nothing about where a
// connection goes is taken from the channel: a listener is registered for one
// port and dials only that.

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lhns/remote-docker/core/tunnel"
)

// hostDialTimeout bounds the dial to this machine, which is loopback: a port
// nothing listens on is refused at once, so waiting longer only holds the
// container's connection open.
const hostDialTimeout = 5 * time.Second

// hostForward is one listed port and what became of it on this connection.
type hostForward struct {
	port     int
	listener net.Listener
	// problem is why there is no listener, empty when there is one.
	problem string
}

// hostGateway is the address the workspace reports for host forwards, asked
// again when the first answer was empty: the agent reports it only from a
// daemon that is already running, and at connect that daemon is often still
// starting. By the time the tunnel is bound it is up.
func (s *Session) hostGateway(ctx context.Context, live *liveConn) string {
	if live.info.HostGateway == "" {
		if info, err := readInfo(ctx, live.ssh); err == nil {
			live.info.HostGateway = info.HostGateway
		}
	}
	if ip, err := netip.ParseAddr(live.info.HostGateway); err == nil && ip.Is4() {
		return ip.String()
	}
	return ""
}

// startHostPorts binds every listed port on the workspace. It never fails the
// connection: a port that cannot be forwarded is warned about and shown by
// `status`, and the session carries on with the rest.
func (s *Session) startHostPorts(ctx context.Context, live *liveConn) {
	s.hostMu.Lock()
	ports := slices.Clone(s.hostPorts)
	s.hostMu.Unlock()

	live.hostMu.Lock()
	live.hostCtx = ctx
	live.hostMu.Unlock()
	s.applyHostPorts(ctx, live, ports)
	if len(ports) > 0 {
		s.hostConnects.Add(1)
	}
}

// SetHostPorts replaces the listed ports, satisfying proxy.Control: the next
// connection starts from this list, and a live one binds the new ports and
// closes the removed ones without a restart (ADR 0057).
func (s *Session) SetHostPorts(ports []int) error {
	ports = slices.Clone(ports)
	s.hostMu.Lock()
	s.hostPorts = ports
	s.hostMu.Unlock()

	live, ok := s.gate.currentLive()
	if !ok || live == nil {
		return nil
	}
	live.hostMu.Lock()
	ctx := live.hostCtx
	live.hostMu.Unlock()
	if ctx == nil {
		return nil // not a hosting connection
	}
	s.applyHostPorts(ctx, live, ports)
	return nil
}

// applyHostPorts makes the connection's forwards the listed ports: keeps what
// is bound, closes what is no longer listed, and binds the rest. A port that
// was refused before is tried again, since a person adding it back is the
// moment the cause may have gone.
func (s *Session) applyHostPorts(ctx context.Context, live *liveConn, ports []int) {
	live.hostMu.Lock()
	defer live.hostMu.Unlock()
	if live.hostClosed {
		return
	}

	var kept []hostForward
	for _, f := range live.hostForwards {
		switch {
		case f.listener != nil && slices.Contains(ports, f.port):
			kept = append(kept, f)
		case f.listener != nil:
			_ = f.listener.Close()
		}
	}
	live.hostForwards = kept

	if len(ports) == 0 {
		live.rewriter.SetHostGateway("")
		return
	}

	gateway := s.hostGateway(ctx, live)
	if gateway == "" {
		s.hostNoGateway.Do(func() {
			s.log().Warn("host-ports is set, but this workspace reports no host gateway, so containers cannot reach them\n" +
				"  fix: upgrade the workspace to a release with host-ports; if it is current, its log says why")
		})
		live.rewriter.SetHostGateway("")
		for _, port := range ports {
			if !live.hostBound(port) {
				live.hostForwards = append(live.hostForwards, hostForward{port: port, problem: "this workspace reports no host gateway"})
			}
		}
		live.sortHostForwards()
		return
	}
	// The create rewrite names the same address, so a container's
	// host.docker.internal is where the listeners are.
	live.rewriter.SetHostGateway(gateway)

	// A second connection is a reconnect, and the previous one's listeners may
	// still be held by the workspace for a while.
	reconnect := s.hostConnects.Load() > 0
	for _, port := range ports {
		if live.hostBound(port) {
			continue
		}
		addr := net.JoinHostPort(gateway, strconv.Itoa(port))
		l, err := live.ssh.Listen(addr)
		if err != nil {
			problem := "the workspace refused " + addr
			msg := fmt.Sprintf("host port %d: %s\n  fix: the workspace's log says why", port, problem)
			if reconnect {
				msg += "; after a reconnect it may still hold the previous connection's listener for about a minute"
			}
			s.log().Warn(msg)
			live.hostForwards = append(live.hostForwards, hostForward{port: port, problem: problem})
			continue
		}
		live.hostForwards = append(live.hostForwards, hostForward{port: port, listener: l})
		live.wg.Go(func() { serveHostPort(ctx, l, port, s.log().Warn) })
	}
	live.sortHostForwards()
}

// hostBound reports whether a port has a listener, under hostMu.
func (live *liveConn) hostBound(port int) bool {
	return slices.ContainsFunc(live.hostForwards, func(f hostForward) bool { return f.port == port && f.listener != nil })
}

func (live *liveConn) sortHostForwards() {
	slices.SortFunc(live.hostForwards, func(a, b hostForward) int { return a.port - b.port })
}

// closeHostForwards ends every listener and stops later ones from being made.
func (live *liveConn) closeHostForwards() {
	live.hostMu.Lock()
	defer live.hostMu.Unlock()
	live.hostClosed = true
	for _, f := range live.hostForwards {
		if f.listener != nil {
			_ = f.listener.Close()
		}
	}
}

// serveHostPort carries each connection accepted on l to localhost:port here.
// "localhost", not 127.0.0.1: a server on this machine may listen on ::1 only,
// and the dialler tries both. A port nothing listens on closes the container's
// connection, which is what the container would see from any closed port.
func serveHostPort(ctx context.Context, l net.Listener, port int, warn func(string, ...any)) {
	target := net.JoinHostPort("localhost", strconv.Itoa(port))
	var wg sync.WaitGroup
	defer wg.Wait()
	// Once per listener: a container retrying a closed port is not news.
	var warned atomic.Bool

	for {
		in, err := l.Accept()
		if err != nil {
			return
		}
		wg.Go(func() {
			dialer := net.Dialer{Timeout: hostDialTimeout}
			out, err := dialer.DialContext(ctx, "tcp", target)
			if err != nil {
				_ = in.Close()
				if ctx.Err() == nil && warned.CompareAndSwap(false, true) {
					warn(fmt.Sprintf("host port %d: nothing answers on %s here", port, target))
				}
				return
			}
			defer func() { _ = in.Close(); _ = out.Close() }()
			tunnel.SpliceAndClose(in, out)
		})
	}
}

// hostForwardStatus is one line per listed port for `remote status`.
func (live *liveConn) hostForwardStatus() []string {
	live.hostMu.Lock()
	defer live.hostMu.Unlock()
	var out []string
	for _, f := range live.hostForwards {
		if f.problem != "" {
			out = append(out, fmt.Sprintf("%d: not forwarded, %s", f.port, f.problem))
			continue
		}
		out = append(out, fmt.Sprintf("%d: containers reach it as host.docker.internal:%d", f.port, f.port))
	}
	return out
}
