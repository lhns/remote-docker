package nfsserve

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/lhns/remote-docker/core/logx"
)

// The errno rule of symlink_windows.go, pinned in both directions because the
// obvious spelling of it never fires.
func TestSymlinkPrivilegedMatchesTheErrnoAndNotErrPermission(t *testing.T) {
	// The shape os.Symlink returns, measured on 2026-09-07.
	privilege := &os.LinkError{Op: "symlink", Old: "target", New: "link", Err: syscall.Errno(1314)}

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

// One message per share however many links a tool makes (symlink.go).
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
