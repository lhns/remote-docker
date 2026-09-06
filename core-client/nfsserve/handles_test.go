package nfsserve

// The root handle, which is the one the kernel cannot ask for a second time.
//
// These are the E3 failure in miniature: a handle issued by one server, offered
// to a server that has never seen it. Section 6 of test/nfs-resilience.sh is the
// same question asked of a real kernel through a real container.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/lhns/remote-docker/core/logx"
	nfs "github.com/willscott/go-nfs"
)

// rootHandleOf is what MOUNT hands the kernel: ToHandle for the empty path.
func rootHandleOf(t *testing.T, s *Server, export string) []byte {
	t.Helper()
	share, _, ok := s.registry.Lookup(export)
	if !ok {
		t.Fatalf("no share for %s", export)
	}
	h := s.handler.ToHandle(share.fs, []string{})
	if len(h) == 0 {
		t.Fatal("empty root handle")
	}
	return h
}

// The whole point: a restarted client is a NEW process with a new server and an
// empty cache, and the mount it inherits has to keep working.
func TestRootHandleResolvesInAServerThatNeverIssuedIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := registryFor(t, dir)

	handle := rootHandleOf(t, New(r, nil), "/cwd")

	// The client restarts: a second server, over a registry rebuilt the way a
	// reconnect rebuilds it.
	fresh := New(registryFor(t, dir), nil)
	fs, path, err := fresh.handler.FromHandle(handle)
	if err != nil {
		t.Fatalf("FromHandle in a new server: %v", err)
	}
	if len(path) != 0 {
		t.Errorf("path = %v, want the share root", path)
	}
	if _, err := fs.Stat("marker"); err != nil {
		t.Errorf("the resolved filesystem is not the share: %v", err)
	}
}

// The DERIVED part must agree between processes. The whole handle does not and
// must not: it carries the cache's own answer in front of nothing, so that a
// live process resolves a root exactly as it did before any of this existed,
// and the derived key answers only once that cache is gone.
func TestTheDerivedPartOfARootHandleIsTheSameInEveryServer(t *testing.T) {
	dir := t.TempDir()
	r := registryFor(t, dir)

	first := rootHandleOf(t, New(r, nil), "/cwd")
	second := rootHandleOf(t, New(registryFor(t, dir), nil), "/cwd")

	if string(first[:exportKeySize]) != string(second[:exportKeySize]) {
		t.Errorf("derived keys differ: %x vs %x", first[:exportKeySize], second[:exportKeySize])
	}
	if string(first) == string(second) {
		t.Error("the whole handle matched, so the cache's answer is not in it")
	}
}

// A handle is a capability the workspace may name, never a path it may supply
// (ADR 0027). A share that is no longer exported must not come back.
func TestRootHandleForAnUnexportedShareIsStale(t *testing.T) {
	dir := t.TempDir()
	handle := rootHandleOf(t, New(registryFor(t, dir), nil), "/cwd")

	empty := New(NewRegistry(DefaultAttrs), nil)
	if _, _, err := empty.handler.FromHandle(handle); err == nil {
		t.Error("a handle resolved against a registry that exports nothing")
	}
}

// Two shares must not share a handle, or a mount of one serves the other.
func TestRootHandlesDifferPerShare(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()

	r := registryFor(t, first)
	other, err := r.Register(second)
	if err != nil {
		t.Fatal(err)
	}
	s := New(r, nil)

	if string(rootHandleOf(t, s, "/cwd")) == string(rootHandleOf(t, s, other.ExportPath)) {
		t.Error("two shares were given the same root handle")
	}
}

// A subdirectory mount resolves against the subdirectory. Handing it the
// share's root handle would serve the wrong directory to a client that asked
// correctly -- and it would look like it worked.
func TestASubdirectoryMountDoesNotTakeTheShareRootHandle(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := registryFor(t, dir)
	s := New(r, nil)

	share, _, ok := r.Lookup("/cwd")
	if !ok {
		t.Fatal("no share")
	}
	sub, err := share.fs.Chroot("/sub")
	if err != nil {
		t.Fatal(err)
	}

	root := rootHandleOf(t, s, "/cwd")
	subHandle := s.handler.ToHandle(sub, []string{})
	if string(subHandle) == string(root) {
		t.Error("a subdirectory mount was given the share's root handle")
	}
}

// A live process answers a root from its cache, because that is the path every
// mount uses for as long as the client runs. The derived key is the fallback
// behind it, not a replacement for it.
func TestALiveServerResolvesItsOwnRootThroughTheCache(t *testing.T) {
	dir := t.TempDir()
	s := New(registryFor(t, dir), nil)
	handle := rootHandleOf(t, s, "/cwd")

	fs, path, err := s.handler.FromHandle(handle)
	if err != nil {
		t.Fatalf("FromHandle: %v", err)
	}
	if len(path) != 0 {
		t.Errorf("path = %v, want the share root", path)
	}
	if _, err := fs.Stat("."); err != nil {
		t.Errorf("the resolved filesystem is not usable: %v", err)
	}
}

// An unrecognised handle is stale rather than a panic or a wrong file: one
// from an older build has to degrade to "look it up again".
func TestAnUnknownHandleIsStale(t *testing.T) {
	s := New(registryFor(t, t.TempDir()), nil)
	for _, h := range [][]byte{nil, {}, {0xff, 1, 2, 3}, make([]byte, rootHandleSize)} {
		if _, _, err := s.handler.FromHandle(h); err == nil {
			t.Errorf("FromHandle(%x) succeeded, want an error", h)
		}
	}
}

// A root handle must keep the length this code recognises it by, and an
// ordinary handle must keep the bytes go-nfs gave it. Both are pinned here
// because a mount succeeds either way: the damage shows up only as reads
// failing against a real kernel, which no unit test sees.
func TestHandleSizes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := registryFor(t, dir)
	s := New(r, nil)

	if got := len(rootHandleOf(t, s, "/cwd")); got != rootHandleSize {
		t.Errorf("root handle is %d bytes, want %d", got, rootHandleSize)
	}

	share, _, _ := r.Lookup("/cwd")
	if got := len(s.handler.ToHandle(share.fs, []string{"marker"})); got != cachedHandleSize {
		t.Errorf("an ordinary handle is %d bytes, want the %d go-nfs mints", got, cachedHandleSize)
	}
}

// forgetfulHandler is a Handler that cannot move a cached handle, and records
// what it was asked to forget instead.
type forgetfulHandler struct {
	nfs.Handler
	handle      []byte
	invalidated [][]byte
}

func (f *forgetfulHandler) ToHandle(billy.Filesystem, []string) []byte { return f.handle }

func (f *forgetfulHandler) InvalidateHandle(_ billy.Filesystem, h []byte) error {
	f.invalidated = append(f.invalidated, h)
	return nil
}

// A rename the embedded handler cannot follow must INVALIDATE the source
// handle, which is what go-nfs does for a handler with no Rename of its own.
// It cannot do it here: rootHandler always implements the interface, so
// go-nfs never reaches its own fallback, and returning nil left the cache
// naming a path the file no longer has.
func TestRenameInvalidatesWhenTheHandlerCannotMoveAHandle(t *testing.T) {
	r := registryFor(t, t.TempDir())
	share, _, ok := r.Lookup("/cwd")
	if !ok {
		t.Fatal("no share")
	}
	stub := &forgetfulHandler{handle: make([]byte, cachedHandleSize)}
	h := &rootHandler{Handler: stub, registry: r, log: logx.Or(nil)}

	if err := h.Rename(share.fs, []string{"old"}, share.fs, []string{"new"}); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if len(stub.invalidated) != 1 || string(stub.invalidated[0]) != string(stub.handle) {
		t.Errorf("the source handle was invalidated %d times (%x); a client keeps resolving the old path",
			len(stub.invalidated), stub.invalidated)
	}
}
