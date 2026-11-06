package session

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/client/internal/rewrite"
	"github.com/lhns/remote-docker/core/workspace"
)

// echoOn serves one TCP echo server on the loopback port and returns it.
func echoOn(t *testing.T, network, addr string) net.Listener {
	t.Helper()
	l, err := net.Listen(network, addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = c.Close() }(); _, _ = io.Copy(c, c) }()
		}
	}()
	return l
}

// freePort is a loopback port nothing holds right now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// What a container sends to the workspace's listener arrives at the port this
// listener was registered for and nowhere else, and comes back.
func TestAHostPortConnectionReachesLocalhostOnItsOwnPort(t *testing.T) {
	local := echoOn(t, "tcp", "127.0.0.1:0")
	other := echoOn(t, "tcp", "127.0.0.1:0")
	port := local.Addr().(*net.TCPAddr).Port

	// Stands in for the listener the tunnel hands back.
	tunnelEnd, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var served sync.WaitGroup
	served.Go(func() { serveHostPort(ctx, tunnelEnd, port, func(string, ...any) {}) })
	defer func() { cancel(); _ = tunnelEnd.Close(); served.Wait() }()

	// Anything arriving on the listener is carried, whatever it asked for.
	c, err := net.Dial("tcp", tunnelEnd.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}

	// The other local server was never touched: the destination is the
	// registered port, never one from the connection.
	_ = other.(*net.TCPListener).SetDeadline(time.Now().Add(50 * time.Millisecond))
	if conn, err := other.Accept(); err == nil {
		_ = conn.Close()
		t.Error("a connection reached a port the listener was not registered for")
	}
}

// A server that listens on ::1 only is reached too, which is what localhost
// is dialled for.
func TestAHostPortReachesAnIPv6OnlyServer(t *testing.T) {
	local := echoOn(t, "tcp6", "[::1]:0")
	port := local.Addr().(*net.TCPAddr).Port

	tunnelEnd, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var served sync.WaitGroup
	served.Go(func() { serveHostPort(t.Context(), tunnelEnd, port, func(string, ...any) {}) })
	defer func() { _ = tunnelEnd.Close(); served.Wait() }()

	c, err := net.Dial("tcp", tunnelEnd.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("v6"))
	buf := make([]byte, 2)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "v6" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
}

// A port nothing listens on here closes the container's connection, and says
// so once however often the container retries.
func TestAClosedLocalPortClosesTheConnectionAndWarnsOnce(t *testing.T) {
	port := freePort(t)
	tunnelEnd, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var warnings []string
	var served sync.WaitGroup
	served.Go(func() {
		serveHostPort(t.Context(), tunnelEnd, port, func(msg string, _ ...any) {
			mu.Lock()
			warnings = append(warnings, msg)
			mu.Unlock()
		})
	})

	for range 3 {
		c, err := net.Dial("tcp", tunnelEnd.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err != io.EOF {
			t.Errorf("read = %v, want the connection closed", err)
		}
		_ = c.Close()
	}
	_ = tunnelEnd.Close()
	served.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "nothing answers") {
		t.Errorf("warnings = %q, want exactly one naming the closed port", warnings)
	}
}

func TestOnlyAnIPv4AddressIsAHostGateway(t *testing.T) {
	for in, want := range map[string]string{
		"172.17.0.1":    "172.17.0.1",
		"fd00::1":       "",
		"localhost":     "",
		"172.17.0.1/16": "",
	} {
		s := &Session{}
		// A gateway already reported is never asked for again, so no
		// connection is needed.
		live := &liveConn{info: workspace.Info{HostGateway: in}}
		if got := s.hostGateway(t.Context(), live); got != want {
			t.Errorf("hostGateway(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusNamesEachHostPort(t *testing.T) {
	live := &liveConn{hostForwards: []hostForward{
		{port: 3000},
		{port: 8931, problem: "the workspace refused 172.17.0.1:8931"},
	}}
	got := live.hostForwardStatus()
	want := []string{
		"3000: containers reach it as host.docker.internal:3000",
		"8931: not forwarded, the workspace refused 172.17.0.1:8931",
	}
	if !slices.Equal(got, want) {
		t.Errorf("status = %q, want %q", got, want)
	}
}

// The idle release must not drop the connection under a running container that
// uses a host port: that container holds no gate lease. A container made
// through this client carries the account and machine labels even with no
// mounts at all, and hasLiveDependents counts any such running container.
func TestARunningContainerWithNoMountsKeepsTheConnection(t *testing.T) {
	r := &rewrite.Rewriter{Owner: "alice", Client: "machine-1"}
	r.SetHostGateway("172.17.0.1")
	out, err := r.ContainerCreate(t.Context(), []byte(`{"Image":"alpine","HostConfig":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct{ Labels map[string]string }
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatal(err)
	}

	s, err := Open(t.Context(), Options{Config: config.Config{Host: "unused", Port: 1, User: "alice"}, Role: Query})
	if err != nil {
		t.Skipf("cannot open a session offline: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	live := &liveConn{
		info:     workspace.Info{User: "alice"},
		clientID: "machine-1",
		api:      &proxy.APIClient{Dialer: listingDaemon{containers: []proxy.Container{{ID: "c1", Labels: created.Labels}}}},
	}
	busy, err := s.hasLiveDependents(t.Context(), live)
	if err != nil || !busy {
		t.Errorf("busy = %v, %v for a running container with labels %v, want busy", busy, err, created.Labels)
	}
}

// Removing a port closes its listener and keeps the others; emptying the list
// turns the container alias off. Everything still listed is already bound, so
// no connection is needed.
func TestChangingTheListClosesWhatWasRemoved(t *testing.T) {
	open := func() net.Listener {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		return l
	}
	l3000, l8931 := open(), open()
	rw := &rewrite.Rewriter{}
	rw.SetHostGateway("172.17.0.1")
	live := &liveConn{
		info:     workspace.Info{HostGateway: "172.17.0.1"},
		rewriter: rw,
		hostForwards: []hostForward{
			{port: 3000, listener: l3000},
			{port: 8931, listener: l8931},
		},
	}
	s := &Session{}

	s.applyHostPorts(t.Context(), live, []int{8931})
	if _, err := l3000.Accept(); err == nil {
		t.Error("the removed port's listener is still open")
	}
	if status := live.hostForwardStatus(); len(status) != 1 || !strings.HasPrefix(status[0], "8931:") {
		t.Errorf("status = %q, want only 8931", status)
	}
	if rw.HostGateway() != "172.17.0.1" {
		t.Errorf("the alias went off with a port still listed")
	}

	s.applyHostPorts(t.Context(), live, nil)
	if _, err := l8931.Accept(); err == nil {
		t.Error("the last listener is still open")
	}
	if rw.HostGateway() != "" || len(live.hostForwardStatus()) != 0 {
		t.Errorf("an empty list left the alias %q and status %q", rw.HostGateway(), live.hostForwardStatus())
	}

	// A closed connection takes no more changes.
	live.closeHostForwards()
	s.applyHostPorts(t.Context(), live, []int{8931})
	if len(live.hostForwardStatus()) != 0 {
		t.Error("a closed connection bound a port")
	}
}
