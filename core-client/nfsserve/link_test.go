package nfsserve

import (
	"os"
	"path/filepath"
	"testing"

	nfs "github.com/willscott/go-nfs"
	nfsclient "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// A hard link is refused, and the refusal says which status it is.
//
// The share's Change (attrChange) implements no Link, so go-nfs cannot make
// one, and nothing here wants it to: a second name for one inode is exactly
// what the fileid rules in attrs.go cannot express twice (Nlink is always 1).
// What must not happen is a refusal that leaves a name behind, or one that
// damages the file it was asked to link -- `ln` or `cp -l` in a container would
// then fail with something on disk changed. Two things are pinned so a change
// in go-nfs is noticed: the status word, and the fact that go-nfs reads
// LINK3args in SYMLINK's layout (nfs_onlink.go reads a sattr3 and a string
// where the RFC has only diropargs3), so a kernel-shaped request is refused at
// the parser and never reaches the Change at all. A parser fix upstream would
// move the status to NFS3ERR_ACCES, which is the second subtest.
func TestLinkIsRefusedByName(t *testing.T) {
	dir := t.TempDir()
	const content = "the original, before anything linked it\n"
	if err := os.WriteFile(filepath.Join(dir, "orig"), []byte(content), 0o644); err != nil {
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

	unharmed := func(t *testing.T, linkName string) {
		t.Helper()
		if _, err := os.Lstat(filepath.Join(dir, linkName)); !os.IsNotExist(err) {
			t.Errorf("after a refused LINK, %q exists on disk (lstat: %v)", linkName, err)
		}
		got, err := os.ReadFile(filepath.Join(dir, "orig"))
		if err != nil || string(got) != content {
			t.Errorf("the source after a refused LINK: %q, %v; want it untouched", got, err)
		}
		if _, err := target.Getattr("orig"); err != nil {
			t.Errorf("the source is no longer reachable over the wire: %v", err)
		}
	}

	t.Run("as a kernel client sends it", func(t *testing.T) {
		status := rawLink(t, client, orig, root, "hard")
		if status != uint32(nfs.NFSStatusInval) {
			t.Errorf("LINK returned %s (%d), want NFS3ERR_INVAL: go-nfs's parser "+
				"reads SYMLINK's layout for LINK and runs off the end of the RFC's",
				statusName(status), status)
		}
		unharmed(t, "hard")
	})

	// The layout go-nfs's parser actually expects, so the request gets past it
	// and the refusal comes from the share's own Change having no Link.
	t.Run("as go-nfs parses it", func(t *testing.T) {
		type linkArgs struct {
			rpc.Header
			Where nfsclient.Diropargs3
			Attr  nfsclient.Sattr3
			File  []byte
		}
		status, _ := rawStatus(t, client, &linkArgs{
			Header: nfsHeader(uint32(nfs.NFSProcedureLink)),
			Where:  nfsclient.Diropargs3{FH: root, Filename: "hard2"},
			File:   orig,
		})
		if status != uint32(nfs.NFSStatusAccess) {
			t.Errorf("LINK returned %s (%d), want NFS3ERR_ACCES: the share's Change "+
				"has no Link, and go-nfs refuses with ACCES rather than NOTSUPP",
				statusName(status), status)
		}
		unharmed(t, "hard2")
	})
}
