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
)

func registryFor(t *testing.T, dir string) *Registry {
	t.Helper()
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	return r
}

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

	handle := rootHandleOf(t, New(r), "/cwd")

	// The client restarts: a second server, over a registry rebuilt the way a
	// reconnect rebuilds it.
	fresh := New(registryFor(t, dir))
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

// Two servers must agree, or the handle depends on which process minted it,
// which is the bug.
func TestRootHandleIsTheSameInEveryServer(t *testing.T) {
	dir := t.TempDir()
	r := registryFor(t, dir)

	first := string(rootHandleOf(t, New(r), "/cwd"))
	second := string(rootHandleOf(t, New(registryFor(t, dir)), "/cwd"))
	if first != second {
		t.Errorf("root handles differ between servers: %x vs %x", first, second)
	}
}

// A handle is a capability the workspace may name, never a path it may supply
// (ADR 0027). A share that is no longer exported must not come back.
func TestRootHandleForAnUnexportedShareIsStale(t *testing.T) {
	dir := t.TempDir()
	handle := rootHandleOf(t, New(registryFor(t, dir)), "/cwd")

	empty := New(NewRegistry(DefaultAttrs))
	if _, _, err := empty.handler.FromHandle(handle); err == nil {
		t.Error("a handle resolved against a registry that exports nothing")
	}
}

// Two shares must not share a handle, or a mount of one serves the other.
func TestRootHandlesDifferPerShare(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()

	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(first); err != nil {
		t.Fatal(err)
	}
	other, err := r.Register(second)
	if err != nil {
		t.Fatal(err)
	}
	s := New(r)

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
	s := New(r)

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

// An unknown tag is stale rather than a panic or a wrong file: a handle from
// an older build has to degrade to "look it up again".
func TestAnUnknownHandleIsStale(t *testing.T) {
	s := New(registryFor(t, t.TempDir()))
	for _, h := range [][]byte{nil, {}, {0xff, 1, 2, 3}, {tagRoot}, {tagRoot, 1, 2}} {
		if _, _, err := s.handler.FromHandle(h); err == nil {
			t.Errorf("FromHandle(%x) succeeded, want an error", h)
		}
	}
}
