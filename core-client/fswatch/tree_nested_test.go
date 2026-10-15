package fswatch

import (
	"path/filepath"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// A share of a directory the outer share's walk skipped by name is still
// walked: only a root at the very same path may share a walk.
func TestANestedShareOfAnExcludedDirectoryIsWalked(t *testing.T) {
	outer := mkdirs(t, t.TempDir(), "node_modules", "src")
	inner := filepath.Join(outer, "node_modules")
	tr := newTestTree(t, newFakeBackend(), 100, []string{"node_modules"})
	tr.sync([]Share{
		{ExportPath: "/m/1111111111111111", LocalPath: outer, Mode: workspace.WatchPartial},
		{ExportPath: "/m/2222222222222222", LocalPath: inner, Mode: workspace.WatchPartial},
	})
	if !tr.watching(inner) {
		t.Errorf("the share of %s has no watch: the outer walk excluded it and the inner one skipped its own", inner)
	}
}
