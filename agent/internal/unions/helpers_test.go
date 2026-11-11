package unions

import (
	"io"
	"os"
	"testing"

	"github.com/lhns/remote-docker/core/cache"
)

// rootAt is a directory opened as the Root the agent works through.
func rootAt(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// pulled is pull with the spooled tar read back and removed.
func pulled(t *testing.T, upper *os.Root, paths []string, carry bool) ([]byte, []cache.Unreadable, error) {
	t.Helper()
	spool, skipped, err := pull(upper, paths, carry)
	if err != nil {
		return nil, skipped, err
	}
	defer func() { _ = spool.Close() }()
	b, rerr := io.ReadAll(spool)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if int64(len(b)) != spool.Size {
		t.Errorf("the spool is %d bytes and says %d", len(b), spool.Size)
	}
	return b, skipped, nil
}
