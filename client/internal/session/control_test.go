package session

import "testing"

// A share can be most of its files and a fraction of its bytes, or the other
// way round, so the status reports both. The formatter is what makes the second
// half readable.
func TestHumanBytes(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1 << 20, "1.0MB"},
		{1 << 30, "1.0GB"},
		{1 << 40, "1.0TB"},
		// Past the last unit it keeps counting in it rather than inventing one.
		{1 << 50, "1024.0TB"},
	} {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
