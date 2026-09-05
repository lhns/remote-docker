package nfsserve

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	nfs "github.com/willscott/go-nfs"
	nfsclient "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// A hard link is made, and the second name IS the file.
//
// LINK arrives as RFC 1813 spells it (a file handle, then a directory handle
// and the new name; rawLink in helpers_test.go), and attrChange.Link answers
// it with os.Link. Afterwards both names are one inode on the host, the share
// reports nlink=2 for each (nlink_test.go is where the count itself is
// pinned), and a write through one name is read through the other, which is
// what `ln`, `cp -l` and git's object store depend on.
//
// One thing must still be refused: a new name that leaves the share. It comes
// back as NFS3ERR_ACCES, and nothing is created.
func TestLinkMakesASecondName(t *testing.T) {
	dir := t.TempDir()
	const content = "the original, before anything linked it\n"
	if err := os.WriteFile(filepath.Join(dir, "orig"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "orig"), filepath.Join(dir, "probe")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "probe")); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	client, root, err := mountRaw(t, serve(t, r), "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	target, err := nfsclient.NewTargetWithClient(client, rpc.AuthNull, root, "/cwd", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, orig, err := target.Lookup("orig")
	if err != nil {
		t.Fatalf("looking up the file to link: %v", err)
	}

	t.Run("as a kernel client sends it", func(t *testing.T) {
		if status := rawLink(t, client, orig, root, "hard"); status != uint32(nfs.NFSStatusOk) {
			t.Fatalf("LINK returned %s (%d), want NFS3_OK", statusName(status), status)
		}

		a, err := os.Stat(filepath.Join(dir, "orig"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.Stat(filepath.Join(dir, "hard"))
		if err != nil {
			t.Fatalf("the new name after LINK: %v", err)
		}
		if !os.SameFile(a, b) {
			t.Errorf("orig and hard are two files on the host, want one")
		}

		for _, name := range []string{"orig", "hard"} {
			fi, err := target.Getattr(name)
			if err != nil {
				t.Fatalf("%s over the wire: %v", name, err)
			}
			if fi.Nlink != 2 {
				t.Errorf("%s reports nlink=%d after LINK, want 2", name, fi.Nlink)
			}
		}

		const appended = "and a line written through the link\n"
		f, err := target.OpenFile("hard", 0o644)
		if err != nil {
			t.Fatal(err)
		}
		// The client library cannot seek from the end; the length is known.
		if _, err := f.Seek(int64(len(content)), io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(appended)); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		g, err := target.Open("orig")
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(g)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != content+appended {
			t.Errorf("read through orig after writing through hard:\n%q\nwant\n%q", got, content+appended)
		}
	})

	t.Run("a name that leaves the share is refused", func(t *testing.T) {
		status := rawLink(t, client, orig, root, "../escaped")
		if status != uint32(nfs.NFSStatusAccess) {
			t.Errorf("LINK to ../escaped returned %s (%d), want NFS3ERR_ACCES", statusName(status), status)
		}
		for _, p := range []string{filepath.Join(dir, "..", "escaped"), filepath.Join(dir, "escaped")} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Errorf("after a refused LINK, %q exists (lstat: %v)", p, err)
			}
		}
		if _, err := target.Getattr("orig"); err != nil {
			t.Errorf("the source is no longer reachable over the wire: %v", err)
		}
	})
}
