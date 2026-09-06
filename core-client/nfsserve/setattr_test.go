package nfsserve

import (
	"testing"

	nfsclient "github.com/willscott/go-nfs-client/nfs"
)

// Truncating a file the share already holds.
//
// This is the operation `test/integration.sh` section 15d narrowed the reported
// bug to. A build on a share dies at the linker with "final link failed: Stale
// file handle", and taking the ending apart showed that a plain
// create-write-close survives while sizing the file afterwards does not:
//
//	truncate: p2: truncate: Stale file handle
//
// That is why compiling works and linking does not. gcc writes a .o and closes
// it; ld sizes the output it just wrote, and SETATTR is the operation underneath
// that.
//
// Driven over a real NFSv3 conversation rather than a kernel mount, so it runs
// on the machine this is developed on. If it passes here and the integration
// suite still fails, the difference is the kernel client and that is worth
// knowing precisely.
func TestServeTruncatesAFile(t *testing.T) {
	dir := t.TempDir()
	target := mountCWD(t, dir)

	f, err := target.OpenFile("p2", 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	// Shrinking, which is what the narrowing probe did and what ld does to the
	// output it has finished writing.
	var sattr nfsclient.Sattr3
	sattr.Size.SetIt = true
	sattr.Size.Size = 4

	if err := target.Setattr("p2", sattr); err != nil {
		t.Fatalf("truncating a file the share holds: %v", err)
	}

	if fi, err := target.Getattr("p2"); err != nil {
		t.Fatalf("stat after truncate: %v", err)
	} else if fi.Filesize != 4 {
		t.Errorf("size after truncate = %d, want 4", fi.Filesize)
	}
}
