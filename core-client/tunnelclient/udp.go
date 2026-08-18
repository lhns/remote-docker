package tunnelclient

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core/tunnel"
)

// DialRemoteUDP opens a datagram flow from the workspace to addr.
//
// One flow per local sender, so what comes back on the workspace's socket
// belongs to exactly one of them (ADR 0038). The returned conn carries whole
// datagrams: one Write is one datagram and one Read is one datagram, which the
// length prefix in core/tunnel is what makes true over a byte stream.
//
// A workspace too old to know this channel type REJECTS it, and that refusal is
// the version check: there is nothing to ask first.
func (c *Client) DialRemoteUDP(addr string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("tunnel: %s is not an address: %w", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("tunnel: %s has no port: %w", addr, err)
	}

	payload := ssh.Marshal(tunnel.ForwardPayload{
		DestAddr:   host,
		DestPort:   uint32(port),
		OriginAddr: "127.0.0.1",
		OriginPort: 0,
	})

	ch, reqs, err := c.ssh.OpenChannel(tunnel.UDPChannelType, payload)
	if err != nil {
		return nil, fmt.Errorf("tunnel: opening a datagram flow to %s: %w", addr, err)
	}
	go ssh.DiscardRequests(reqs)

	return &datagramConn{ch: ch, remote: addr}, nil
}

// datagramConn is one datagram flow, shaped as a net.Conn so a caller can treat
// it like the connected UDP socket it stands in for.
type datagramConn struct {
	ch     ssh.Channel
	remote string
}

func (d *datagramConn) Read(p []byte) (int, error) {
	n, err := tunnel.ReadDatagram(d.ch, p)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (d *datagramConn) Write(p []byte) (int, error) {
	if err := tunnel.WriteDatagram(d.ch, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (d *datagramConn) Close() error { return d.ch.Close() }

// The rest of net.Conn, which nothing on this path uses: a datagram flow has no
// deadlines of its own, and its addresses are the tunnel's.
func (d *datagramConn) LocalAddr() net.Addr  { return udpAddr("127.0.0.1:0") }
func (d *datagramConn) RemoteAddr() net.Addr { return udpAddr(d.remote) }

func (d *datagramConn) SetDeadline(_ time.Time) error      { return errNoDeadline }
func (d *datagramConn) SetReadDeadline(_ time.Time) error  { return errNoDeadline }
func (d *datagramConn) SetWriteDeadline(_ time.Time) error { return errNoDeadline }

var errNoDeadline = errors.New("tunnel: a datagram flow has no deadline of its own")

type udpAddr string

func (a udpAddr) Network() string { return "udp" }
func (a udpAddr) String() string  { return string(a) }
