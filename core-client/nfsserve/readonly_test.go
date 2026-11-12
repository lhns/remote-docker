package nfsserve

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	nfs "github.com/willscott/go-nfs"
	nfsclient "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"

	"github.com/lhns/remote-docker/core/workspace"
)

// Every test here speaks to a real server over a socket, as the write-through
// tests do (ADR 0058).

// roFixture is a directory shared read-only, with a file, a subdirectory and a
// link in it.
type roFixture struct {
	dir    string
	addr   string
	export string
	reg    *Registry
	target *nfsclient.Target
	client *rpc.Client
	root   []byte
}

func newROFixture(t *testing.T) *roFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "inner"), []byte("inner\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(DefaultAttrs)
	share, err := r.Register(dir, workspace.DefaultMode, workspace.ShareWords{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	addr := serve(t, r)
	target, client, root := mustMountAt(t, addr, share.ExportPath)
	t.Cleanup(func() { client.Close() })
	return &roFixture{dir: dir, addr: addr, export: share.ExportPath, reg: r, target: target, client: client, root: root}
}

// snapshot is everything observable about a tree on disk: names, modes, sizes,
// times and content.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		var content string
		if info.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			content = string(b)
		}
		lines = append(lines, fmt.Sprintf("%s|%v|%d|%d|%q", filepath.ToSlash(rel), info.Mode(), info.Size(), info.ModTime().UnixNano(), content))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func rawRename(t *testing.T, client *rpc.Client, fromDir []byte, from string, toDir []byte, to string) uint32 {
	t.Helper()
	type renameArgs struct {
		rpc.Header
		From nfsclient.Diropargs3
		To   nfsclient.Diropargs3
	}
	status, _ := rawStatus(t, client, &renameArgs{
		Header: nfsHeader(uint32(nfs.NFSProcedureRename)),
		From:   nfsclient.Diropargs3{FH: fromDir, Filename: from},
		To:     nfsclient.Diropargs3{FH: toDir, Filename: to},
	})
	return status
}

func rawSocket(t *testing.T, client *rpc.Client, dir []byte, name string) uint32 {
	t.Helper()
	type mknodArgs struct {
		rpc.Header
		Where nfsclient.Diropargs3
		Type  uint32
		Attrs nfsclient.Sattr3
	}
	status, _ := rawStatus(t, client, &mknodArgs{
		Header: nfsHeader(uint32(nfs.NFSProcedureMkNod)),
		Where:  nfsclient.Diropargs3{FH: dir, Filename: name},
		Type:   6, // NF3SOCK
	})
	return status
}

func rawLookup(t *testing.T, client *rpc.Client, dir []byte, name string) uint32 {
	t.Helper()
	type lookupArgs struct {
		rpc.Header
		What nfsclient.Diropargs3
	}
	status, _ := rawStatus(t, client, &lookupArgs{
		Header: nfsHeader(uint32(nfs.NFSProcedureLookup)),
		What:   nfsclient.Diropargs3{FH: dir, Filename: name},
	})
	return status
}

// Every procedure that changes a file or a name is refused, and nothing on
// disk moves.
func TestReadOnlyShareRefusesEveryMutation(t *testing.T) {
	f := newROFixture(t)
	_, fileFH, err := f.target.Lookup("file")
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, f.dir)

	size := func() nfsclient.Sattr3 {
		var s nfsclient.Sattr3
		s.Size.SetIt, s.Size.Size = true, 0
		return s
	}
	mode := func() nfsclient.Sattr3 {
		var s nfsclient.Sattr3
		s.Mode.SetIt, s.Mode.Mode = true, 0o600
		return s
	}
	owner := func() nfsclient.Sattr3 {
		var s nfsclient.Sattr3
		s.UID.SetIt, s.UID.UID = true, 4242
		return s
	}
	otherMtime := func() nfsclient.Sattr3 {
		attr, err := f.target.Getattr("file")
		if err != nil {
			t.Fatal(err)
		}
		var s nfsclient.Sattr3
		s.Mtime.SetIt = nfsclient.SetToClientTime
		s.Mtime.Time = attr.Mtime
		s.Mtime.Time.Seconds += 3600
		return s
	}
	serverAtime := func() nfsclient.Sattr3 {
		var s nfsclient.Sattr3
		s.Atime.SetIt = nfsclient.SetToServerTime
		return s
	}

	for _, c := range []struct {
		name string
		do   func() error // nil error means the server accepted it
	}{
		{"WRITE", func() error {
			w, err := f.target.OpenFile("file", 0o644)
			if err != nil {
				return err
			}
			_, err = w.Write([]byte("overwritten"))
			return err
		}},
		{"CREATE", func() error { _, err := f.target.Create("new", 0o644); return err }},
		{"MKDIR", func() error { _, err := f.target.Mkdir("newdir", 0o755); return err }},
		{"SYMLINK", func() error { return f.target.Symlink("file", "lnk") }},
		{"MKNOD", func() error {
			if s := rawSocket(t, f.client, f.root, "sock"); s != uint32(nfs.NFSStatusOk) {
				return fmt.Errorf("%s", statusName(s))
			}
			return nil
		}},
		{"REMOVE", func() error { return f.target.Remove("file") }},
		{"RMDIR", func() error { return f.target.RmDir("sub") }},
		{"RENAME within the root", func() error { return f.target.Rename("file", "moved") }},
		{"RENAME out of a subdirectory", func() error { return f.target.Rename("sub/inner", "out") }},
		{"RENAME into a subdirectory", func() error { return f.target.Rename("file", "sub/in") }},
		{"LINK", func() error {
			if s := rawLink(t, f.client, fileFH, f.root, "hard"); s != uint32(nfs.NFSStatusOk) {
				return fmt.Errorf("%s", statusName(s))
			}
			return nil
		}},
		{"SETATTR size", func() error { return f.target.Setattr("file", size()) }},
		{"SETATTR mode", func() error { return f.target.Setattr("file", mode()) }},
		{"SETATTR owner", func() error { return f.target.Setattr("file", owner()) }},
		{"SETATTR mtime to another time", func() error { return f.target.Setattr("file", otherMtime()) }},
		{"SETATTR atime to the server's time", func() error { return f.target.Setattr("file", serverAtime()) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.do()
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			t.Logf("%s refused: %v", c.name, err)
			if after := snapshot(t, f.dir); after != before {
				t.Errorf("the directory changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// COMMIT flushes writes, and a read-only share has none to flush: go-nfs
// answers it without touching the filesystem, so nothing can be refused and
// nothing changes.
func TestReadOnlyShareCommitChangesNothing(t *testing.T) {
	f := newROFixture(t)
	_, fileFH, err := f.target.Lookup("file")
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, f.dir)

	type commitArgs struct {
		rpc.Header
		FH     []byte
		Offset uint64
		Count  uint32
	}
	status, _ := rawStatus(t, f.client, &commitArgs{Header: nfsHeader(uint32(nfs.NFSProcedureCommit)), FH: fileFH})
	t.Logf("COMMIT answered %s", statusName(status))
	if after := snapshot(t, f.dir); after != before {
		t.Errorf("COMMIT changed the directory:\n%s\n%s", before, after)
	}
}

// What the workspace needs from a read-only share still works, the agent's
// poker above all: ACCESS keeps write (the kernel opens a file for writing
// before it does anything else), and a SETATTR carrying the file's own mtime
// succeeds, because the kernel raises its inotify event only if it does.
func TestReadOnlyShareStillReadsAndPokes(t *testing.T) {
	f := newROFixture(t)
	before := snapshot(t, f.dir)

	r, err := f.target.Open("file")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "original\n" {
		t.Errorf("read %q, %v", got, err)
	}
	if _, _, err := f.target.Lookup("sub/inner"); err != nil {
		t.Errorf("lookup: %v", err)
	}
	entries, err := f.target.ReadDirPlus("/")
	if err != nil || len(entries) != 2 {
		t.Errorf("listing = %v, %v; want file and sub", entries, err)
	}

	const mask = 0x1 | 0x2 | 0x4 | 0x8 | 0x10 | 0x20 // read lookup modify extend delete execute
	granted, err := f.target.Access("file", mask)
	if err != nil {
		t.Fatal(err)
	}
	if granted&0x4 == 0 || granted&0x8 == 0 {
		t.Errorf("ACCESS granted %#x; the poker's write open needs MODIFY and EXTEND", granted)
	}

	attr, err := f.target.Getattr("file")
	if err != nil {
		t.Fatal(err)
	}
	var identity nfsclient.Sattr3
	identity.Mtime.SetIt = nfsclient.SetToClientTime
	identity.Mtime.Time = attr.Mtime
	if err := f.target.Setattr("file", identity); err != nil {
		t.Errorf("the identity SETATTR (mtime set to its own value) was refused: %v", err)
	}

	// Both times, each the file's own: the share reports them as one value.
	identity.Atime.SetIt = nfsclient.SetToClientTime
	identity.Atime.Time = attr.Mtime
	if err := f.target.Setattr("file", identity); err != nil {
		t.Errorf("an identity SETATTR carrying both times was refused: %v", err)
	}

	if after := snapshot(t, f.dir); after != before {
		t.Errorf("reading and poking changed the directory:\n%s\n%s", before, after)
	}
}

// A mount of a subdirectory is as read-only as the share.
func TestReadOnlyShareSubdirectoryMount(t *testing.T) {
	f := newROFixture(t)
	sub := mustMount(t, f.addr, f.export+"/sub")
	if _, err := sub.Create("new", 0o644); err == nil {
		t.Error("created a file through a mount of a subdirectory")
	}
	if err := sub.Remove("inner"); err == nil {
		t.Error("removed a file through a mount of a subdirectory")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "sub", "new")); err == nil {
		t.Error("the new file is on disk")
	}
}

// A single file shared read-only cannot be written or replaced.
func TestReadOnlySingleFileShare(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(file, []byte("setting\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(DefaultAttrs)
	share, err := r.Register(file, workspace.DefaultMode, workspace.ShareWords{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	target := mustMount(t, serve(t, r), share.ExportPath)

	w, err := target.OpenFile("app.conf", 0o644)
	if err == nil {
		_, err = w.Write([]byte("changed"))
	}
	if err == nil {
		t.Error("wrote the file")
	}
	if err := target.Remove("app.conf"); err == nil {
		t.Error("removed the file")
	}
	if got, _ := os.ReadFile(file); string(got) != "setting\n" {
		t.Errorf("the file is now %q", got)
	}
}

// A read-only share cannot be left through the namespace, and nothing moves
// into or out of it through another share.
func TestReadOnlyShareContainment(t *testing.T) {
	outer := t.TempDir()
	roDir, rwDir := filepath.Join(outer, "ro"), filepath.Join(outer, "rw")
	for _, d := range []string{roDir, rwDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, c := range map[string]string{
		filepath.Join(outer, "outside"): "outside\n",
		filepath.Join(roDir, "inside"):  "ro\n",
		filepath.Join(rwDir, "other"):   "rw\n",
	} {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r := NewRegistry(DefaultAttrs)
	ro, err := r.Register(roDir, workspace.DefaultMode, workspace.ShareWords{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	rw, err := r.Register(rwDir, workspace.DefaultMode, workspace.ShareWords{})
	if err != nil {
		t.Fatal(err)
	}
	addr := serve(t, r)

	t.Run("the namespace root is not mountable", func(t *testing.T) {
		for _, p := range []string{"/", "/m", "/m/"} {
			if _, client, _, err := mountAt(t, addr, p); err == nil {
				client.Close()
				t.Errorf("mounting %q succeeded", p)
			}
		}
	})

	roTarget, roClient, roRoot := mustMountAt(t, addr, ro.ExportPath)
	t.Cleanup(func() { roClient.Close() })
	_, rwClient, rwRoot := mustMountAt(t, addr, rw.ExportPath)
	t.Cleanup(func() { rwClient.Close() })

	t.Run("dot-dot from the root does not leave", func(t *testing.T) {
		// Whatever ".." answers, a sibling of the share is not behind it.
		if rawLookup(t, roClient, roRoot, "..") == uint32(nfs.NFSStatusOk) {
			t.Log("LOOKUP .. from the root answered OK (the root itself)")
		}
		if s := rawLookup(t, roClient, roRoot, "../outside"); s == uint32(nfs.NFSStatusOk) {
			t.Error("LOOKUP ../outside reached a file beside the share")
		}
	})

	t.Run("nothing moves between shares", func(t *testing.T) {
		before := snapshot(t, outer)
		_, inFH, err := roTarget.Lookup("inside")
		if err != nil {
			t.Fatal(err)
		}
		if s := rawRename(t, roClient, roRoot, "inside", rwRoot, "stolen"); s == uint32(nfs.NFSStatusOk) {
			t.Error("RENAME out of the read-only share into the read-write one succeeded")
		}
		if s := rawRename(t, rwClient, rwRoot, "other", roRoot, "planted"); s == uint32(nfs.NFSStatusOk) {
			t.Error("RENAME from the read-write share into the read-only one succeeded")
		}
		if s := rawLink(t, roClient, inFH, rwRoot, "linked"); s == uint32(nfs.NFSStatusOk) {
			t.Error("LINK of a read-only file into the read-write share succeeded")
		}
		if after := snapshot(t, outer); after != before {
			t.Errorf("the tree changed:\n%s\n%s", before, after)
		}
	})
}

// A read-only and a read-write share of one directory are two exports, and the
// read-write one writes (ADR 0058: it reaches the same files).
func TestReadOnlyAndReadWriteSharesAreTwo(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(DefaultAttrs)
	ro, err := r.Register(dir, workspace.DefaultMode, workspace.ShareWords{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	rw, err := r.Register(dir, workspace.DefaultMode, workspace.ShareWords{})
	if err != nil {
		t.Fatal(err)
	}
	if ro.ExportPath == rw.ExportPath {
		t.Fatal("one export for both")
	}
	if rw.ExportPath != exportOf(dir) {
		t.Errorf("the read-write export moved to %s", rw.ExportPath)
	}
	if !ro.Words().ReadOnly || rw.Words().ReadOnly {
		t.Errorf("words: ro=%+v rw=%+v", ro.Words(), rw.Words())
	}
	if got := RecordedExport(dir, workspace.DefaultMode, workspace.ShareWords{Owner: workspace.Owner{Set: true}, ReadOnly: true}); got != ro.ExportPath {
		t.Errorf("RecordedExport = %s, want %s", got, ro.ExportPath)
	}

	addr := serve(t, r)
	if _, err := mustMount(t, addr, rw.ExportPath).Create("written", 0o644); err != nil {
		t.Errorf("the read-write share refused a create: %v", err)
	}
	if _, err := mustMount(t, addr, ro.ExportPath).Create("again", 0o644); err == nil {
		t.Error("the read-only share accepted a create")
	}
}
