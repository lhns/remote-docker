package sshd

// The address a container reaches this account's host forwards at (ADR 0057).
//
// A client lists local ports it wants containers to reach. For each, it asks
// the workspace to listen on the daemon's default bridge gateway, which is what
// `host.docker.internal:host-gateway` resolves to, and carries what arrives
// back to localhost on its own machine.

import (
	"context"
	"net/netip"
	"strings"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/dockercli"
)

// bridgeGateway is the IPv4 gateway of the daemon's default bridge, or "" when
// the daemon has none or cannot say. An empty answer means "no host forwards",
// never a guess.
func (s *Server) bridgeGateway(ctx context.Context, target daemons.Target) string {
	ctx, cancel := context.WithTimeout(ctx, infoQueryTimeout)
	defer cancel()

	out, err := s.line(ctx, target.Host, dockercli.BridgeGatewayArgs()...)
	if err != nil {
		return ""
	}
	return firstIPv4(out)
}

// firstIPv4 reads the first IPv4 address, one per line: a bridge may carry an
// IPv6 range too, and the forward binds IPv4 only.
func firstIPv4(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if ip, err := netip.ParseAddr(strings.TrimSpace(line)); err == nil && ip.Is4() && !ip.IsUnspecified() && !ip.IsLoopback() {
			return ip.String()
		}
	}
	return ""
}

// hostGatewayInfo is the gateway workspace-info reports. Lookup, never Ensure:
// see infoField. A daemon that has not booted yet answers "", and the client
// asks again once its tunnel has started one.
func (s *Server) hostGatewayInfo(ctx context.Context, account string) string {
	target, ok := s.cfg.Daemons.Lookup(ctx, account)
	if !ok {
		return ""
	}
	gateway := s.bridgeGateway(ctx, target)
	if gateway == "" {
		// The client's warning says the log names why, so it must. Once per
		// account: every `docker` command asks for info.
		if _, seen := s.gatewayWarned.LoadOrStore(account, true); !seen {
			s.log().Warn("the account's daemon reported no IPv4 gateway for its default bridge, so host-ports cannot be served",
				"account", account)
		}
	}
	return gateway
}

// allowHostForward decides a reverse forward whose host is not loopback. It
// reports handled=false for any host that is not this account's own bridge
// gateway, which then meets the loopback rule unchanged.
//
// Any port is allowed, and none is reserved: with a daemon per account (ADR
// 0019) alice and bob both bind 172.17.0.1:3000 in different namespaces, so a
// (host, port) reservation would refuse one of them. The kernel's EADDRINUSE in
// the bind is the arbiter, and the caller returns token 0, which Release treats
// as never live. The listener binds that one IP and never 0.0.0.0, which in
// shared mode (ADR 0012) would be the workspace's external interface.
func (s *Server) allowHostForward(ctx context.Context, account sessionAccount, host string, port uint32) (handled, ok bool) {
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.IsLoopback() || ip.IsUnspecified() {
		return false, false
	}
	// Lookup, never Ensure: any connection may send a non-loopback host, and
	// that must not start a daemon. The client binds only after its NFS tunnel
	// has started this one.
	target, ok := s.cfg.Daemons.Lookup(ctx, account.Name())
	if !ok {
		return false, false
	}
	gateway := s.bridgeGateway(ctx, target)
	if gateway == "" || ip.String() != gateway {
		return false, false
	}
	// 0 would pick a port the client never named.
	if port == 0 || port > 65535 {
		s.log().Warn("refused a host forward: not a port", "host", host, "port", port, "account", account.Name())
		return true, false
	}
	s.log().Info("forwarding a client port to containers", "account", account.Name(), "host", host, "port", port)
	return true, true
}
