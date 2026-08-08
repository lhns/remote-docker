package session

import (
	"io"
	"testing"

	"github.com/lhns/remote-docker/internal/client/nfsserve"
	"github.com/lhns/remote-docker/pkg/workspace"
)

// fakeStream stands in for the SSH stream a dial returns.
type fakeStream struct{ closed bool }

func (f *fakeStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (f *fakeStream) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeStream) Close() error                { f.closed = true; return nil }

type halfCloser struct {
	fakeStream
	wroteClose bool
}

func (h *halfCloser) CloseWrite() error { h.wroteClose = true; return nil }

// A stream must hold its lease for as long as it is open.
//
// The lease used to be released the instant the stream opened, so a hijacked
// connection -- attach, exec -it, logs -f -- held nothing. Those survived an
// idle release only because their container happened to be running; a
// `logs -f` on a stopped container would simply be cut.
func TestLeasedStreamHoldsUntilClosed(t *testing.T) {
	released := 0
	inner := &fakeStream{}
	s := &leasedStream{ReadWriteCloser: inner, release: func() { released++ }}

	if released != 0 {
		t.Fatal("the lease was released before the stream was closed")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !inner.closed {
		t.Error("closing the lease did not close the stream underneath it")
	}
	if released != 1 {
		t.Errorf("released %d times, want exactly 1", released)
	}
}

// Close can be reached twice -- a handler's defer and an explicit close -- and
// double-releasing a gate lease would drop the user count below zero and let a
// sweep close a connection still in use.
func TestLeasedStreamReleasesOnce(t *testing.T) {
	released := 0
	s := &leasedStream{ReadWriteCloser: &fakeStream{}, release: func() { released++ }}

	_ = s.Close()
	_ = s.Close()
	_ = s.Close()

	if released != 1 {
		t.Errorf("released %d times, want exactly 1", released)
	}
}

// The wrapper must not hide CloseWrite. The proxy half-closes the upstream
// when a client stops sending, and `docker run` without -i does exactly that
// the moment attach is established -- losing that signal is how the container's
// output disappears and the command exits 0 having printed nothing.
func TestLeasedStreamForwardsCloseWrite(t *testing.T) {
	inner := &halfCloser{}
	var s io.ReadWriteCloser = &leasedStream{ReadWriteCloser: inner, release: func() {}}

	cw, ok := s.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("the wrapper hides CloseWrite; the proxy's half-close would silently do nothing")
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if !inner.wroteClose {
		t.Error("CloseWrite did not reach the stream underneath")
	}
}

// A stream with no half-close of its own must not report a failure: the proxy
// treats "cannot half-close" as fine, and an error here would be reported as
// one.
func TestLeasedStreamCloseWriteWithoutSupport(t *testing.T) {
	s := &leasedStream{ReadWriteCloser: &fakeStream{}, release: func() {}}
	if err := s.CloseWrite(); err != nil {
		t.Errorf("CloseWrite on a stream without it = %v, want nil", err)
	}
}

// The volume match must name only volumes this session created.
//
// It used to accept any "rd-" prefix, so on a shared daemon (ADR 0012) another
// account's volume pinned this connection open forever: an idle release that
// could never fire, for a dependency that was not ours.
func TestOurVolumesNamesOnlyOurShares(t *testing.T) {
	s := &Session{registry: nfsserve.NewRegistry(defaultAttrs())}
	if _, err := s.registry.RegisterCWD(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if _, err := s.registry.Register(other); err != nil {
		t.Fatal(err)
	}

	ours := s.ourVolumes()
	if len(ours) != 2 {
		t.Fatalf("named %d volumes, want one per share: %v", len(ours), ours)
	}
	if !ours[workspace.VolumeNamePrefix+"cwd"] {
		t.Errorf("the working directory's volume is missing: %v", ours)
	}

	// Another account's managed volume carries the same prefix and must not
	// be claimed.
	theirs := workspace.VolumeNameForID(workspace.ShareID("/somebody/elses/project"))
	if ours[theirs] {
		t.Errorf("claimed %s, which belongs to another session", theirs)
	}
}
