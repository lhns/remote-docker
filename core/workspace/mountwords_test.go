package workspace

import (
	"io/fs"
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

func TestParseMasks(t *testing.T) {
	for in, want := range map[string]fs.FileMode{"0": 0, "0111": 0o111, "111": 0o111, "022": 0o022, " 22 ": 0o022, "777": 0o777, "0777": 0o777} {
		got, err := ParseFileMask(in)
		if err != nil || got != (Mask{Bits: want, Set: true}) {
			t.Errorf("ParseFileMask(%q) = %v, %v; want %04o", in, got, err, want)
		}
	}
	for _, in := range []string{"", "8", "0999", "1000", "00777", "-1", "+1", "0o111", "1_1", "a"} {
		_, err := ParseFileMask(in)
		if err == nil || !strings.Contains(err.Error(), "fmask") || !strings.Contains(err.Error(), "octal") {
			t.Errorf("ParseFileMask(%q) = %v, want a refusal naming fmask and the forms", in, err)
		}
	}
	if m, err := ParseDirMask("022"); err != nil || m.Bits != 0o022 {
		t.Errorf("ParseDirMask(022) = %v, %v", m, err)
	}
	// One execute bit left is enough to enter; none is not.
	if _, err := ParseDirMask("0110"); err != nil {
		t.Errorf("ParseDirMask(0110): %v", err)
	}
	for _, in := range []string{"0111", "0777", "0133"} {
		if _, err := ParseDirMask(in); err == nil || !strings.Contains(err.Error(), "execute") {
			t.Errorf("ParseDirMask(%q) = %v, want a refusal naming the execute bits", in, err)
		}
	}
	if s := (Mask{Bits: 0o22, Set: true}).String(); s != "0022" {
		t.Errorf("String = %q, want 0022", s)
	}
}

func TestParseMountReadsMasks(t *testing.T) {
	got, err := ParseMount("write=back,fmask=0111,dmask=022")
	if err != nil {
		t.Fatal(err)
	}
	want := MountWords{Mode: Mode{Write: WriteBack}, ShareWords: ShareWords{FMask: Mask{0o111, true}, DMask: Mask{0o022, true}}}
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	// A named 0 is set, so it can override a session default.
	if got, err := ParseMount("fmask=0"); err != nil || got.FMask != (Mask{Set: true}) {
		t.Errorf("fmask=0 = %v, %v", got, err)
	}
	for in, says := range map[string]string{
		"fmask=1,fmask=2": "twice",
		"dmask=1,dmask=2": "twice",
		"fmask=0999":      "fmask",
		"dmask=x":         "dmask",
		"dmask=0111":      "execute",
	} {
		if _, err := ParseMount(in); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("ParseMount(%q) = %v, want a refusal naming %s", in, err, says)
		}
	}
}

func TestParseModeRefusesMasksAndNamesTheSettings(t *testing.T) {
	for word, setting := range map[string]string{"fmask=0111": "REMOTE_DOCKER_SHARE_FMASK", "dmask=022": "REMOTE_DOCKER_SHARE_DMASK"} {
		if _, err := ParseMode("read=cached," + word); err == nil || !strings.Contains(err.Error(), setting) {
			t.Errorf("ParseMode(%s) = %v, want a refusal naming %s", word, err, setting)
		}
		if !IsModeWord(word) {
			t.Errorf("%q is a mount word and was not recognised", word)
		}
	}
	// Claimed by its key, so a misspelt value is refused rather than forwarded.
	if !IsModeWord("fmask=rw") || IsModeWord("fmask") {
		t.Error("IsModeWord claims fmask= by its key, and only with a value")
	}
}
