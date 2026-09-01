package workspace

import "testing"

func TestParseConsistency(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Consistency
	}{
		{"", Unset},
		{"default", Default},
		{"consistent", Consistent},
		{"cached", Cached},
		{"delegated", Delegated},
		{"  cached  ", Cached}, // a config file written by a person
	} {
		got, err := ParseConsistency(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseConsistency(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}

	// Docker's four words and no others: a fifth would be a spelling nothing
	// else in the toolchain accepts.
	for _, in := range []string{"Cached", "fast", "rw", "ro,cached", "none"} {
		if _, err := ParseConsistency(in); err == nil {
			t.Errorf("ParseConsistency(%q) was accepted", in)
		}
	}
}

func TestIsConsistency(t *testing.T) {
	for _, in := range []string{"cached", "delegated", "consistent", "default"} {
		if !IsConsistency(in) {
			t.Errorf("IsConsistency(%q) = false", in)
		}
	}
	// The empty string is the absence of an answer, not one of the words, so
	// an empty option in a `-v` list is not read as a consistency.
	for _, in := range []string{"", "ro", "rw", "z", "nocopy"} {
		if IsConsistency(in) {
			t.Errorf("IsConsistency(%q) = true", in)
		}
	}
}

// Precedence lives in one place: what was asked for wins, and Unset is the only
// thing that falls through. `default` is an answer somebody gave, so it stops
// the fall.
func TestConsistencyOr(t *testing.T) {
	if got := Unset.Or(Cached); got != Cached {
		t.Errorf("Unset.Or(Cached) = %q", got)
	}
	if got := Default.Or(Cached); got != Default {
		t.Errorf("Default.Or(Cached) = %q, want the value that was asked for", got)
	}
	if got := Cached.Or(Unset); got != Cached {
		t.Errorf("Cached.Or(Unset) = %q", got)
	}
}
