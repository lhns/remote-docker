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
	if err == nil || !strings.Contains(err.Error(), "REMOTE_DOCKER_SHARE_OWNER") {
		t.Errorf("ParseMode(owner=) = %v, want a refusal naming REMOTE_DOCKER_SHARE_OWNER", err)
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

func TestParseWatch(t *testing.T) {
	for in, want := range map[string]Watch{"off": WatchOff, "partial": WatchPartial, "coarse": WatchCoarse, " partial ": WatchPartial} {
		got, err := ParseWatch(in)
		if err != nil || got != want {
			t.Errorf("ParseWatch(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// The session setting's aliases are not spellings of a mount's word.
	for _, in := range []string{"", "on", "true", "1", "yes", "best-effort", "sometimes"} {
		_, err := ParseWatch(in)
		if err == nil || !strings.Contains(err.Error(), "off|partial|coarse") {
			t.Errorf("ParseWatch(%q) = %v, want a refusal naming the values", in, err)
		}
	}
}

func TestParseMountReadsWatch(t *testing.T) {
	got, err := ParseMount("write=back,watch=coarse,owner=1000")
	if err != nil {
		t.Fatal(err)
	}
	want := MountWords{Mode: Mode{Write: WriteBack}, ShareWords: ShareWords{Owner: Owner{1000, 1000, true}, Watch: WatchCoarse}}
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, err := ParseMount("watch=partial,watch=off"); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a repeated watch = %v, want a refusal naming the repeat", err)
	}
	if _, err := ParseMount("watch=on"); err == nil {
		t.Error("an alias was accepted as a mount word")
	}
}

// The session-level parser has no watch: the setting is `watch`, with its own
// aliases, and accepting the word there would do nothing.
func TestParseModeRefusesWatch(t *testing.T) {
	_, err := ParseMode("read=cached,watch=partial")
	if err == nil || !strings.Contains(err.Error(), "watch") {
		t.Errorf("ParseMode(watch=) = %v, want a refusal", err)
	}
}

func TestIsModeWordClaimsWatch(t *testing.T) {
	for _, w := range []string{"watch=partial", "watch=sometimes"} {
		if !IsModeWord(w) {
			t.Errorf("%q is a mount word and was not recognised", w)
		}
	}
}

func TestWatchAndPrefetchTakeTheStronger(t *testing.T) {
	for _, tc := range []struct{ a, b, want Watch }{
		{WatchUnset, WatchOff, WatchOff},
		{WatchOff, WatchPartial, WatchPartial},
		{WatchPartial, WatchOff, WatchPartial},
		{WatchPartial, WatchCoarse, WatchCoarse},
		{WatchCoarse, WatchPartial, WatchCoarse},
		{WatchCoarse, WatchOff, WatchCoarse},
		{WatchOff, WatchUnset, WatchOff},
		{WatchUnset, WatchUnset, WatchUnset},
	} {
		if got := tc.a.Max(tc.b); got != tc.want {
			t.Errorf("Watch(%q).Max(%q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
	for _, tc := range []struct{ a, b, want Prefetch }{
		{PrefetchUnset, PrefetchOff, PrefetchOff},
		{PrefetchOff, PrefetchTree, PrefetchTree},
		{PrefetchTree, PrefetchOff, PrefetchTree},
		{PrefetchTree, PrefetchEager, PrefetchEager},
		{PrefetchEager, PrefetchTree, PrefetchEager},
		{PrefetchEager, PrefetchOff, PrefetchEager},
		{PrefetchOff, PrefetchUnset, PrefetchOff},
	} {
		if got := tc.a.Max(tc.b); got != tc.want {
			t.Errorf("Prefetch(%q).Max(%q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParsePrefetchAndMount(t *testing.T) {
	for in, want := range map[string]Prefetch{"off": PrefetchOff, "eager": PrefetchEager, "tree": PrefetchTree} {
		if got, err := ParsePrefetch(in); err != nil || got != want {
			t.Errorf("ParsePrefetch(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParsePrefetch("lazy"); err == nil || !strings.Contains(err.Error(), "off|eager|tree") {
		t.Errorf("ParsePrefetch(lazy) = %v", err)
	}
	got, err := ParseMount("read=cached,write=back,prefetch=tree")
	if err != nil || got.Prefetch != PrefetchTree {
		t.Errorf("ParseMount = %v, %v", got, err)
	}
	if _, err := ParseMode("prefetch=tree"); err == nil || !strings.Contains(err.Error(), "prefetch") {
		t.Errorf("ParseMode(prefetch=) = %v, want a refusal", err)
	}
	if !IsModeWord("prefetch=lazy") {
		t.Error("prefetch= is a mount word")
	}
}
