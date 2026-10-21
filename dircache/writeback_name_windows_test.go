//go:build windows

package dircache

import "testing"

// A name NTFS refuses cannot even be looked up: Lstat answers
// ERROR_INVALID_NAME, not "absent". The check made just before a write must
// read that as nothing there, so the write fails, the name is set aside and
// named, and the rest land; read as "changed here" it was never named at all.
func TestANameNTFSRefusesIsNamedNotTakenForAChangeHere(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	store.add("/bad?name", "x")
	store.add("/good", "y")
	c, _, clk := wbCache(t, store, root)

	rounds(t, c, clk, 2)

	if !exists(root, "good") {
		t.Error("good did not land beside a name NTFS refuses")
	}
	r := c.Reports()
	if len(r) != 1 || len(r[0].WriteBack.Skipped) != 1 || r[0].WriteBack.Skipped[0].Path != "/bad?name" {
		t.Errorf("status does not name the refused file: %+v", r)
	}
}
