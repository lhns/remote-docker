package tunnelclient

import (
	"fmt"
	"io"
	"net"
	"strconv"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core/tunnel"
)

// DialRemoteUDP opens a datagram flow from the workspace to addr (ADR 0038).
//
// One Write is one datagram and one Read is one datagram, which the length
// prefix in core/tunnel is what makes true over a byte stream.
//
// A workspace too old to know this channel type REJECTS it, and that refusal is
// the version check: there is nothing to ask first.
func (c *Client) DialRemoteUDP(addr string) (io.ReadWriteCloser, error) {
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

	return &datagramConn{ch: ch}, nil
}

// datagramConn is one datagram flow. Read, Write and Close and no more: it
// stands in for a connected UDP socket, and nothing that uses it wants an
// address or a deadline.
type datagramConn struct{ ch ssh.Channel }

func (d *datagramConn) Read(p []byte) (int, error) { return tunnel.ReadDatagram(d.ch, p) }

func (d *datagramConn) Write(p []byte) (int, error) {
	if err := tunnel.WriteDatagram(d.ch, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (d *datagramConn) Close() error { return d.ch.Close() }
