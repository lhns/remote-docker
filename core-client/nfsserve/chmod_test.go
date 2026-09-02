package nfsserve

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	nfsclient "github.com/willscott/go-nfs-client/nfs"
)

// A mode set through the share has to reach the file.
//
// It did not. `Change` type-asserts the filesystem it is handed to
// billy.Change, and the filesystem it is handed is an *attrFS, which EMBEDS the
// billy.Filesystem interface -- so the inner filesystem's Chmod is not
// promoted, the assertion fails, and attrChange was built with a nil inner
// that discards every call and reports success.
//
// The visible cost is that a binary built on a share cannot be run: the linker
// writes it, asks for the executable bit, is told yes, and the bit never lands.
// `test/integration.sh` section 15d found it once the ESTALE above it was gone.
//
// Asserted against the file on THIS machine rather than through the share,
// because the share synthesises permissions: reading it back through NFS would
// report the executable bit from Attrs and pass while the real file has none.
func TestChmodThroughTheShareReachesTheFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod has no meaning here; the executable bit is a POSIX one")
	}

	dir := t.TempDir()
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	target := mustMount(t, serve(t, r), "/cwd")

	f, err := target.OpenFile("prog", 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.Write([]byte("#!/bin/sh\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	var sattr nfsclient.Sattr3
	sattr.Mode.SetIt = true
	sattr.Mode.Mode = 0o755

	if err := target.Setattr("prog", sattr); err != nil {
		t.Fatalf("setting the mode through the share: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "prog"))
	if err != nil {
		t.Fatalf("stat on this machine: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the file is %v on disk, so the executable bit never landed; "+
			"a binary built on this share could not be run", info.Mode().Perm())
	}
}

// The same fault, on every platform: a timestamp written through the share.
//
// Chtimes went the same way as Chmod and its comment claimed otherwise --
// "timestamps are read from the underlying filesystem, so a change here is
// observable, and build tools depend on mtime". It was a no-op, so `touch`
// through a share did nothing, make saw nothing rebuilt, and dircache's
// write-back compares the mtimes this was failing to set.
//
// Windows has no executable bit to check but it has timestamps, which is why
// this one carries the assertion everywhere.
func TestChtimesThroughTheShareReachesTheFile(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	target := mustMount(t, serve(t, r), "/cwd")

	f, err := target.OpenFile("stamped", 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	f.Close()

	want := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	var sattr nfsclient.Sattr3
	sattr.Mtime.SetIt = nfsclient.SetToClientTime
	sattr.Mtime.Time.Seconds = uint32(want.Unix())

	if err := target.Setattr("stamped", sattr); err != nil {
		t.Fatalf("setting the time through the share: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "stamped"))
	if err != nil {
		t.Fatalf("stat on this machine: %v", err)
	}
	if got := info.ModTime().Truncate(time.Second); !got.Equal(want) {
		t.Errorf("mtime on disk is %v, want %v; the write never landed", got, want)
	}
}
