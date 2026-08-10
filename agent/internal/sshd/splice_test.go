package sshd

import (
	"io"
	"sync"
	"testing"
	"time"
)

// clientSide models the SSH session as the client actually behaves.
//
// The important property is what it does NOT do: it never closes its own write
// half on its own initiative. That is a Windows named pipe, which has no
// half-close at all -- and a Docker client attached to a container it is not
// feeding stdin to simply sits there. Only being told the stream is over makes
// it go away.
type clientSide struct {
	mu       sync.Mutex
	done     chan struct{}
	closed   bool
	gotClose bool
}

func newClientSide() *clientSide { return &clientSide{done: make(chan struct{})} }

// Read blocks until the far side signals end-of-input, exactly as a client
// with nothing to send does.
func (c *clientSide) Read([]byte) (int, error) {
	<-c.done
	return 0, io.EOF
}

func (c *clientSide) Write(p []byte) (int, error) { return len(p), nil }

// CloseWrite is the signal under test. Receiving it is what lets a real client
// finish and close, which is modelled here by unblocking Read.
func (c *clientSide) CloseWrite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gotClose = true
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	return nil
}

func (c *clientSide) told() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gotClose
}

// daemonSide models the Docker daemon's end of an attach: the container has
// exited, so there is nothing more to read.
type daemonSide struct{ closedWrite bool }

func (d *daemonSide) Read([]byte) (int, error)    { return 0, io.EOF }
func (d *daemonSide) Write(p []byte) (int, error) { return len(p), nil }
func (d *daemonSide) Close() error                { return nil }
func (d *daemonSide) CloseWrite() error           { d.closedWrite = true; return nil }

// splice must tell the client when the daemon is done, without waiting for the
// client to speak first.
//
// Leaving that signal out is a deadlock, not an untidiness: this side waited
// for the client to half-close while the client waited for a stream that would
// never say anything again. It was broken only by a ~90 second timeout, so
// `docker run` took a minute and a half to return from a container that had
// finished in one second.
//
// It passed CI throughout, because the signal is only load-bearing when the
// client cannot half-close by itself. A unix socket can, so the Linux client
// unwound it every time.
func TestSpliceSignalsEndOfInputWhenTheDaemonFinishes(t *testing.T) {
	client := newClientSide()
	daemon := &daemonSide{}

	done := make(chan struct{})
	go func() {
		splice(client, daemon)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("splice did not return: it is waiting for the client to " +
			"half-close while the client waits to be told the stream is over")
	}

	if !client.told() {
		t.Error("the client was never told the daemon had finished")
	}
	if !daemon.closedWrite {
		t.Error("the daemon was never told the client had finished")
	}
}

// The direction that already worked must keep working: a client that closes
// its own write half still ends the daemon's read.
func TestSpliceStillSignalsTheDaemon(t *testing.T) {
	client := newClientSide()
	_ = client.CloseWrite() // as a unix-socket client does on its own
	daemon := &daemonSide{}

	done := make(chan struct{})
	go func() {
		splice(client, daemon)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("splice did not return")
	}
	if !daemon.closedWrite {
		t.Error("the daemon was never told the client had finished")
	}
}
