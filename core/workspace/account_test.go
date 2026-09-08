package workspace

import (
	"strings"
	"testing"
)

func TestAccountName(t *testing.T) {
	tests := map[string]string{
		"alice":         "alice",
		"Alice":         "alice",
		"ALICE":         "alice",
		"alice.smith":   "alice-smith",
		"alice@example": "alice-example",
		"alice_smith":   "alice_smith",
		"alice-smith":   "alice-smith",
		"123alice":      "alice",
		"_alice":        "_alice",
	}
	for in, want := range tests {
		got, err := AccountName(in)
		if err != nil {
			t.Errorf("AccountName(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("AccountName(%q) = %q, want %q", in, got, want)
		}
	}

	for _, in := range []string{"", "123", "---", "..."} {
		if got, err := AccountName(in); err == nil {
			t.Errorf("AccountName(%q) = %q, want an error", in, got)
		}
	}

	long, err := AccountName(strings.Repeat("a", 60))
	if err != nil {
		t.Fatal(err)
	}
	if len(long) != MaxAccountNameLength {
		t.Errorf("a long name produced %d characters, want %d", len(long), MaxAccountNameLength)
	}
}
