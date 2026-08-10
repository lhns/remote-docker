//go:build linux

package netns

import (
	"strings"
	"testing"
	"time"
)

// A namespace that cannot be opened must fail rather than hang.
//
// Do runs its work on a goroutine and reports through a channel, so a mistake
// in the failure paths presents as a deadlock rather than as an error -- and a
// deadlocked session is far harder to read than a refused one.
func TestDoReportsAnUnopenableNamespace(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- Do("/proc/0/ns/net", func() error { return nil })
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("entering a namespace that does not exist succeeded")
		}
		if !strings.Contains(err.Error(), "netns:") {
			t.Errorf("error does not name the package: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Do hung on a namespace it could not open")
	}
}

// Listen and Dial must fail the same way rather than returning a nil listener
// with a nil error, which would be found later as a nil dereference somewhere
// unrelated.
func TestListenAndDialFailClosed(t *testing.T) {
	if l, err := Listen("/proc/0/ns/net", "tcp", "127.0.0.1:0"); err == nil {
		_ = l.Close()
		t.Error("Listen succeeded in a namespace that could not be opened")
	} else if l != nil {
		t.Error("Listen returned a listener alongside an error")
	}

	if c, err := Dial("/proc/0/ns/net", "tcp", "127.0.0.1:1"); err == nil {
		_ = c.Close()
		t.Error("Dial succeeded in a namespace that could not be opened")
	} else if c != nil {
		t.Error("Dial returned a connection alongside an error")
	}
}
