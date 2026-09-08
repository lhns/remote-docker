package session

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core-client/tunnelclient"
	"github.com/lhns/remote-docker/core/cache"
)

// What a client does when the workspace at the other end is older than the
// channel it is asking for. x/crypto/ssh directly rather than the agent,
// because what is under test is a workspace this repository can no longer
// build.

// silentWorkspace is an SSH server that accepts a session, answers the exec
// request so the client believes the command started, and then does what
// answer says: its string goes out on stdout, and false means the server writes
// nothing and never exits. That is a v0.5.1 agent asked for workspace-cache,
// whose os/exec Wait blocks on a copy of a stdin nobody closes, so the exit
// status is never sent and the channel stays open with nothing on it.
type silentWorkspace struct {
	addr   net.Addr
	answer func(cmd string) (string, bool)
}

// startWorkspace runs one and connects to it, which is all any test here wants
// of it.
func startWorkspace(t *testing.T, answer func(cmd string) (string, bool)) *tunnelclient.Client {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	ws := &silentWorkspace{addr: l.Addr(), answer: answer}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go ws.serve(conn, cfg)
		}
	}()
	return ws.dial(t)
}

func (w *silentWorkspace) serve(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go w.session(ch, chReqs)
	}
}

func (w *silentWorkspace) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		// The command is a length-prefixed string (RFC 4254 section 6.5).
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			continue
		}
		_ = req.Reply(true, nil)

		out, exits := w.answer(payload.Command)
		if !exits {
			// Draining rather than returning: an agent stuck in cmd.Wait is
			// still reading, and a server that closed the channel instead would
			// give the client an EOF, which is a different case entirely.
			go func() { _, _ = io.Copy(io.Discard, ch) }()
			return
		}
		_, _ = io.WriteString(ch, out)
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		_ = ch.Close()
		return
	}
}

// dial connects to the workspace.
func (w *silentWorkspace) dial(t *testing.T) *tunnelclient.Client {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("client key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	host, portText, err := net.SplitHostPort(w.addr.String())
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	client, err := tunnelclient.Dial(t.Context(), tunnelclient.Config{
		Host:    host,
		Port:    port,
		User:    "alice",
		Signer:  signer,
		HostKey: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// helloFor is the greeting a workspace that DOES serve the cache channel sends.
func helloFor(t *testing.T) string {
	t.Helper()
	line, err := json.Marshal(cache.Reply{Hello: &cache.Hello{Version: cache.Version}})
	if err != nil {
		t.Fatal(err)
	}
	return string(line) + "\n"
}

// A workspace that accepts the command and then says nothing must cost the
// deadline and no more.
//
// On main this call never returns: greet did an unbounded ReadString on a
// stream with no deadline to set, against an agent whose own read of our stdin
// could not finish either. The client printed nothing and hung.
func TestAGreetingNobodySendsEndsAtTheDeadline(t *testing.T) {
	client := startWorkspace(t, func(string) (string, bool) { return "", false })

	// A deadline of its own, well under handshakeTimeout, so a regression is a
	// clean failure here rather than a test binary wedged until go test's own
	// ten minutes are up.
	ctx, cancel := context.WithTimeout(t.Context(), handshakeTimeout/5)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := openCache(ctx, client)
		done <- err
	}()

	select {
	case err := <-done:
		var silent *silentError
		if !errors.As(err, &silent) {
			t.Fatalf("openCache = %v, want a silentError", err)
		}
	case <-time.After(handshakeTimeout):
		t.Fatal("openCache did not return; the greeting read is unbounded again")
	}
}

// The common case against an older workspace: it answers the command with
// something that is not a greeting, and the session goes on working.
//
// Nothing is logged and nothing is printed here. That is the whole point: a
// workspace serving ordinary write=through mounts is not doing anything wrong,
// and telling somebody about a channel they never asked for is noise.
func TestACommandAnOlderWorkspaceDoesNotServe(t *testing.T) {
	// What `sh -c "workspace-cache"` leaves on stdout, which is nothing.
	client := startWorkspace(t, func(string) (string, bool) { return "", true })

	_, err := openCache(t.Context(), client)
	var notServed *notServedError
	if !errors.As(err, &notServed) {
		t.Fatalf("openCache = %v, want a notServedError", err)
	}
}

// A workspace that does serve it is unaffected by the deadline.
func TestAGreetingArrivesWithinTheDeadline(t *testing.T) {
	client := startWorkspace(t, func(string) (string, bool) { return helloFor(t), true })

	c, err := openCache(t.Context(), client)
	if err != nil {
		t.Fatalf("openCache: %v", err)
	}
	_ = c.Close()
}

// Nothing is left running behind a handshake that succeeded. The watchdog is a
// goroutine per greeting, and one that outlived its greeting would be one per
// connection for the life of the process.
func TestTheFastPathLeavesNoGoroutineBehind(t *testing.T) {
	client := startWorkspace(t, func(string) (string, bool) { return helloFor(t), true })

	// One first, so the connection's own goroutines are already up and are not
	// counted as ours.
	c, err := openCache(t.Context(), client)
	if err != nil {
		t.Fatalf("openCache: %v", err)
	}
	_ = c.Close()

	settle(t)
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		c, err := openCache(t.Context(), client)
		if err != nil {
			t.Fatalf("openCache %d: %v", i, err)
		}
		_ = c.Close()
	}
	settle(t)
	if grew := runtime.NumGoroutine() - before; grew > 5 {
		t.Errorf("20 handshakes left %d goroutines behind", grew)
	}
}

// settle waits for goroutines that have been asked to stop to actually stop.
func settle(t *testing.T) {
	t.Helper()
	for i := 0; i < 50; i++ {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
}

// The two refusals are different sentences, because only one of them is a
// statement about the workspace. Silence names no cause and must not be
// reported as an old workspace.
func TestARefusalSaysOnlyWhatWasEstablished(t *testing.T) {
	notServed := cacheRefusal(&notServedError{command: cache.Command}, "0.5.1")
	for _, want := range []string{
		"this workspace does not serve it",
		"it runs remote-dockerd 0.5.1",
		"fix: update the workspace, or use write=through",
	} {
		if !strings.Contains(notServed.Error(), want) {
			t.Errorf("a workspace that does not serve the channel: %v, want it to name %q", notServed, want)
		}
	}

	silent := cacheRefusal(&silentError{command: cache.Command, after: handshakeTimeout}, "0.5.1")
	if !strings.Contains(silent.Error(), "said nothing for 10s") {
		t.Errorf("a workspace that did not answer: %v, want it to say so", silent)
	}
	for _, unwanted := range []string{"0.5.1", "does not serve", "update the workspace"} {
		if strings.Contains(silent.Error(), unwanted) {
			t.Errorf("a workspace that did not answer: %v, must not claim %q", silent, unwanted)
		}
	}
}

// A version is CONTEXT and never the test, so a workspace that reports none is
// refused in the same words minus the clause.
func TestARefusalWithoutAVersionIsStillTheSameRefusal(t *testing.T) {
	err := cacheRefusal(&notServedError{command: cache.Command}, "")
	if !strings.Contains(err.Error(), "this workspace does not serve it") ||
		strings.Contains(err.Error(), "remote-dockerd") {
		t.Errorf("with no version reported: %v", err)
	}
}
