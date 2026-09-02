package nfsserve

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	nfsclient "github.com/willscott/go-nfs-client/nfs"
)

// A mode set through the share has to reach the file. It did not: every
// attribute write was accepted and discarded, so a binary built on a share
// linked and could not be run (integration.sh section 15d).
//
// Asserted against the file on THIS machine, not through the share: the share
// synthesises permissions, so reading it back would pass either way.
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

// A timestamp is accepted and deliberately dropped, which is the opposite of
// what a reader expects, so it is pinned. Applying it loops with the agent's
// replay -- 3063 events for one edit. See attrChange.Chtimes.
func TestChtimesThroughTheShareIsAcceptedAndNotApplied(t *testing.T) {
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

	before, err := os.Stat(filepath.Join(dir, "stamped"))
	if err != nil {
		t.Fatal(err)
	}

	var sattr nfsclient.Sattr3
	sattr.Mtime.SetIt = nfsclient.SetToClientTime
	sattr.Mtime.Time.Seconds = uint32(time.Now().Add(-48 * time.Hour).Unix())

	// Accepted: refusing reports a failure for something right to ask.
	if err := target.Setattr("stamped", sattr); err != nil {
		t.Fatalf("setting the time through the share: %v", err)
	}

	after, err := os.Stat(filepath.Join(dir, "stamped"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the mtime moved from %v to %v; applying it closes a loop with "+
			"the agent's replay", before.ModTime(), after.ModTime())
	}
}
