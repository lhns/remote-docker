//go:build linux

package unions

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO a container made is left out rather than blocking the channel that
// answers one request at a time.
func TestPullSkipsAFifoWithoutBlocking(t *testing.T) {
	upper := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(upper, "pipe"), 0o644); err != nil {
		t.Skipf("cannot make a fifo here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(upper, "good"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, carry := range []bool{false, true} {
		done := make(chan []byte, 1)
		go func() {
			out, _, _ := pulled(t, rootAt(t, upper), []string{"/pipe", "/good"}, carry)
			done <- out
		}()
		select {
		case out := <-done:
			tr := tar.NewReader(bytes.NewReader(out))
			h, err := tr.Next()
			if err != nil || h.Name != "good" {
				t.Fatalf("carry=%v: first entry = %v, %v; want good", carry, h, err)
			}
			if _, err := tr.Next(); !errors.Is(err, io.EOF) {
				t.Errorf("carry=%v: the fifo was carried: %v", carry, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("carry=%v: a pull blocked on a fifo", carry)
		}
	}
}
