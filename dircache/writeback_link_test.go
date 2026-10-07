package dircache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// openRoot opens dir as a share root for the test's lifetime.
func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// linkMaker makes link a directory link to target, skipping the test when this
// account cannot.
type linkMaker struct {
	name string
	make func(t *testing.T, link, target string)
}

// linkMakers always has the symlink; windows_test.go adds the junction.
var linkMakers = []linkMaker{{"symlink", makeSymlink}}

func makeSymlink(t *testing.T, link, target string) {
	t.Helper()
	err := os.Symlink(target, link)
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == 1314 { // ERROR_PRIVILEGE_NOT_HELD
		t.Skip("symlink creation is privileged on this account")
	}
	if err != nil {
		t.Fatal(err)
	}
}

// A link in the share pointing outside it must not carry a write-back out: the
// container controls what links exist in a write=back share.
func TestWriteUnderRefusesALinkLeavingTheShare(t *testing.T) {
	for _, lm := range linkMakers {
		t.Run(lm.name, func(t *testing.T) {
			base := t.TempDir()
			share, outside := filepath.Join(base, "share"), filepath.Join(base, "outside")
			for _, d := range []string{share, outside} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			lm.make(t, filepath.Join(share, "link"), outside)

			r := openRoot(t, share)
			for _, p := range []string{"/link/new.txt", "/link/sub/new.txt"} {
				err := writeUnder(r, File{Path: p, Mode: 0o644, ModTime: time.Now(), Body: strings.NewReader("x")})
				if err == nil {
					t.Errorf("writeUnder(%q) through a link out of the share succeeded", p)
				}
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				t.Errorf("%s appeared outside the share", e.Name())
			}
		})
	}
}

// A delete the container made under a link out of the share removes nothing
// outside it, even when the file there matches what the fill recorded.
func TestWriteBackShareDoesNotDeleteThroughALink(t *testing.T) {
	for _, lm := range linkMakers {
		t.Run(lm.name, func(t *testing.T) {
			base := t.TempDir()
			share, outside := filepath.Join(base, "share"), filepath.Join(base, "outside")
			for _, d := range []string{share, outside} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			victim := filepath.Join(outside, "victim.txt")
			if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			lm.make(t, filepath.Join(share, "link"), outside)

			store := &fakeStore{changes: []Change{{Path: "/link/victim.txt", Deleted: true}}}
			c := cacheWith(t, store)
			const id = "/m/1111111111111111"
			c.shares.set(id, share, &shareState{Report: Report{Done: true}, Cached: true}, nil)
			c.shares.noteSent(id, share, []Entry{{Path: "link/victim.txt", Size: 4}})

			c.writeBackShare(t.Context(), id)

			if _, err := os.Stat(victim); err != nil {
				t.Errorf("a delete under a link removed a file outside the share: %v", err)
			}
		})
	}
}

// A link that stays inside the share keeps working, as it does on a plain mount.
func TestWriteUnderFollowsALinkInsideTheShare(t *testing.T) {
	for _, lm := range linkMakers {
		t.Run(lm.name, func(t *testing.T) {
			if lm.name == "junction" {
				t.Skip("os.Root refuses every junction on Windows, inside the share or out")
			}
			share := t.TempDir()
			real := filepath.Join(share, "real")
			if err := os.Mkdir(real, 0o755); err != nil {
				t.Fatal(err)
			}
			lm.make(t, filepath.Join(share, "link"), real)

			err := writeUnder(openRoot(t, share), File{Path: "/link/f.txt", Mode: 0o644, Body: strings.NewReader("x")})
			if err != nil {
				t.Fatalf("writeUnder through an inside link: %v", err)
			}
			if _, err := os.Stat(filepath.Join(real, "f.txt")); err != nil {
				t.Errorf("the write did not land through the link: %v", err)
			}
		})
	}
}
