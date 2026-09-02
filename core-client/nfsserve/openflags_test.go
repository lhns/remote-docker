package nfsserve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5/osfs"
)

// What go-nfs asks of the exported filesystem when a client truncates a file.
//
// go-nfs's SetFileAttributes.Apply opens the file with
//
//	fs.OpenFile(file, os.O_WRONLY|os.O_EXCL, 0)
//
// before calling Truncate (file.go, the SetSize branch). O_EXCL WITHOUT O_CREAT
// is undefined by POSIX, so whether that open succeeds is a property of the
// platform rather than of the protocol -- and the export is served from the
// user's machine, which may be any of the three this project builds for.
//
// It matters because of where the error goes. Apply returns a raw error there,
// not an NFSStatusError, and go-nfs turns anything it does not recognise into
// an RPC-level ResponseCodeSystemError rather than a status the client can
// interpret. So a refusal here does not reach the application as "cannot
// truncate"; it reaches it as a broken reply.
//
// Asserted rather than assumed because a build on a share dies at exactly this
// operation (test/integration.sh section 15d) and the server logs nothing: no
// file handle it could not resolve, no status it chose to send.
func TestExportSupportsTheTruncateOpen(t *testing.T) {
	dir := t.TempDir()
	fs := osfs.New(dir, osfs.WithBoundOS())

	f, err := fs.Create("p2")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	fp, err := fs.OpenFile("p2", os.O_WRONLY|os.O_EXCL, 0)
	if err != nil {
		t.Fatalf("OpenFile(O_WRONLY|O_EXCL) on an existing file: %v\n"+
			"go-nfs opens this way to serve SETATTR with a size, so a refusal "+
			"here is a truncate the client cannot complete", err)
	}
	if err := fp.Truncate(4); err != nil {
		_ = fp.Close()
		t.Fatalf("truncate: %v", err)
	}
	if err := fp.Close(); err != nil {
		t.Fatalf("closing after truncate: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "p2"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 4 {
		t.Errorf("size after truncate = %d, want 4", info.Size())
	}
}
