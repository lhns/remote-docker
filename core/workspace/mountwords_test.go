package workspace

import (
	"strings"
	"testing"
)

func TestParseOwner(t *testing.T) {
	for _, tc := range []struct {
		in       string
		uid, gid uint32
	}{
		{"1000", 1000, 1000},
		{"0", 0, 0},
		{" 1000 ", 1000, 1000},
		{"1000:2000", 1000, 2000},
		{"1000.2000", 1000, 2000},
		{"1000/2000", 1000, 2000},
		{"1000-2000", 1000, 2000},
		{"1000;2000", 1000, 2000},
		{"1000é2000", 1000, 2000},
		{"4294967295.4294967295", 4294967295, 4294967295},
	} {
		got, err := ParseOwner(tc.in)
		if err != nil {
			t.Errorf("ParseOwner(%q): %v", tc.in, err)
			continue
		}
		if got != (Owner{UID: tc.uid, GID: tc.gid, Set: true}) {
			t.Errorf("ParseOwner(%q) = %v, want %d:%d", tc.in, got, tc.uid, tc.gid)
		}
	}
}

// Every refusal names the accepted forms.
func TestParseOwnerRefusals(t *testing.T) {
	for _, in := range []string{
		"", "alice", "1000.", ".2000", "1000..2000", "1000.2000.3000", "-1", "+1",
		"1000=2000", "1000,2000", "4294967296", "1000.4294967296", "1000:alice",
	} {
		_, err := ParseOwner(in)
		if err == nil {
			t.Errorf("ParseOwner(%q) accepted", in)
			continue
		}
		if !strings.Contains(err.Error(), "1000.2000") {
			t.Errorf("ParseOwner(%q) refused without naming the forms: %v", in, err)
		}
	}
}

func TestParseMount(t *testing.T) {
	got, err := ParseMount("read=cached,owner=1000.2000,write=back")
	if err != nil {
		t.Fatal(err)
	}
	want := MountWords{Mode: Mode{ReadCached, WriteBack}, ShareWords: ShareWords{Owner: Owner{1000, 2000, true}}}
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}

	// Docker's word and ours together.
	got, err = ParseMount("delegated,owner=7")
	if err != nil || got.Mode != (Mode{ReadCached, WriteBack}) || got.Owner.UID != 7 {
		t.Errorf("delegated,owner=7 = %v, %v", got, err)
	}

	// Nothing named leaves everything unset.
	if got, err := ParseMount(""); err != nil || got != (MountWords{}) {
		t.Errorf("empty = %v, %v", got, err)
	}

	for _, in := range []string{"owner=1,owner=2", "owner=x", "owner", "read=cached,speed=3"} {
		if _, err := ParseMount(in); err == nil {
			t.Errorf("ParseMount(%q) accepted", in)
		}
	}
}

// The session-level parser, which reads consistency, consistencyPaths and
// REMOTE_DOCKER_CONSISTENCY, has no owner: that is a setting of its own, and
// accepting it there would silently do nothing.
func TestParseModeRefusesOwnerAndNamesTheSetting(t *testing.T) {
	_, err := ParseMode("read=cached,owner=1000")
	if err == nil || !strings.Contains(err.Error(), "shareOwner") {
		t.Errorf("ParseMode(owner=) = %v, want a refusal naming shareOwner", err)
	}
}

func TestIsModeWordClaimsOwner(t *testing.T) {
	for _, w := range []string{"owner=1000", "owner=x", " owner=1000.2000"} {
		if !IsModeWord(w) {
			t.Errorf("%q is a mount word and was not recognised", w)
		}
	}
	if IsModeWord("owner") {
		t.Error("a bare owner is not a word")
	}
}
