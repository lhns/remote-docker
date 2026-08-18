package tunnelserver

import (
	"net"
	"strconv"

	gssh "github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core/tunnel"
)

// HandleUDPChannel answers the datagram channel, which is ssh -L for UDP
// (ADR 0038). Register it as the handler for tunnel.UDPChannelType.
//
// The same payload and the same policy as direct-tcpip: the question of who may
// reach which loopback port does not change with the protocol, so AllowDial
// answers both and there is no second rule to keep in step.
func (f *Forwards) HandleUDPChannel(_ *gssh.Server, _ *gossh.ServerConn, newChan gossh.NewChannel, ctx gssh.Context) {
	var d tunnel.ForwardPayload
	if err := gossh.Unmarshal(newChan.ExtraData(), &d); err != nil {
		_ = newChan.Reject(gossh.ConnectionFailed, "error parsing forward data: "+err.Error())
		return
	}

	if !f.Local.AllowDial(ctx, d.DestAddr, d.DestPort) {
		_ = newChan.Reject(gossh.Prohibited, "port forwarding is disabled")
		return
	}

	dest := net.JoinHostPort(d.DestAddr, strconv.FormatInt(int64(d.DestPort), 10))
	conn, err := f.Local.DialUDP(ctx, dest)
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
	bridgeDatagrams(ch, conn)
}

// bridgeDatagrams copies datagrams both ways, framed on the channel and bare on
// the socket, and closes both when either finishes.
//
// The socket is CONNECTED, so a read is one datagram from the port this flow
// belongs to and nothing else. An error reading it is how an ICMP
// port-unreachable arrives, and it ends this flow rather than the forward the
// client is keeping open.
func bridgeDatagrams(ch gossh.Channel, conn net.Conn) {
	go func() {
		defer func() { _ = ch.Close() }()
		defer func() { _ = conn.Close() }()

		buf := make([]byte, tunnel.MaxDatagram)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if err := tunnel.WriteDatagram(ch, buf[:n]); err != nil {
				return
			}
		}
	}()

	go func() {
		defer func() { _ = ch.Close() }()
		defer func() { _ = conn.Close() }()

		buf := make([]byte, tunnel.MaxDatagram)
		for {
			n, err := tunnel.ReadDatagram(ch, buf)
			if err != nil {
				// Including io.EOF, which is the client letting go: a datagram
				// flow has no other close.
				return
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return
			}
		}
	}()
}
