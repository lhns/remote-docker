package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// shutdowns is atomic because Shutdown is called from the connection handler
// AFTER the response has been written -- deliberately, since it closes the very
// connection carrying the request. So the test observes it from one goroutine
// while the proxy increments it on another.
type fakeControl struct {
	status    any
	safe      bool
	shutdowns atomic.Int64
}

func (f *fakeControl) Status() any { return f.status }
func (f *fakeControl) Idle() any   { return Idle{Safe: f.safe, Quiet: "1m0s"} }
func (f *fakeControl) Shutdown()   { f.shutdowns.Add(1) }

// waitForShutdown polls rather than reading once, for the same reason: the
// acknowledgement arrives before the action, so a bare read races the handler
// and would pass or fail on timing.
func (f *fakeControl) waitForShutdown(t *testing.T) int64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n := f.shutdowns.Load(); n > 0 {
			return n
		}
		time.Sleep(time.Millisecond)
	}
	return f.shutdowns.Load()
}

// A control request must never reach the workspace. Forwarding it would ask a
// daemon that has never heard of it, and the user would get a bewildering 404
// attributed to Docker.
func TestControlIsAnsweredLocally(t *testing.T) {
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		respondJSON(conn, http.StatusOK, `{"reached":"workspace"}`)
	})
	ctrl := &fakeControl{status: Status{Workspace: "dev", Connected: true}}
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{addr: daemon.listener.Addr().String()}, Control: ctrl})

	resp, err := http.Get("http://" + addr + ControlPrefix + "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if len(daemon.recorded()) != 0 {
		t.Errorf("a control request was forwarded to the workspace: %+v", daemon.recorded())
	}
	var got Status
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if got.Workspace != "dev" || !got.Connected {
		t.Errorf("status = %+v, want the session's own report", got)
	}
}

// Shutdown closes the very connection carrying the request, so the answer has
// to be written before the stopping starts or the caller sees an unexplained
// EOF instead of an acknowledgement.
func TestShutdownIsAcknowledgedBeforeActing(t *testing.T) {
	ctrl := &fakeControl{}
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{addr: "127.0.0.1:1"}, Control: ctrl})

	resp, err := http.Post("http://"+addr+ControlPrefix+"shutdown", "", nil)
	if err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("shutdown returned %d, want 200", resp.StatusCode)
	}
	if n := ctrl.waitForShutdown(t); n != 1 {
		t.Errorf("Shutdown called %d times, want 1", n)
	}
}

// GET must not stop a daemon: a stray browser or a probe would take the
// session down.
func TestShutdownRefusesGet(t *testing.T) {
	ctrl := &fakeControl{}
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{addr: "127.0.0.1:1"}, Control: ctrl})

	resp, err := http.Get("http://" + addr + ControlPrefix + "shutdown")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET shutdown returned %d, want 405", resp.StatusCode)
	}
	// Given a moment to be wrong: an immediate read would pass even if a GET
	// did trigger a shutdown on another goroutine.
	time.Sleep(100 * time.Millisecond)
	if n := ctrl.shutdowns.Load(); n != 0 {
		t.Errorf("a GET stopped the daemon (%d shutdowns)", n)
	}
}

// A session that is not the daemon must say so rather than pretend.
func TestControlAbsentWhenNotADaemon(t *testing.T) {
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{addr: "127.0.0.1:1"}})

	resp, err := http.Get("http://" + addr + ControlPrefix + "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status without a Control returned %d, want 404", resp.StatusCode)
	}
}

// The prefix must not shadow anything the Engine API serves.
func TestControlPrefixCannotCollide(t *testing.T) {
	for _, path := range []string{
		"/v1.51/containers/create", "/containers/json", "/_ping",
		"/v1.51/images/json", "/events", "/version", "/info",
	} {
		req := &http.Request{URL: mustURL(t, path)}
		if isControl(req) {
			t.Errorf("%q was treated as a control path", path)
		}
	}
	for _, path := range []string{ControlPrefix + "status", ControlPrefix + "shutdown"} {
		req := &http.Request{URL: mustURL(t, path)}
		if !isControl(req) {
			t.Errorf("%q was not treated as a control path", path)
		}
	}
}

func mustURL(t *testing.T, path string) *url.URL {
	t.Helper()
	u, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The version a daemon reports is what lets a freshly updated client notice it
// is talking to an older build. Without it a stale daemon makes a new binary
// behave like the old one, silently.
func TestStatusCarriesTheVersion(t *testing.T) {
	ctrl := &fakeControl{status: Status{Version: "sha-abc1234", Workspace: "dev"}}
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{addr: "127.0.0.1:1"}, Control: ctrl})

	resp, err := http.Get("http://" + addr + ControlPrefix + "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer resp.Body.Close()

	var got Status
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Version != "sha-abc1234" {
		t.Errorf("version = %q, want the daemon's build", got.Version)
	}
}

// Whether a restart would lose anything is a separate question from what the
// daemon is, because answering it costs a round trip to the workspace and is
// only needed when the versions actually differ.
func TestIdleReportsWhetherARestartIsSafe(t *testing.T) {
	for _, safe := range []bool{true, false} {
		ctrl := &fakeControl{safe: safe}
		addr := startProxy(t, &Proxy{Dialer: &tcpDialer{addr: "127.0.0.1:1"}, Control: ctrl})

		resp, err := http.Get("http://" + addr + ControlPrefix + "idle")
		if err != nil {
			t.Fatalf("idle: %v", err)
		}
		var got Idle
		err = json.NewDecoder(resp.Body).Decode(&got)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if got.Safe != safe {
			t.Errorf("safe = %v, want %v", got.Safe, safe)
		}
	}
}
