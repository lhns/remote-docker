package nfsserve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	return NewRegistry(DefaultAttrs)
}

func TestRegisterCWD(t *testing.T) {
	dir := t.TempDir()
	r := newTestRegistry(t)

	share, err := r.RegisterCWD(dir)
	if err != nil {
		t.Fatalf("RegisterCWD: %v", err)
	}
	if share.ExportPath != workspace.ExportCWD {
		t.Errorf("ExportPath = %q, want %q", share.ExportPath, workspace.ExportCWD)
	}
}

// Registering the same directory twice must not create a second share: the
// share id is derived from the path precisely so a reconnecting client reuses
// its handles and its remote volumes rather than orphaning a set per session.
func TestRegisterIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	r := newTestRegistry(t)

	first, err := r.Register(dir)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	second, err := r.Register(dir)
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}
	if first.ExportPath != second.ExportPath {
		t.Errorf("same directory produced %q then %q", first.ExportPath, second.ExportPath)
	}
	if got := len(r.Shares()); got != 1 {
		t.Errorf("registry holds %d shares, want 1", got)
	}
}

func TestRegisterDistinctDirectories(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	r := newTestRegistry(t)

	shareA, err := r.Register(a)
	if err != nil {
		t.Fatalf("Register(a): %v", err)
	}
	shareB, err := r.Register(b)
	if err != nil {
		t.Fatalf("Register(b): %v", err)
	}
	if shareA.ExportPath == shareB.ExportPath {
		t.Fatalf("two directories share the export path %q", shareA.ExportPath)
	}
	if !strings.HasPrefix(shareA.ExportPath, workspace.ExportMountPrefix) {
		t.Errorf("ExportPath = %q, want the %q prefix", shareA.ExportPath, workspace.ExportMountPrefix)
	}
}

// A file is exported as a synthesised directory holding only that file, and
// the base name travels on the share so the mount can name it as a subpath
// (ADR 0039).
func TestRegisterAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(file, []byte("server {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newTestRegistry(t)

	share, err := r.Register(file)
	if err != nil {
		t.Fatalf("Register(file): %v", err)
	}
	if share.File != "nginx.conf" {
		t.Errorf("share.File = %q, want nginx.conf", share.File)
	}
	if share.LocalPath != file {
		t.Errorf("share.LocalPath = %q, want %q", share.LocalPath, file)
	}
}

// The whole reason a file gets its own export: the siblings in its directory
// must not come with it. Exporting the parent would share them, which is the
// property ADR 0007 relies on not happening.
func TestAFileShareHidesItsSiblings(t *testing.T) {
	dir := t.TempDir()
	wanted := filepath.Join(dir, "wanted.conf")
	secret := filepath.Join(dir, "secret.env")
	for _, f := range []string{wanted, secret} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := newTestRegistry(t)

	share, err := r.Register(wanted)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	entries, err := share.fs.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "wanted.conf" {
		t.Fatalf("the export lists %+v, want only wanted.conf", names(entries))
	}
	if _, err := share.fs.Open("secret.env"); err == nil {
		t.Error("a sibling of the exported file is readable through the share")
	}
	if _, err := share.fs.Stat("secret.env"); err == nil {
		t.Error("a sibling of the exported file is visible through the share")
	}
	// The root itself still has to behave like a directory, or the kernel
	// cannot mount it.
	if _, err := share.fs.Stat("/"); err != nil {
		t.Errorf("Stat(/) on a file share: %v", err)
	}
}

func names(entries []os.FileInfo) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}

func TestRegisterRejectsWhatItCannotServe(t *testing.T) {
	dir := t.TempDir()
	r := newTestRegistry(t)

	if _, err := r.Register(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Error("Register(missing) = nil error, want an error")
	}
}

// The message names what the path is, because "not a directory" sent somebody
// looking for a single-file limitation when the real one is that a socket is a
// kernel object and NFS carries only the name of it.
func TestDescribeMode(t *testing.T) {
	for _, c := range []struct {
		mode os.FileMode
		want string
	}{
		{os.ModeSocket, "socket"},
		{os.ModeDevice, "device"},
		{os.ModeNamedPipe, "named pipe"},
		{os.ModeSymlink, "symlink"},
		{os.ModeIrregular, "special file"},
	} {
		if got := describeMode(c.mode); got != c.want {
			t.Errorf("describeMode(%v) = %q, want %q", c.mode, got, c.want)
		}
	}
}

func TestLookup(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newTestRegistry(t)
	share, err := r.RegisterCWD(dir)
	if err != nil {
		t.Fatalf("RegisterCWD: %v", err)
	}

	tests := []struct {
		name     string
		query    string
		wantRest string
	}{
		{"exact", "/cwd", "/"},
		{"trailing slash", "/cwd/", "/"},
		{"subdirectory", "/cwd/src", "/src"},
		{"nested subdirectory", "/cwd/src/inner", "/src/inner"},
		{"redundant separators", "/cwd//src", "/src"},
		{"dot segment", "/cwd/./src", "/src"},
		{"missing leading slash", "cwd/src", "/src"},
		// Cleaning resolves this to "/cwd", which is a legitimate spelling of
		// a registered share, not an escape. The boundary that matters is
		// that nothing *unregistered* becomes reachable, which is asserted in
		// TestLookupRefusesUnregisteredPaths.
		{"parent of root", "/../cwd", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, rest, ok := r.Lookup(tt.query)
			if !ok {
				t.Fatalf("Lookup(%q) not found", tt.query)
			}
			if got != share {
				t.Errorf("Lookup(%q) returned the wrong share", tt.query)
			}
			if rest != tt.wantRest {
				t.Errorf("Lookup(%q) rest = %q, want %q", tt.query, rest, tt.wantRest)
			}
		})
	}
}

// The export root lists nothing, so only paths that were explicitly
// registered are reachable. A client asking for anything else, including a
// path built to climb out of a share, gets nothing.
func TestLookupRefusesUnregisteredPaths(t *testing.T) {
	dir := t.TempDir()
	r := newTestRegistry(t)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatalf("RegisterCWD: %v", err)
	}

	for _, query := range []string{
		"/",
		"/m",
		"/m/0011223344556677",
		"/etc",
		"/cwd/../etc",
		"/cwd/../../etc/passwd",
		"/cwdx",
		"",
	} {
		if share, _, ok := r.Lookup(query); ok {
			t.Errorf("Lookup(%q) resolved to %q, want not found", query, share.ExportPath)
		}
	}
}

// A share and a sibling whose export path is a prefix of it must not be
// confused: "/m/aaa" must never serve a lookup of "/m/aaabbb".
func TestLookupDoesNotMatchPartialSegments(t *testing.T) {
	dir := t.TempDir()
	r := newTestRegistry(t)
	share, err := r.RegisterCWD(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := r.Lookup(share.ExportPath + "extra"); ok {
		t.Error("a partial segment match resolved to a share")
	}
}

func TestNormalizeExport(t *testing.T) {
	tests := map[string]string{
		"/cwd":        "/cwd",
		"/cwd/":       "/cwd",
		"cwd":         "/cwd",
		"//cwd":       "/cwd",
		"/cwd/./x":    "/cwd/x",
		"/cwd/y/../x": "/cwd/x",
		"/cwd/../..":  "/",
		"":            "/",
		"   /cwd   ":  "/cwd",
	}
	for in, want := range tests {
		if got := normalizeExport(in); got != want {
			t.Errorf("normalizeExport(%q) = %q, want %q", in, got, want)
		}
	}
}

// Only a MOUNT may bring a share back, and only for an export the resolver
// already knows.
func TestLookupOrRestore(t *testing.T) {
	dir := t.TempDir()
	export := workspace.ExportPathForID(workspace.ShareID(dir))

	r := NewRegistry(Attrs{})
	asked := 0
	r.Restore = func(path string) (string, bool) {
		asked++
		if path == export {
			return dir, true
		}
		return "", false
	}

	// A miss the resolver knows about comes back.
	share, rest, ok := r.LookupOrRestore(export)
	if !ok {
		t.Fatal("a known export was not restored")
	}
	if share.LocalPath != dir || rest != "/" {
		t.Errorf("restored %q rest %q, want %q", share.LocalPath, rest, dir)
	}

	// And is then an ordinary registration: the resolver is not asked again.
	before := asked
	if _, _, ok := r.LookupOrRestore(export); !ok {
		t.Error("a restored share was not found on the next lookup")
	}
	if asked != before {
		t.Error("the resolver was asked about a share already registered")
	}

	// A miss it does not know stays a miss.
	if _, _, ok := r.LookupOrRestore("/m/0123456789abcdef"); ok {
		t.Error("an export the resolver refused was restored anyway")
	}
}

// Lookup and Shares must never resurrect anything. The volume collector and
// the watcher read them, and "in use" cannot depend on who asked.
func TestOnlyMountRestores(t *testing.T) {
	dir := t.TempDir()
	export := workspace.ExportPathForID(workspace.ShareID(dir))

	r := NewRegistry(Attrs{})
	r.Restore = func(string) (string, bool) { return dir, true }

	if _, _, ok := r.Lookup(export); ok {
		t.Error("Lookup restored a share")
	}
	if n := len(r.Shares()); n != 0 {
		t.Errorf("Shares reported %d shares before anything was registered", n)
	}
}

// A registry with no resolver behaves exactly as it did, which is what a query
// session and every test without a record need.
func TestNoResolverMeansAMissIsAMiss(t *testing.T) {
	r := NewRegistry(Attrs{})
	if _, _, ok := r.LookupOrRestore("/m/0123456789abcdef"); ok {
		t.Error("a registry with no resolver restored something")
	}
}

// A file directly under a root has a containing directory too, and getting it
// wrong exports nothing at all.
func TestRegisterAFileAtARoot(t *testing.T) {
	dir := t.TempDir()
	// The nearest portable stand-in for "/x.conf": the volume root on Windows,
	// "/" elsewhere, is not writable in a test, so this asserts the split
	// rather than the export.
	file := filepath.Join(dir, "at-root.conf")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	share, err := newTestRegistry(t).Register(file)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if share.File != "at-root.conf" {
		t.Errorf("share.File = %q", share.File)
	}
	if got := filepath.Dir(file); filepath.Dir(share.LocalPath) != got {
		t.Errorf("the share does not sit under %q", got)
	}
}
