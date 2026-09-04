package nfsserve

import (
	"path/filepath"
	"strings"
	"testing"
)

// The share root is spelled as the bind mount was written, which on Windows is
// with forward slashes, and go-nfs joins names with the OS separator. The
// containment check must compare cleaned paths, or a create that carries a
// mode (tar, install, cp -p) fails with EIO for "leaving the share" it is in.
func TestResolveAcceptsARootSpelledWithForwardSlashes(t *testing.T) {
	dir := t.TempDir()
	c := &attrChange{root: filepath.ToSlash(dir)}
	for _, name := range []string{"a", "sub/a", filepath.FromSlash("sub/a")} {
		got, err := c.resolve(name)
		if err != nil {
			t.Errorf("resolve(%q) with a forward-slash root: %v", name, err)
			continue
		}
		if want := filepath.Join(dir, filepath.FromSlash(name)); got != want {
			t.Errorf("resolve(%q) = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"..", "../x", "sub/../../x"} {
		if _, err := c.resolve(name); err == nil || !strings.Contains(err.Error(), "leaves the share") {
			t.Errorf("resolve(%q) was allowed: %v", name, err)
		}
	}
}
