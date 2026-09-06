package nfsserve

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	nfs "github.com/willscott/go-nfs"
	nfsclient "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
	nfsfile "github.com/willscott/go-nfs/file"

	"github.com/lhns/remote-docker/core/workspace"
)

// dialWithRetry works around the host, not the code under test: these tests
// make many short-lived connections, and on Windows each one holds its source
// port in TIME_WAIT, so a dial intermittently fails with "Only one usage of
// each socket address".
func dialWithRetry(host string, port int) (*rpc.Client, error) {
	var err error
	for attempt := range 10 {
		var client *rpc.Client
		client, err = nfsclient.DialServiceAtPort(host, port)
		if err == nil {
			return client, nil
		}
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
	return nil, err
}

// mountAt performs an NFSv3 MOUNT and returns the target, the connection under
// it and the root handle. The connection and the handle are what a test needs
// to speak a procedure the client library hides, and they come from here so no
// test rebuilds the pair for itself.
//
// The client's own Mount helper cannot be used: it reaches the NFS program
// through rpcbind on port 111, and this server, like the deployment it models,
// serves MOUNT and NFS on one port with no portmapper anywhere. That is the
// same reason the mount options say port == mountport.
func mountAt(t *testing.T, addr, export string) (*nfsclient.Target, *rpc.Client, []byte, error) {
	t.Helper()

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port %q: %v", portStr, err)
	}

	client, err := dialWithRetry(host, port)
	if err != nil {
		t.Fatalf("dialling %s: %v", addr, err)
	}
	fail := func(err error) (*nfsclient.Target, *rpc.Client, []byte, error) {
		client.Close()
		return nil, nil, nil, err
	}

	type mountRequest struct {
		rpc.Header
		Dirpath string
	}
	res, err := client.Call(&mountRequest{
		rpc.Header{
			Rpcvers: 2,
			Prog:    nfsclient.MountProg,
			Vers:    nfsclient.MountVers,
			Proc:    nfsclient.MountProc3MNT,
			Cred:    rpc.AuthNull,
			Verf:    rpc.AuthNull,
		},
		export,
	})
	if err != nil {
		return fail(fmt.Errorf("mount call: %w", err))
	}

	status, err := xdr.ReadUint32(res)
	if err != nil {
		return fail(fmt.Errorf("reading mount status: %w", err))
	}
	if status != nfsclient.MNT3Ok {
		return fail(fmt.Errorf("mount of %q refused with status %d", export, status))
	}

	fh, err := xdr.ReadOpaque(res)
	if err != nil {
		return fail(fmt.Errorf("reading file handle: %w", err))
	}
	_, _ = xdr.ReadUint32List(res)

	// The same connection serves the NFS program, so no second dial.
	target, err := nfsclient.NewTargetWithClient(client, rpc.AuthNull, fh, export, 0)
	if err != nil {
		return fail(fmt.Errorf("opening target: %w", err))
	}
	return target, client, fh, nil
}

// mustMountAt is mountAt with the refusal fatal, for the tests that speak a
// procedure the client library hides and so need the connection and the root
// handle as well as the target.
func mustMountAt(t *testing.T, addr, export string) (*nfsclient.Target, *rpc.Client, []byte) {
	t.Helper()
	target, client, root, err := mountAt(t, addr, export)
	if err != nil {
		t.Fatalf("mounting %q: %v", export, err)
	}
	t.Cleanup(func() { target.Close() })
	return target, client, root
}

// mustMount fails the test if the mount is refused.
func mustMount(t *testing.T, addr, export string) *nfsclient.Target {
	t.Helper()
	target, _, _ := mustMountAt(t, addr, export)
	return target
}

// registryFor is a registry with dir as the working-directory share, which is
// what nearly every test here starts from.
func registryFor(t *testing.T, dir string) *Registry {
	t.Helper()
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	return r
}

// cwdShare registers dir as the working-directory share and returns it, for a
// test that asks the share's filesystem directly rather than over the wire.
func cwdShare(t *testing.T, dir string) *Share {
	t.Helper()
	share, _, ok := registryFor(t, dir).Lookup(workspace.ExportCWD)
	if !ok {
		t.Fatal("the working directory share is not registered")
	}
	return share
}

// fileidOf is what the wire would carry for a FileInfo a share returned.
func fileidOf(fi os.FileInfo) uint64 {
	return fi.Sys().(*nfsfile.FileInfo).Fileid
}

// mountCWD serves dir as /cwd and mounts it, for a test that needs nothing
// from the registry afterwards.
func mountCWD(t *testing.T, dir string) *nfsclient.Target {
	t.Helper()
	return mustMount(t, serve(t, registryFor(t, dir)), workspace.ExportCWD)
}

// rawStatus is a procedure's NFS status word, which the client library
// swallows into an error and a raw call has to read for itself.
func rawStatus(t *testing.T, client *rpc.Client, args any) (uint32, io.ReadSeeker) {
	t.Helper()
	res, err := client.Call(args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	status, err := xdr.ReadUint32(res)
	if err != nil {
		t.Fatalf("reading the status: %v", err)
	}
	return status, res
}

// nfsHeader is the RPC header every NFS procedure here starts with.
func nfsHeader(proc uint32) rpc.Header {
	return rpc.Header{
		Rpcvers: 2,
		Prog:    nfsclient.Nfs3Prog,
		Vers:    nfsclient.Nfs3Vers,
		Proc:    proc,
		Cred:    rpc.AuthNull,
		Verf:    rpc.AuthNull,
	}
}

// rawLink is LINK (RFC 1813 procedure 15) as a kernel client sends it:
// the file's handle, then the directory handle and the new name. The client
// library has no Link, which is why this is spelled out.
func rawLink(t *testing.T, client *rpc.Client, file, dir []byte, name string) uint32 {
	t.Helper()
	type linkArgs struct {
		rpc.Header
		File []byte
		Link nfsclient.Diropargs3
	}
	status, _ := rawStatus(t, client, &linkArgs{
		Header: nfsHeader(uint32(nfs.NFSProcedureLink)),
		File:   file,
		Link:   nfsclient.Diropargs3{FH: dir, Filename: name},
	})
	return status
}

// statusName spells a status the way the RFC does, so a failure names it.
func statusName(status uint32) string {
	return nfs.NFSStatus(status).String()
}
