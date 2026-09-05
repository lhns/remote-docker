package workspace

import (
	"strings"
	"testing"
)

var (
	cachedThrough = Mode{ReadCached, WriteThrough}
	cachedBack    = Mode{ReadCached, WriteBack}
)

// Every corner parses from every spelling to the same answer.
func TestEveryCornerParsesFromEverySpelling(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
	}{
		{"read=direct,write=through", DefaultMode},
		{"write=through,read=direct", DefaultMode},

		// Docker's own values, by what Docker says they mean.
		{"default", DefaultMode},
		{"consistent", DefaultMode},
		{"cached", cachedThrough},
		{"delegated", cachedBack},
		{"read=cached,write=through", cachedThrough},
		{"read=cached,write=back", cachedBack},
		{"read=direct,write=back", Mode{ReadDirect, WriteBack}},
		{"read=direct,write=ephemeral", Mode{ReadDirect, WriteEphemeral}},
		{"read=cached,write=ephemeral", Mode{ReadCached, WriteEphemeral}},

		// One axis alone leaves the other unset.
		{"read=cached", Mode{Read: ReadCached}},
		{"write=ephemeral", Mode{Write: WriteEphemeral}},

		{" read=cached , write=back ", cachedBack},
		{"", ModeUnset},
	} {
		got, err := ParseMode(tc.in)
		if err != nil {
			t.Errorf("ParseMode(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// A refusal names the word.
func TestRefusalsNameTheWord(t *testing.T) {
	for _, tc := range []struct {
		in   string
		name string
	}{
		{"fast", "fast"},
		{"read=fast", "fast"},
		{"write=async", "async"},
		{"speed=cached", "speed"},
		{"read=cached,read=direct", "twice"},
		{"write=back,write=through", "twice"},
		{"cached,read=direct", "twice"},
		{"cached+back", "cached+back"},
	} {
		_, err := ParseMode(tc.in)
		if err == nil {
			t.Errorf("ParseMode(%q) accepted", tc.in)
			continue
		}
		if !strings.Contains(err.Error(), tc.name) {
			t.Errorf("ParseMode(%q) refused without naming %q: %v", tc.in, tc.name, err)
		}
	}
}

// The two couplings are properties of the mode.
func TestUnionAndPrefetchFollowTheAxes(t *testing.T) {
	for _, tc := range []struct {
		mode     Mode
		union    bool
		prefetch bool
	}{
		{DefaultMode, false, false},
		{cachedThrough, false, false},
		{Mode{ReadDirect, WriteBack}, true, false},
		{cachedBack, true, true},
		{Mode{ReadDirect, WriteEphemeral}, true, false},
		{Mode{ReadCached, WriteEphemeral}, true, true},
	} {
		if got := tc.mode.Union(); got != tc.union {
			t.Errorf("%v.Union() = %v, want %v", tc.mode, got, tc.union)
		}
		if got := tc.mode.Prefetch(); got != tc.prefetch {
			t.Errorf("%v.Prefetch() = %v, want %v", tc.mode, got, tc.prefetch)
		}
	}
}

// Or fills each axis independently.
func TestOrFillsEachAxisIndependently(t *testing.T) {
	def := cachedBack
	if got := (Mode{Read: ReadDirect}).Or(def); got != (Mode{ReadDirect, WriteBack}) {
		t.Errorf("read alone: %v", got)
	}
	if got := (Mode{Write: WriteEphemeral}).Or(def); got != (Mode{ReadCached, WriteEphemeral}) {
		t.Errorf("write alone: %v", got)
	}
	if got := ModeUnset.Or(def); got != def {
		t.Errorf("unset: %v", got)
	}
	if got := DefaultMode.Or(def); got != DefaultMode {
		t.Errorf("set outranks fallback: %v", got)
	}
}

// The attribute cache follows the read axis and nothing else.
func TestAttributeOptionsFollowRead(t *testing.T) {
	cached := strings.Join(attributeOptions(ReadCached), ",")
	direct := strings.Join(attributeOptions(ReadDirect), ",")
	if !strings.Contains(cached, "actimeo=60") || !strings.Contains(cached, "nocto") {
		t.Errorf("cached: %s", cached)
	}
	if !strings.Contains(direct, "actimeo=1") || strings.Contains(direct, "nocto") {
		t.Errorf("direct: %s", direct)
	}
}

// IsModeWord claims every word on either axis, a misspelt value included, so
// ParseMode refuses it rather than the daemon seeing it.
func TestIsModeWordClaimsEveryAxisWord(t *testing.T) {
	for _, w := range []string{"read=direct", "write=back", "cached", "delegated", "default", "read=fast", "write=async"} {
		if !IsModeWord(w) {
			t.Errorf("%q is a mode word and was not recognised", w)
		}
	}
	for _, w := range []string{"ro", "rw", "z", "nocopy", "rshared", "read", "cached+back"} {
		if IsModeWord(w) {
			t.Errorf("%q is not a mode word and was recognised", w)
		}
	}
}
