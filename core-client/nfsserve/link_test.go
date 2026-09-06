package nfsserve

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	nfs "github.com/willscott/go-nfs"
)

// A hard link is made, and the second name IS the file: LINK as RFC 1813
// spells it (rawLink in helpers_test.go), answered by attrChange.Link.
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
	target, client, root, err := mountAt(t, serve(t, r), "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
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

// A hard link into a single-file share is refused, and creates nothing.
//
// The export is a synthesised directory holding one file (ADR 0039) and the
// directory it really sits in is the user's own. go-nfs stats the new name
// through singleFileFS, which answers not-exist for a sibling, so the check
// passes; what refuses it is singleFileChange, because the Change is built
// from the filesystem's Root, which is the CONTAINING directory.
func TestLinkIntoASingleFileShareIsRefused(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "only.conf")
	if err := os.WriteFile(file, []byte("server {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(DefaultAttrs)
	share, err := r.Register(file)
	if err != nil {
		t.Fatal(err)
	}
	target, client, root, err := mountAt(t, serve(t, r), share.ExportPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	_, fh, err := target.Lookup("only.conf")
	if err != nil {
		t.Fatalf("looking up the one file: %v", err)
	}

	if status := rawLink(t, client, fh, root, "sneaky"); status == uint32(nfs.NFSStatusOk) {
		t.Errorf("LINK into a single-file share returned NFS3_OK")
	}
	if _, err := os.Lstat(filepath.Join(dir, "sneaky")); !os.IsNotExist(err) {
		t.Errorf("a LINK through a single-file share made a name in the user's directory (lstat: %v)", err)
	}
}
