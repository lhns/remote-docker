package tunnelclient

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// A caller with no host key rule has no host key rule, and that must be said
// rather than discovered.
//
// x/crypto refuses a nil callback too, but it does so mid-handshake, and the
// error reads as a connection that went wrong rather than as a configuration
// that accepts anybody. The other direction -- defaulting to accepting any host
// -- is the failure that succeeds, so there is no default at all.
func TestDialRefusesNoHostKeyPolicy(t *testing.T) {
	ts := startTestServer(t)
	host, port := hostPort(t, ts.Addr.String())

	_, err := Dial(t.Context(), Config{Host: host, Port: port, User: "tester"})
	if err == nil {
		t.Fatal("a config with no host key callback connected")
	}
	if !strings.Contains(err.Error(), "HostKey") {
		t.Errorf("the error does not name what is missing: %v", err)
	}
}

func TestRun(t *testing.T) {
	c := startTestServer(t).dial(t)

	out, err := c.Run(t.Context(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "ran: hello" {
		t.Errorf("Run() = %q, want %q", got, "ran: hello")
	}
}

// A remote command that fails with a message on stderr and nothing on stdout
// is the common case. The shell clients threw that message away and reported
// an exit code, which is how they produced unhelpful failures.
func TestRunFoldsStderrIntoTheError(t *testing.T) {
	c := startTestServer(t).dial(t)

	_, err := c.Run(t.Context(), "fail")
	if err == nil {
		t.Fatal("Run() = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("error %q does not carry the remote stderr", err)
	}
}

// OpenStream must carry a long-lived duplex stream: it is how the Docker API
// travels, via `docker system dial-stdio`.
func TestOpenStreamIsBidirectional(t *testing.T) {
	c := startTestServer(t).dial(t)

	stream, err := c.OpenStream("cat")
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close()

	const msg = "the quick brown fox\n"
	if _, err := io.WriteString(stream, msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != msg {
		t.Errorf("echoed %q, want %q", buf, msg)
	}
}

// Listen is ssh -R: the reverse forward that carries the client's NFS export
// into the workspace. Prove a connection made on the far side arrives here.
func TestListenCarriesRemoteConnections(t *testing.T) {
	ts := startTestServer(t)
	c := ts.dial(t)

	l, err := c.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	const payload = "from the workspace"
	got := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			got <- "accept error: " + err.Error()
			return
		}
		defer conn.Close()
		b, _ := io.ReadAll(conn)
		got <- string(b)
	}()

	// Dial the forwarded address the way something inside the workspace
	// would. The test server shares this process, so its loopback is ours.
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dialling the forwarded port: %v", err)
	}
	io.WriteString(conn, payload)
	conn.Close()

	if v := <-got; v != payload {
		t.Errorf("reverse forward delivered %q, want %q", v, payload)
	}
}

// Forward is ssh -L: how a published container port becomes reachable at the
// same address on this machine.
func TestForwardCarriesLocalConnections(t *testing.T) {
	c := startTestServer(t).dial(t)

	// Stand in for a service inside the workspace.
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer target.Close()
	go func() {
		for {
			conn, err := target.Accept()
			if err != nil {
				return
			}
			io.Copy(conn, conn)
			conn.Close()
		}
	}()

	fwd, err := c.Forward("127.0.0.1:0", target.Addr().String())
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer fwd.Close()

	conn, err := net.Dial("tcp", fwd.Local.String())
	if err != nil {
		t.Fatalf("dialling the forward: %v", err)
	}
	defer conn.Close()

	// Read a known number of bytes rather than to EOF. Half-close does not
	// survive gliderlabs' direct-tcpip handler, which tears down both
	// directions as soon as either copy ends, a property of the test
	// server, not of the forward under test.
	const msg = "round trip"
	if _, err := io.WriteString(conn, msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	b := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, b); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != msg {
		t.Errorf("forward delivered %q, want %q", b, msg)
	}
}

// Asking for port 0 gets a kernel-chosen port, so Local must report what was
// actually bound. Callers show this to the user and it has to be true.
func TestForwardReportsTheBoundAddress(t *testing.T) {
	c := startTestServer(t).dial(t)

	fwd, err := c.Forward("127.0.0.1:0", "127.0.0.1:1")
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer fwd.Close()

	_, port, err := net.SplitHostPort(fwd.Local.String())
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", fwd.Local, err)
	}
	if port == "0" || port == "" {
		t.Errorf("Local = %q, want a concrete bound port", fwd.Local)
	}
}

// A port already in use must fail rather than be quietly remapped: a listener
// at an address nobody asked for looks like success and breaks the next thing
// that expects the real one.
func TestForwardRefusesAPortInUse(t *testing.T) {
	c := startTestServer(t).dial(t)

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer occupied.Close()

	if fwd, err := c.Forward(occupied.Addr().String(), "127.0.0.1:1"); err == nil {
		fwd.Close()
		t.Error("Forward() onto a bound port = nil error, want an error")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c := startTestServer(t).dial(t)
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// A connection that goes away is known to have gone.
//
// Noticing is not enough on its own: the keepalive can close the client and
// tell nobody, and whatever holds the connection then keeps handing out the
// corpse. Dead is how the holder learns it must open another.
func TestDeadClosesWhenTheTransportGoesAway(t *testing.T) {
	ts := startTestServer(t)
	cut := startCutter(t, ts.Addr)
	c := ts.dialAddr(t, cut.Addr, 0)

	if !c.Alive() {
		t.Fatal("a fresh connection reported itself dead")
	}

	cut.cut()

	select {
	case <-c.Dead():
	case <-time.After(10 * time.Second):
		t.Fatal("the connection did not notice the transport going away")
	}
	if c.Alive() {
		t.Error("Alive still says yes after Dead closed")
	}
}

// Closing here is a death like any other, so a holder waiting on Dead is woken
// rather than left waiting for a keepalive that will never run again.
func TestDeadClosesOnClose(t *testing.T) {
	ts := startTestServer(t)
	c := ts.dial(t)

	_ = c.Close()
	select {
	case <-c.Dead():
	case <-time.After(time.Second):
		t.Fatal("Dead did not close when the client did")
	}
}

// The case the probe exists for: a link that stopped carrying anything without
// breaking.
//
// A server that accepts the request and never answers leaves SendRequest
// blocked until TCP gives up, which is minutes, while KeepAlive promises
// seconds. The probe waits on its own clock instead.
func TestKeepAliveGivesUpOnAnUnansweredProbe(t *testing.T) {
	ts := startTestServer(t)
	ts.silent.Store(true)

	c := ts.dialAddr(t, ts.Addr, 50*time.Millisecond)

	select {
	case <-c.Dead():
	case <-time.After(5 * time.Second):
		t.Fatal("an unanswered keepalive never gave up")
	}
}
