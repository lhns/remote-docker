package workspace

import "testing"

// Both protocols ask this, and what they ask it about becomes a syscall the
// agent performs as root.
func TestValidSharePathRejects(t *testing.T) {
	for _, p := range []string{
		"",
		"relative/path",
		"/..",
		"/../etc/shadow",
		"/a/../../etc/shadow",
		"/a/./b",
		"/a//b",
		"/trailing/",
		`/back\slash`,
		"/nul\x00byte",
	} {
		if err := ValidSharePath(p); err == nil {
			t.Errorf("ValidSharePath(%q) accepted it", p)
		}
	}
}

func TestValidSharePathAccepts(t *testing.T) {
	for _, p := range []string{
		"/",
		"/main.go",
		"/pkg/deep/lib.go",
		"/a file with spaces.txt",
		"/..hidden",     // a leading dot-dot in a NAME is not a traversal
		"/dir/...weird", // nor is a three-dot name
	} {
		if err := ValidSharePath(p); err != nil {
			t.Errorf("ValidSharePath(%q) = %v, want it accepted", p, err)
		}
	}
}
