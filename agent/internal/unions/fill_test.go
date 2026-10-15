package unions

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// land writes one regular file through the same path a batch takes.
func land(t *testing.T, root, name, body string) {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644,
		Size: int64(len(body)), ModTime: time.Now().Add(-time.Hour).Truncate(time.Second)}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(body))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extract(root, &b, func(string, os.FileInfo) {}); err != nil {
		t.Fatal(err)
	}
}

// sizeSpy reports the size of a file when the landing first reads the new
// content, which is after it has opened the target and before it has written.
type sizeSpy struct {
	io.Reader
	path string
	seen int64
	done bool
}

func (s *sizeSpy) Read(p []byte) (int, error) {
	if !s.done {
		s.done = true
		if st, err := os.Stat(s.path); err == nil {
			s.seen = st.Size()
		}
	}
	return s.Reader.Read(p)
}

// A file is never emptied first: a container with it mapped dies with SIGBUS on
// the next access, and one reading it gets nothing (ADR 0044).
func TestLandingNeverEmptiesTheTarget(t *testing.T) {
	root := t.TempDir()
	old := "0123456789abcdef"
	land(t, root, "index", old)

	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	spy := &sizeSpy{Reader: strings.NewReader(old), path: filepath.Join(root, "index")}
	header := &tar.Header{Name: "index", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(old))}
	if _, err := writeEntry(r, header, spy); err != nil {
		t.Fatal(err)
	}
	if spy.seen != int64(len(old)) {
		t.Errorf("the target was %d bytes while the new content was copied in, want %d", spy.seen, len(old))
	}
}

// A shorter file is cut, a longer one grows.
func TestLandingResizesTheTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "f")

	land(t, root, "f", "a long first version")
	land(t, root, "f", "short")
	if got, _ := os.ReadFile(target); string(got) != "short" {
		t.Errorf("after a shorter landing the file holds %q", got)
	}
	land(t, root, "f", "now a longer one again")
	if got, _ := os.ReadFile(target); string(got) != "now a longer one again" {
		t.Errorf("after a longer landing the file holds %q", got)
	}
}
