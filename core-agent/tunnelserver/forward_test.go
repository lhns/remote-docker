package tunnelserver

import (
	"net"
	"testing"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// One Forwards serves every connection, and a reservation is released on the
// same ctx.Done that takes the listener down, so a second session can hold the
// address by then. Dropping by name closed that one instead, leaving it bound
// with nothing accepting and no way to reach it again.
func TestDropLeavesASuccessorAlone(t *testing.T) {
	first, second := listen(t), listen(t)
	f := &Forwards{forwards: map[string]net.Listener{"127.0.0.1:2049": second}}

	f.drop("127.0.0.1:2049", first)

	if f.forwards["127.0.0.1:2049"] != second {
		t.Error("dropping one session's listener forgot the successor's")
	}
	// Closing an open listener succeeds; closing one already closed does not.
	if err := second.Close(); err != nil {
		t.Error("dropping one session's listener closed the successor's")
	}
}

// A listener that leaves serve for any reason but its own close is still
// bound, and the map entry is the only thing that could reach it.
func TestDropClosesTheListenerItForgets(t *testing.T) {
	ln := listen(t)
	f := &Forwards{forwards: map[string]net.Listener{"127.0.0.1:2049": ln}}

	f.drop("127.0.0.1:2049", ln)

	if _, ok := f.forwards["127.0.0.1:2049"]; ok {
		t.Error("the entry survived its listener")
	}
	if err := ln.Close(); err == nil {
		t.Error("drop returned without closing the listener")
	}
}
