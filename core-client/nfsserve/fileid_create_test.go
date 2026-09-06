package nfsserve

import (
	"testing"

	nfsclient "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// The CREATE reply's fileid must be the fileid every later reply reports for
// the file. The Linux client builds the inode from the CREATE reply and, on
// the next reply with a different fileid, marks it stale: GNU tar then fails
// every utime, chown and chmod on the file it just wrote. The client library's
// Create discards the reply's attributes, so this speaks CREATE itself.
func TestCreateReplyFileIDMatchesGetattr(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	target, client, root, err := mountAt(t, serve(t, r), "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	type how struct {
		Mode uint32
		Attr nfsclient.Sattr3
	}
	type createArgs struct {
		rpc.Header
		Where nfsclient.Diropargs3
		HW    how
	}
	type createRes struct {
		FH     nfsclient.PostOpFH3
		Attr   nfsclient.PostOpAttr
		DirWcc nfsclient.WccData
	}
	res, err := client.Call(&createArgs{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    nfsclient.Nfs3Prog,
			Vers:    nfsclient.Nfs3Vers,
			Proc:    nfsclient.NFSProc3Create,
			Cred:    rpc.AuthNull,
			Verf:    rpc.AuthNull,
		},
		Where: nfsclient.Diropargs3{FH: root, Filename: "fresh"},
		HW:    how{Attr: nfsclient.Sattr3{Mode: nfsclient.SetMode{SetIt: true, Mode: 0o644}}},
	})
	if err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if status, err := xdr.ReadUint32(res); err != nil || status != nfsclient.NFS3Ok {
		t.Fatalf("CREATE status %d, err %v", status, err)
	}
	var created createRes
	if err := xdr.Read(res, &created); err != nil {
		t.Fatalf("decoding the CREATE reply: %v", err)
	}
	if !created.FH.IsSet || !created.Attr.IsSet {
		t.Fatalf("the CREATE reply carried no handle or no attributes: %+v", created)
	}

	after, err := target.GetAttr(created.FH.FH)
	if err != nil {
		t.Fatalf("GETATTR on the created file: %v", err)
	}
	if created.Attr.Attr.Fileid != after.Fileid {
		t.Errorf("CREATE reported fileid %#x and GETATTR %#x for one file; the client marks the inode stale on that",
			created.Attr.Attr.Fileid, after.Fileid)
	}
}
