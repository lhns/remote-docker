package session

import (
	"slices"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// Only unions are worth asking the cache channel about, so a session whose
// record holds none never opens it and stays silent against a workspace too old
// for it.
func TestOnlyRecordedUnionsAreReattached(t *testing.T) {
	st, project, _ := store(t)
	through := shareWith(t, project, workspace.DefaultMode, workspace.ShareWords{})
	st.remember(through)

	if got := st.unionExports(); len(got) != 0 {
		t.Errorf("a write=through share is listed for the cache: %v", got)
	}

	other := t.TempDir()
	union := shareWith(t, other, workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteBack}, workspace.ShareWords{Watch: workspace.WatchPartial})
	st.remember(union)
	if got := st.unionExports(); !slices.Equal(got, []string{union.ExportPath}) {
		t.Errorf("unionExports = %v, want only %s", got, union.ExportPath)
	}
}
