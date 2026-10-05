package accounts

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// purgeStore creates real home directories, owned by whoever runs the test, so
// the uids it hands out start there.
func purgeStore(t *testing.T) *testStore {
	t.Helper()
	s := newStore(t)
	s.prov.homes = t.TempDir()
	if uid := os.Getuid(); uid >= 0 {
		s.Mapping.UIDBase = uid
	}
	return s
}

func TestPurgeRemovesTheHomeAndTheUserAndKeepsTheUID(t *testing.T) {
	s := purgeStore(t)
	if err := s.CreateAccount("bob", newKey(t), ""); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Lookup("bob")
	if err := os.WriteFile(filepath.Join(before.Home, "notes"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Purge("bob"); err == nil {
		t.Fatal("Purge removed an account that is still enrolled")
	}
	if err := s.RemoveAccountFile("bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.Purge("bob"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := os.Stat(before.Home); !os.IsNotExist(err) {
		t.Errorf("bob's home is still there: %v", err)
	}
	if uid, ok := s.prov.removed["bob"]; !ok || uid != before.UID {
		t.Errorf("removed %v, want bob's uid %d", s.prov.removed, before.UID)
	}

	uids, err := s.loadUIDs()
	if err != nil || uids["bob"] != before.UID {
		t.Fatalf("uidmap %v (%v), want bob kept at %d", uids, err, before.UID)
	}
	// A bound token for bob appends a key; he comes back as the same uid,
	// with a new, empty home.
	if _, err := s.AppendKey("bob", newKey(t), ""); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Lookup("bob")
	if after.UID != before.UID || len(after.Keys) != 1 {
		t.Errorf("bob came back as %+v, want uid %d", after, before.UID)
	}
	if entries, err := os.ReadDir(after.Home); err != nil || len(entries) != 0 {
		t.Errorf("bob's new home: %v %v, want empty", entries, err)
	}
}

func TestRemoveHomeRefusesAnyPathButTheRecordedHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "rd-bob")
	other := filepath.Join(root, "elsewhere")
	for _, dir := range []string{home, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bob := &Account{Name: "bob", UID: os.Getuid(), Home: home}

	for _, tc := range []struct {
		name string
		a    *Account
		path string
	}{
		{"a path outside the home", bob, other},
		{"the home's parent", bob, root},
		{"no home recorded", &Account{Name: "bob", UID: bob.UID}, other},
		{"a relative home", &Account{Name: "bob", UID: bob.UID, Home: "rd-bob"}, "rd-bob"},
		{"the root", &Account{Name: "bob", UID: bob.UID, Home: string(filepath.Separator)}, string(filepath.Separator)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := removeHome(tc.a, tc.path); err == nil {
				t.Errorf("removeHome(%q) was not refused", tc.path)
			}
		})
	}
	for _, dir := range []string{home, other, root} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}
}

func TestRemoveHomeRefusesALink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "rd-bob")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if err := removeHome(&Account{Name: "bob", UID: os.Getuid(), Home: link}, link); err == nil {
		t.Error("a home that is a link was removed")
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the link's target: %v", err)
	}
}

func TestRemoveHomeRefusesAnotherUIDsDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ownership is read on Linux only")
	}
	home := t.TempDir()
	if err := removeHome(&Account{Name: "bob", UID: os.Getuid() + 1, Home: home}, home); err == nil {
		t.Error("a home owned by another uid was removed")
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("the home: %v", err)
	}
}
