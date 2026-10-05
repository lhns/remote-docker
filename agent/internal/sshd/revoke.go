package sshd

// Revocation disconnects. Authentication is checked once, at the handshake, so
// without this a revoked key kept every connection it already had, and with it
// its reverse-tunnel port (ADR 0028). The store tells us after every sync, and
// closing the connection releases its ports through the usual path.

import (
	"net"
	"sync"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"
)

// conns is every live connection and, once it has authenticated, who as.
type conns struct {
	mu   sync.Mutex
	live map[gssh.Context]*liveConn
}

type liveConn struct {
	conn    net.Conn
	account string
	key     ssh.PublicKey // nil until authenticated
}

// track registers a connection until it ends. From ConnCallback.
func (c *conns) track(ctx gssh.Context, conn net.Conn) {
	c.mu.Lock()
	if c.live == nil {
		c.live = map[gssh.Context]*liveConn{}
	}
	c.live[ctx] = &liveConn{conn: conn}
	c.mu.Unlock()

	go func() {
		<-ctx.Done()
		c.mu.Lock()
		delete(c.live, ctx)
		c.mu.Unlock()
	}()
}

// authenticated records who a connection is, and reports whether that is
// still authorized. Asked again under the registry's lock, because a sync
// landing between authenticate's lookup and this record would otherwise sweep
// before there was anything to close.
func (s *Server) authenticated(ctx gssh.Context, account string, key ssh.PublicKey) bool {
	s.conns.mu.Lock()
	defer s.conns.mu.Unlock()
	if lc, ok := s.conns.live[ctx]; ok {
		lc.account, lc.key = account, key
	}
	return s.stillAuthorized(account, key)
}

func (s *Server) stillAuthorized(account string, key ssh.PublicKey) bool {
	a, ok := s.cfg.Accounts.Lookup(account)
	return ok && a.Authorized(key)
}

// sweep closes every connection whose key its account no longer enrols.
func (s *Server) sweep() {
	var revoked []liveConn // copies: authenticate may still write the originals
	s.conns.mu.Lock()
	for _, lc := range s.conns.live {
		if lc.key != nil && !s.stillAuthorized(lc.account, lc.key) {
			revoked = append(revoked, *lc)
		}
	}
	s.conns.mu.Unlock()

	for _, lc := range revoked {
		s.log().Info("closing a connection: its key was revoked",
			"account", lc.account, "key", ssh.FingerprintSHA256(lc.key), "from", lc.conn.RemoteAddr())
		_ = lc.conn.Close()
	}
}
