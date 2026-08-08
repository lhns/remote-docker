package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
)

type fakeControl struct {
	status    any
	shutdowns int
}

func (f *fakeControl) Status() any { return f.status }
func (f *fakeControl) Shutdown()   { f.shutdowns++ }

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
	if ctrl.shutdowns != 1 {
		t.Errorf("Shutdown called %d times, want 1", ctrl.shutdowns)
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
	if ctrl.shutdowns != 0 {
		t.Error("a GET stopped the daemon")
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
