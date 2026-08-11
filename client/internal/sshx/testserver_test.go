package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"
)

// A real SSH server, in process, for the client tests.
//
// gliderlabs/ssh is deliberately the same library the workspace agent will use
// (ADR 0010), so these tests exercise the pairing that ships rather than a
// mock that agrees with whatever the client happens to do.
type testServer struct {
	Addr     net.Addr
	HostKey  ssh.PublicKey
	listener net.Listener
	srv      *gssh.Server

	// silent makes the server accept global requests and never answer them,
	// which is what a link that has stopped carrying anything looks like from
	// this end. A server that is merely gone is a different test.
	silent atomic.Bool
}

// startTestServer runs a server that accepts any public key, echoes commands
// it understands, and permits both forward directions.
func startTestServer(t *testing.T) *testServer {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	forwardHandler := &gssh.ForwardedTCPHandler{}

	ts := &testServer{}
	srv := &gssh.Server{
		PublicKeyHandler: func(gssh.Context, gssh.PublicKey) bool { return true },

		// Both directions are permitted here; restricting them is the
		// workspace agent's job, not the transport's.
		ReversePortForwardingCallback: func(gssh.Context, string, uint32) bool { return true },
		LocalPortForwardingCallback:   func(gssh.Context, string, uint32) bool { return true },

		RequestHandlers: map[string]gssh.RequestHandler{
			"tcpip-forward":        forwardHandler.HandleSSHRequest,
			"cancel-tcpip-forward": forwardHandler.HandleSSHRequest,

			// Registered so that silence can be arranged. A request with no
			// handler is answered with a refusal, which is still an ANSWER and
			// so is not the case being tested: the probe wants to know what
			// happens when nothing comes back at all. Blocking here sends no
			// reply, and the request dies with the connection.
			"keepalive@openssh.com": func(ctx gssh.Context, _ *gssh.Server, _ *ssh.Request) (bool, []byte) {
				if ts.silent.Load() {
					<-ctx.Done()
					return false, nil
				}
				return true, nil
			},
		},
		ChannelHandlers: map[string]gssh.ChannelHandler{
			"session":      gssh.DefaultSessionHandler,
			"direct-tcpip": gssh.DirectTCPIPHandler,
		},

		Handler: func(s gssh.Session) {
			cmd := strings.Join(s.Command(), " ")
			switch cmd {
			case "":
				io.WriteString(s, "interactive\n")
			case "fail":
				io.WriteString(s.Stderr(), "something went wrong\n")
				s.Exit(3)
				return
			case "cat":
				// Bidirectional: used to prove OpenStream carries a
				// long-lived duplex stream, which is what the Docker API
				// needs from `docker system dial-stdio`.
				io.Copy(s, s)
			default:
				fmt.Fprintf(s, "ran: %s\n", cmd)
			}
			s.Exit(0)
		},
	}
	srv.AddHostKey(hostSigner)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ts.Addr = l.Addr()
	ts.HostKey = hostSigner.PublicKey()
	ts.listener = l
	ts.srv = srv
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return ts
}

// dial builds a client against the test server, with state under t.TempDir().
func (ts *testServer) dial(t *testing.T) *Client {
	t.Helper()
	return ts.dialWith(t, 0)
}

// dialWith is dial with a keepalive interval of its own, for the tests about
// what happens when probes go unanswered.
func (ts *testServer) dialWith(t *testing.T, keepAlive time.Duration) *Client {
	t.Helper()

	dir := t.TempDir()
	key, err := LoadOrCreateKey(dir+"/id_ed25519", "test")
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	kh, err := NewKnownHosts(dir + "/known_hosts")
	if err != nil {
		t.Fatalf("NewKnownHosts: %v", err)
	}

	host, portStr, _ := net.SplitHostPort(ts.Addr.String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	c, err := Dial(t.Context(), Config{
		Host:       host,
		Port:       port,
		User:       "tester",
		Key:        key,
		KnownHosts: kh,
		KeepAlive:  keepAlive,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
