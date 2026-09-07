package nfsserve

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/lhns/remote-docker/core/logx"
)

func TestParseSymlinkMode(t *testing.T) {
	for _, row := range []struct {
		in      string
		want    SymlinkMode
		refused bool
	}{
		{in: "", want: SymlinkNative},
		{in: "native", want: SymlinkNative},
		{in: "hardlink", want: SymlinkHardlink},
		{in: "symlink", refused: true},
		{in: "Hardlink", refused: true},
	} {
		got, err := ParseSymlinkMode(row.in)
		if row.refused {
			if err == nil {
				t.Errorf("ParseSymlinkMode(%q) = %q, want a refusal", row.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSymlinkMode(%q): %v", row.in, err)
		} else if got != row.want {
			t.Errorf("ParseSymlinkMode(%q) = %q, want %q", row.in, got, row.want)
		}
	}
}

// hardlinkShare is a share that translates, over a directory laid out the way
// a package manager lays one out: a bin directory beside a lib directory, with
// the link climbing out of one and into the other.
func hardlinkShare(t *testing.T) (billyFS, string) {
	t.Helper()

	dir := t.TempDir()
	for _, sub := range []string{"bin", "lib"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "prog"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(DefaultAttrs)
	r.Symlinks = SymlinkHardlink
	return r.shareFS(dir, ""), dir
}

// billyFS is the part of billy.Filesystem these tests use, named so the helper
// above can return it without importing billy for one signature.
type billyFS interface {
	Symlink(target, link string) error
}

// The case the mode exists for: a relative target, resolved against the
// directory holding the link exactly as the kernel would resolve the symlink.
func TestHardlinkModeLinksAnExistingFile(t *testing.T) {
	fs, dir := hardlinkShare(t)

	if err := fs.Symlink("../lib/prog", "bin/prog"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// What the mode costs, asserted rather than assumed: the name is an
	// ordinary file with two links, and it is not a symlink.
	fi, err := os.Lstat(filepath.Join(dir, "bin", "prog"))
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("the new name is a symlink; this mode makes hard links")
	}
	if got, err := os.ReadFile(filepath.Join(dir, "bin", "prog")); err != nil || string(got) != "payload" {
		t.Errorf("read through the link = %q, %v; want the target's content", got, err)
	}
	if _, err := os.Readlink(filepath.Join(dir, "bin", "prog")); err == nil {
		t.Error("Readlink succeeded; a hard link has no target to read")
	}
}

// Each way a hard link cannot stand in for a symlink is refused with the
// reason, rather than served as something else.
func TestHardlinkModeRefusesWhatItCannotRepresent(t *testing.T) {
	for _, row := range []struct {
		name   string
		target string
		link   string
	}{
		{name: "a target that does not exist", target: "../lib/missing", link: "bin/a"},
		{name: "a directory", target: "../lib", link: "bin/b"},
		{name: "an absolute target, which is the container's path", target: "/lib/prog", link: "bin/c"},
		{name: "a target outside the share", target: "../../outside", link: "bin/d"},
	} {
		t.Run(row.name, func(t *testing.T) {
			fs, dir := hardlinkShare(t)
			if err := fs.Symlink(row.target, row.link); err == nil {
				t.Fatalf("Symlink(%q, %q) succeeded, want a refusal", row.target, row.link)
			}
			if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(row.link))); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s was created anyway: %v", row.link, err)
			}
		})
	}
}

// The security case. An untranslated symlink out of a share is harmless
// because traversal resolves it back inside; a hard link to a file outside IS
// that file, writable through the share.
func TestHardlinkModeCannotReachOutsideTheShare(t *testing.T) {
	fs, dir := hardlinkShare(t)

	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	if err := fs.Symlink("../../"+filepath.Base(outside), "bin/escape"); err == nil {
		t.Fatal("a link to a file outside the share was created")
	}
	if _, err := os.Lstat(filepath.Join(dir, "bin", "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the escaping name exists: %v", err)
	}

	if got, err := os.ReadFile(outside); err != nil || string(got) != "secret" {
		t.Errorf("the file outside the share reads %q, %v; want it untouched", got, err)
	}
}

// The trap this detection exists to avoid: Windows refuses a symlink with
// ERROR_PRIVILEGE_NOT_HELD, which does NOT match os.ErrPermission, so the
// obvious test for it silently never fires.
func TestSymlinkPrivilegedMatchesTheErrnoAndNotErrPermission(t *testing.T) {
	privilege := &fs.PathError{Op: "symlink", Path: "link", Err: syscall.Errno(1314)}

	want := runtime.GOOS == "windows"
	if got := symlinkPrivileged(privilege); got != want {
		t.Errorf("symlinkPrivileged(ERROR_PRIVILEGE_NOT_HELD) = %v on %s, want %v", got, runtime.GOOS, want)
	}
	if symlinkPrivileged(os.ErrPermission) {
		t.Error("symlinkPrivileged matched os.ErrPermission; the Windows errno does not, which is the point")
	}
	if symlinkPrivileged(nil) {
		t.Error("symlinkPrivileged(nil) is true, so a symlink that worked would be reported as refused")
	}
}

// The message is said once per share however many links a tool makes: npm
// creates a bin entry per package, and a hundred identical warnings is a
// message nobody reads.
func TestThePrivilegeMessageIsSaidOncePerShare(t *testing.T) {
	var buf bytes.Buffer
	n := &noFollowFS{log: logx.Logger(&buf, "  ", false)}

	for range 5 {
		n.warnPrivilege()
	}

	if got := strings.Count(buf.String(), "Developer Mode"); got != 1 {
		t.Errorf("the remedy was named %d times, want once:\n%s", got, buf.String())
	}
}
