package nfsserve

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	nfs "github.com/willscott/go-nfs"
	nfsclient "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// readDirPlusArgs is READDIRPLUS3args, with the counts the test chooses.
type readDirPlusArgs struct {
	rpc.Header
	FH         []byte
	Cookie     uint64
	CookieVerf uint64
	DirCount   uint32
	MaxCount   uint32
}

// readDirPlusPage is one READDIRPLUS reply, decoded by hand so the test holds
// the cookie and the verifier the client library keeps to itself.
type readDirPlusPage struct {
	verifier uint64
	entries  []nfsclient.EntryPlus
	eof      bool
}

// readDirPlus issues one READDIRPLUS with the smallest counts go-nfs accepts
// (nfs_onreaddirplus.go refuses less than 512/4096 with TOOSMALL), which is
// what makes a 3,000-entry directory page: about seven entries a reply.
func readDirPlus(t *testing.T, client *rpc.Client, dir []byte, cookie, verifier uint64) readDirPlusPage {
	t.Helper()
	type head struct {
		DirAttrs   nfsclient.PostOpAttr
		CookieVerf uint64
	}
	type item struct {
		IsSet bool                `xdr:"union"`
		Entry nfsclient.EntryPlus `xdr:"unioncase=1"`
	}

	status, res := rawStatus(t, client, &readDirPlusArgs{
		Header:     nfsHeader(nfsclient.NFSProc3ReadDirPlus),
		FH:         dir,
		Cookie:     cookie,
		CookieVerf: verifier,
		DirCount:   512,
		MaxCount:   4096,
	})
	if status != nfsclient.NFS3Ok {
		t.Fatalf("READDIRPLUS at cookie %d with verifier %#x: %s", cookie, verifier, statusName(status))
	}
	var h head
	if err := xdr.Read(res, &h); err != nil {
		t.Fatalf("decoding the reply head: %v", err)
	}
	page := readDirPlusPage{verifier: h.CookieVerf}
	for {
		var it item
		if err := xdr.Read(res, &it); err != nil {
			t.Fatalf("decoding an entry: %v", err)
		}
		if !it.IsSet {
			break
		}
		page.entries = append(page.entries, it.Entry)
	}
	if err := xdr.Read(res, &page.eof); err != nil {
		t.Fatalf("decoding eof: %v", err)
	}
	return page
}

// A listing paged through a directory that changes under it.
//
// A build writes into the directory it is listing, and `rm -r` removes what it
// has just read. Two failures are silent here: an entry listed twice, which
// makes `cp -r` and `tar` fail on the copy, and an entry dropped, which makes
// them succeed with a file missing. What this pins is go-nfs's answer: the
// listing is a SNAPSHOT keyed by the verifier (helpers/cachinghandler.go), so a
// page asked for with the first reply's verifier is served from what the
// first reply saw, and a file created or removed mid-scan is not reflected
// until a scan starts over. That is the RFC's intent for the verifier, and it
// is what keeps the cookie meaningful when the sorted listing shifts.
func TestReadDirPlusPagesThroughAChangingDirectory(t *testing.T) {
	dir := t.TempDir()
	const total = 3000
	for i := range total {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
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

	seen := map[string]int{}
	removed := map[string]bool{}
	created := map[string]bool{}
	var cookie, verifier uint64
	pages := 0
	for {
		page := readDirPlus(t, client, root, cookie, verifier)
		pages++
		if cookie == 0 {
			verifier = page.verifier
		} else if page.verifier != verifier {
			t.Fatalf("page %d came back with verifier %#x after the scan started with %#x", pages, page.verifier, verifier)
		}
		if len(page.entries) == 0 && !page.eof {
			t.Fatalf("page %d at cookie %d is empty and not eof", pages, cookie)
		}
		for _, e := range page.entries {
			if e.FileName == "." || e.FileName == ".." {
				continue
			}
			seen[e.FileName]++
			if !e.Attr.IsSet || !e.Handle.IsSet {
				t.Errorf("entry %q came with no attributes or no handle", e.FileName)
			} else if e.FileId != e.Attr.Attr.Fileid {
				t.Errorf("entry %q: fileid %#x in the entry, %#x in its attributes", e.FileName, e.FileId, e.Attr.Attr.Fileid)
			}
			cookie = e.Cookie
		}
		if page.eof {
			break
		}

		// Change the directory between pages 3 and 4: five new files, and five
		// the scan has already listed taken away, through the same server.
		if pages == 3 {
			for i := range 5 {
				name := fmt.Sprintf("new%d", i)
				if _, err := target.Create(name, 0o644); err != nil {
					t.Fatalf("creating %s mid-scan: %v", name, err)
				}
				created[name] = true
			}
			for _, e := range page.entries {
				if len(removed) == 5 {
					break
				}
				if e.FileName == "." || e.FileName == ".." {
					continue
				}
				if err := target.Remove(e.FileName); err != nil {
					t.Fatalf("removing %s mid-scan: %v", e.FileName, err)
				}
				removed[e.FileName] = true
			}
		}
	}
	t.Logf("%d entries over %d pages, verifier %#x", len(seen), pages, verifier)

	for i := range total {
		name := fmt.Sprintf("f%04d", i)
		switch n := seen[name]; {
		case removed[name]:
			// Listed once before it went; a second listing would be a duplicate.
			if n != 1 {
				t.Errorf("%s was removed after being listed and appears %d times", name, n)
			}
		case n == 0:
			t.Errorf("%s existed for the whole scan and was never listed", name)
		case n > 1:
			t.Errorf("%s existed for the whole scan and was listed %d times", name, n)
		}
	}
	for name := range created {
		if seen[name] != 0 {
			t.Errorf("%s was created mid-scan and appeared in that scan; the verifier no longer names a snapshot", name)
		}
	}

	fresh := readDirPlus(t, client, root, 0, 0)
	if fresh.verifier == verifier {
		t.Errorf("a fresh scan after five creates and five removes carries the old verifier %#x", verifier)
	}
	status, _ := rawStatus(t, client, &readDirPlusArgs{
		Header: nfsHeader(nfsclient.NFSProc3ReadDirPlus),
		FH:     root, Cookie: 2, CookieVerf: verifier ^ 1, DirCount: 512, MaxCount: 4096,
	})
	if status != uint32(nfs.NFSStatusBadCookie) {
		t.Errorf("a verifier no scan produced was answered %s, want NFS3ERR_BAD_COOKIE", statusName(status))
	}
}
