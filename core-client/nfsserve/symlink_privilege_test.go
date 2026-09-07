package nfsserve

import (
	"bytes"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/lhns/remote-docker/core/logx"
)

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
