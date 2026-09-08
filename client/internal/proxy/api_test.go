package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"testing"
	"time"
)

// silentDaemon accepts connections, reads whatever arrives and never answers.
// A workspace whose daemon has stopped responding looks exactly like this from
// here: the stream is open, the request goes out, nothing comes back.
func silentDaemon(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	return l.Addr().String()
}

// TestAPIRequestReturnsWhenTheContextExpires pins the deadline the context is
// supposed to carry. do writes the request by hand and reads it back with
// http.ReadResponse, so no Transport enforces that context and a daemon which
// says nothing blocks the caller forever.
func TestAPIRequestReturnsWhenTheContextExpires(t *testing.T) {
	client := &APIClient{Dialer: &tcpDialer{addr: silentDaemon(t)}}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.ListContainers(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a daemon that never answered")
		}
	case <-time.After(5 * time.Second):
		// Deliberately not left to the test binary's own deadline: a wedged
		// binary reports nothing about which call hung.
		t.Fatal("ListContainers did not return after its context expired")
	}
}

// TestAPIRequestLeavesNoWatchdogBehind: whatever enforces the deadline must end
// when the call does, not when the context is eventually cancelled. A session
// makes these calls every few seconds under one long-lived context.
func TestAPIRequestLeavesNoWatchdogBehind(t *testing.T) {
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		respondJSON(conn, http.StatusOK, `[]`)
	})
	client := &APIClient{Dialer: &tcpDialer{addr: daemon.listener.Addr().String()}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One call first, so anything the connection machinery starts once is
	// already in the baseline.
	if _, err := client.ListContainers(ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	settle()
	before := runtime.NumGoroutine()

	for range 20 {
		if _, err := client.ListContainers(ctx); err != nil {
			t.Fatalf("ListContainers: %v", err)
		}
	}
	settle()

	if grew := runtime.NumGoroutine() - before; grew > 5 {
		t.Fatalf("20 calls left %d goroutines behind (%d -> %d)", grew, before, runtime.NumGoroutine())
	}
}

// settle gives goroutines that are ending time to end, so a count taken after
// them is not a race with the runtime.
func settle() {
	for range 20 {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEventsStreamsAfterTheResponseHead: the deadline covers the request and
// the response head only. Events returns the connection to its caller and goes
// on decoding from it, so anything holding that connection past do -- or
// stopping by closing it -- ends the stream.
func TestEventsStreamsAfterTheResponseHead(t *testing.T) {
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n")
		time.Sleep(150 * time.Millisecond)
		fmt.Fprint(conn, `{"Action":"start"}`)
		time.Sleep(150 * time.Millisecond)
		fmt.Fprint(conn, `{"Action":"die"}`)
		time.Sleep(time.Second)
	})
	client := &APIClient{Dialer: &tcpDialer{addr: daemon.listener.Addr().String()}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, closer, err := client.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer closer.Close()

	for _, want := range []string{"start", "die"} {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("the event stream ended before %q", want)
			}
			if event.Action != want {
				t.Fatalf("got action %q, want %q", event.Action, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %q event arrived", want)
		}
	}
}
