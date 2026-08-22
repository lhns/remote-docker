//go:build unix

package nfsserve

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// A socket is refused, and the reason matters: it is not that single files
// cannot be exported (they can), it is that AF_UNIX does not cross a file
// share. The same socket inside an exported DIRECTORY is equally unreachable.
func TestRegisterRefusesASocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("cannot create a unix socket here: %v", err)
	}
	defer func() { _ = l.Close() }()

	_, err = newTestRegistry(t).Register(sock)
	if err == nil {
		t.Fatal("Register(socket) = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "socket") {
		t.Errorf("the refusal is %q, and does not say what the path is", err)
	}
}
