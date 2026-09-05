package nfsserve

import (
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	nfsclient "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// mountAt performs an NFSv3 MOUNT and returns a usable target.
//
// The client's own Mount helper cannot be used: it reaches the NFS program
// through rpcbind on port 111, and this server, like the deployment it
// models, serves MOUNT and NFS on one port with no portmapper anywhere. That
// is the same reason the mount options say port == mountport.
// dialWithRetry works around the host, not the code under test.
//
// These tests make many short-lived connections in quick succession, and on
// Windows each one holds its source port in TIME_WAIT afterwards. The dial
// then intermittently fails with "Only one usage of each socket address",
// which says nothing about the NFS server and everything about the machine
// running the test. It is a test-harness concern: the real client opens one
// connection per session, not dozens in a loop.
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

func mountAt(t *testing.T, addr, export string) (*nfsclient.Target, error) {
	t.Helper()
	client, fh, err := mountRaw(t, addr, export)
	if err != nil {
		return nil, err
	}
	// The same connection serves the NFS program, so no second dial.
	target, err := nfsclient.NewTargetWithClient(client, rpc.AuthNull, fh, export, 0)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("opening target: %w", err)
	}
	return target, nil
}

// mountRaw is the MOUNT call alone: the connection and the root handle, for a
// test that has to speak an NFS procedure the client library hides.
func mountRaw(t *testing.T, addr, export string) (*rpc.Client, []byte, error) {
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
		client.Close()
		return nil, nil, fmt.Errorf("mount call: %w", err)
	}

	status, err := xdr.ReadUint32(res)
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("reading mount status: %w", err)
	}
	if status != nfsclient.MNT3Ok {
		client.Close()
		return nil, nil, fmt.Errorf("mount of %q refused with status %d", export, status)
	}

	fh, err := xdr.ReadOpaque(res)
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("reading file handle: %w", err)
	}
	_, _ = xdr.ReadUint32List(res)
	return client, fh, nil
}

// mustMount fails the test if the mount is refused.
func mustMount(t *testing.T, addr, export string) *nfsclient.Target {
	t.Helper()
	target, err := mountAt(t, addr, export)
	if err != nil {
		t.Fatalf("mounting %q: %v", export, err)
	}
	t.Cleanup(func() { target.Close() })
	return target
}
