// Package tunnelserver is the workspace end of the tunnel: the SSH forwarding
// machinery, with every decision it depends on injected.
//
// gliderlabs ships handlers for both forward directions and they are nearly
// right. What is wrong with them is one line each: both hardcode `net.Listen`
// and `net.Dial`, and where those calls happen is the whole question when the
// thing that must reach the forward lives in another network namespace. So
// these are near-copies with the listen and the dial handed in.
//
// Nothing here knows what a forward is FOR. Whether an account may bind a port,
// which namespace it must be reachable from, and what happens to a reservation
// when the connection ends are all the caller's, because all three are policy
// this project invented and none of them is SSH (ADR 0021). What is left is the
// protocol: the payload shapes, the listener bookkeeping, opening a channel per
// accepted connection, and taking the listener down with the connection.
package tunnelserver

import (
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"

	gssh "github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core/tunnel"

	"github.com/lhns/remote-docker/core/logx"
)

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

// Reverse decides `ssh -R`: whether a connection may ask the workspace to
// listen, and where that listener goes.
//
// Allow is not a predicate and must not be called as one. It RESERVES the port
// and returns the token that gives the reservation up again, because a
// reservation belongs to one session: releasing by name let a second machine's
// failed bind delete the first machine's live reservation. The machinery holds
// the token and hands it back on every path that ends the forward, including
// the one where Listen itself fails.
type Reverse interface {
	Allow(ctx gssh.Context, host string, port uint32) (token uint64, ok bool)
	Release(token uint64, host string, port uint32)
	Listen(ctx gssh.Context, addr string) (net.Listener, error)
}

// Local decides `ssh -L`: whether a connection may reach an address, and from
// where it is reached.
//
// A connection with nothing authenticated on it must be refused by AllowDial.
// That case cannot be handled here, because what authentication means is the
// caller's, and the one answer that must never be given is permission.
type Local interface {
	AllowDial(ctx gssh.Context, host string, port uint32) bool
	Dial(ctx gssh.Context, addr string) (net.Conn, error)

	// DialUDP opens a datagram flow to addr, from inside whatever namespace
	// this account belongs in. The returned conn carries whole datagrams: a
	// connected UDP socket is one, and that is what makes the framing this
	// package adds the only thing the channel needs (ADR 0038).
	DialUDP(ctx gssh.Context, addr string) (net.Conn, error)
}

// Forwards is the pair of handlers, and the listeners they have open.
//
// The zero value is not usable: both policies are required, because a nil one
// could only mean "allow" or "deny", and one of those is a security hole while
// the other is a server that silently does nothing.
type Forwards struct {
	Reverse Reverse
	Local   Local
	Log     *slog.Logger

	mu       sync.Mutex
	forwards map[string]net.Listener
}

// HandleRequest answers tcpip-forward and cancel-tcpip-forward. Register it for
// both: gliderlabs dispatches by request type and this switches on it.
func (f *Forwards) HandleRequest(ctx gssh.Context, _ *gssh.Server, req *gossh.Request) (bool, []byte) {
	conn, ok := ctx.Value(gssh.ContextKeyConn).(*gossh.ServerConn)
	if !ok {
		return false, nil
	}

	switch req.Type {
	case "tcpip-forward":
		var payload remoteForwardRequest
		if err := gossh.Unmarshal(req.Payload, &payload); err != nil {
			f.log().Warn("unparseable tcpip-forward request", "err", err)
			return false, []byte{}
		}
		return f.open(ctx, conn, payload)

	case "cancel-tcpip-forward":
		var payload remoteForwardCancelRequest
		if err := gossh.Unmarshal(req.Payload, &payload); err != nil {
			return false, []byte{}
		}
		f.close(net.JoinHostPort(payload.BindAddr, strconv.Itoa(int(payload.BindPort))))
		return true, nil

	default:
		return false, nil
	}
}

func (f *Forwards) open(ctx gssh.Context, conn *gossh.ServerConn, payload remoteForwardRequest) (bool, []byte) {
	token, allowed := f.Reverse.Allow(ctx, payload.BindAddr, payload.BindPort)
	if !allowed {
		return false, []byte("port forwarding is disabled")
	}

	addr := net.JoinHostPort(payload.BindAddr, strconv.Itoa(int(payload.BindPort)))
	ln, err := f.Reverse.Listen(ctx, addr)
	if err != nil {
		// The reservation goes with the failure, and only this one.
		//
		// Allow above did not merely permit: it bound the port. A listen that
		// fails here would otherwise leave the port reserved by a forward that
		// does not exist, and every retry was refused on behalf of a session
		// that had already failed.
		f.Reverse.Release(token, payload.BindAddr, payload.BindPort)
		f.log().Error("could not bind a forward", "addr", addr, "err", err)
		return false, []byte{}
	}

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	f.mu.Lock()
	if f.forwards == nil {
		f.forwards = make(map[string]net.Listener)
	}
	f.forwards[addr] = ln
	f.mu.Unlock()

	// The listener does not outlive the connection that asked for it.
	go func() {
		<-ctx.Done()
		f.close(addr)
	}()

	go f.serve(conn, ln, payload.BindAddr, uint32(port), addr)
	return true, gossh.Marshal(&remoteForwardSuccess{BindPort: uint32(port)})
}

// close takes down the listener for addr, if there is one.
func (f *Forwards) close(addr string) {
	f.mu.Lock()
	ln, ok := f.forwards[addr]
	f.mu.Unlock()
	if ok {
		_ = ln.Close()
	}
}

// serve turns each accepted connection into a forwarded-tcpip channel.
func (f *Forwards) serve(conn *gossh.ServerConn, ln net.Listener, bindAddr string, port uint32, key string) {
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
				f.log().Warn("the client refused a forwarded connection", "err", err)
				_ = c.Close()
				return
			}
			go gossh.DiscardRequests(reqs)
			bridge(ch, c)
		}()
	}

	f.mu.Lock()
	delete(f.forwards, key)
	f.mu.Unlock()
}

// HandleChannel answers direct-tcpip, which is `ssh -L`. Register it as the
// handler for that channel type.
func (f *Forwards) HandleChannel(_ *gssh.Server, _ *gossh.ServerConn, newChan gossh.NewChannel, ctx gssh.Context) {
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
	conn, err := f.Local.Dial(ctx, dest)
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

// bridge copies both ways and closes both ends when either finishes.
//
// Deliberately not tunnel.Splice: a forwarded connection carries no output
// stream that a half-close would strand, and leaving one direction open here
// would leak a blocked reader per connection.
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

// log is the server's logger, or silence. See logx.Or.
func (f *Forwards) log() *slog.Logger {
	return logx.Or(f.Log)
}
