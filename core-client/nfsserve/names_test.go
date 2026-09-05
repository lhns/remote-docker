package nfsserve

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// The rule itself, asserted on every platform: it is Windows that applies it,
// and a developer on Linux must still be able to see what it refuses.
func TestNTFSNameRule(t *testing.T) {
	refused := []string{
		"star*", "question?", `quote"`, "pipe|", "lt<gt>", "colon:", "tab\there",
		"nl\nhere", "del\x7f", "trailing.", "trailing ", "nul", "NUL", "Nul.txt",
		"con", "PRN", "aux.log", "COM1", "com9", "LPT1", "lpt9.old",
	}
	for _, name := range refused {
		err := ntfsNameError(name)
		if !errors.Is(err, syscall.EINVAL) {
			t.Errorf("ntfsNameError(%q) = %v, want EINVAL", name, err)
		}
	}
	accepted := []string{
		"", "plain", ".hidden", "a.b.c", "with space", "trailing.txt", "nul_", "nulx",
		"console", "COM0", "COM10", "LPT0", "lptx", "comma,", "unicode-ünïcode",
		"semicolon;", "dash-", "under_", "tilde~", "dot.in.middle", "aux2",
	}
	for _, name := range accepted {
		if err := ntfsNameError(name); err != nil {
			t.Errorf("ntfsNameError(%q) = %v, want accepted", name, err)
		}
	}
}

// On a Windows host a name the rule refuses is refused on the wire, at CREATE
// and at every other way a name is made, and nothing is left on disk. Gated
// because the refusal is checkNewName's, which accepts everything elsewhere:
// Linux can create and delete `star*` with no trouble at all.
func TestWindowsHostRefusesANameItCouldNotDelete(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the name rule applies to a Windows host only")
	}
	dir := t.TempDir()
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	target := mustMount(t, serve(t, r), "/cwd")

	if _, err := target.Create("star*", 0o644); err == nil {
		t.Error("CREATE of star* succeeded; the host could never remove it")
	}
	if _, err := target.Mkdir("nul", 0o755); err == nil {
		t.Error("MKDIR of nul succeeded")
	}
	if _, err := target.Create("fine", 0o644); err != nil {
		t.Fatalf("CREATE of an ordinary name: %v", err)
	}
	if err := target.Rename("fine", "question?"); err == nil {
		t.Error("RENAME onto question? succeeded")
	}
	if err := target.Symlink("fine", "trailing."); err == nil {
		t.Error("SYMLINK named trailing. succeeded")
	}

	// OpenFile with O_CREATE is not a procedure NFS has, so it is asked of the
	// filesystem directly.
	fs := shareFS(dir, "")
	if _, err := fs.OpenFile(`quote"`, os.O_CREATE|os.O_WRONLY, 0o644); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("OpenFile(O_CREATE) of quote\" = %v, want EINVAL", err)
	}
	if err := fs.MkdirAll("ok/pipe|/deeper", 0o755); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("MkdirAll through pipe| = %v, want EINVAL", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "fine" {
		t.Errorf("the share directory holds %q, want only the one ordinary file", names)
	}
	if _, err := os.Stat(filepath.Join(dir, "ok")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("MkdirAll left its leading component behind: %v", err)
	}
}
