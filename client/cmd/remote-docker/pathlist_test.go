package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// PATH is not a thing to be careless with: it is long, it is hand-curated, and
// a user cannot reconstruct it from memory. Every case here is one where a
// plausible implementation loses something.
func TestAppendPath(t *testing.T) {
	sep := string(filepath.ListSeparator)

	for _, tc := range []struct {
		name    string
		current string
		dir     string
		want    string
		added   bool
	}{
		{
			name:    "appended, never prepended",
			current: join("/usr/bin", "/bin"),
			dir:     "/home/me/.local/bin",
			want:    join("/usr/bin", "/bin", "/home/me/.local/bin"),
			added:   true,
		},
		{
			name:    "already there, left exactly alone",
			current: join("/usr/bin", "/home/me/.local/bin", "/bin"),
			dir:     "/home/me/.local/bin",
			want:    join("/usr/bin", "/home/me/.local/bin", "/bin"),
		},
		{
			name:    "a trailing separator is not a second entry",
			current: join("/usr/bin", "/bin") + sep,
			dir:     "/opt/bin",
			want:    join("/usr/bin", "/bin", "/opt/bin"),
			added:   true,
		},
		{
			name:    "an empty PATH becomes just this",
			current: "",
			dir:     "/opt/bin",
			want:    "/opt/bin",
			added:   true,
		},
		{
			name:    "a trailing slash is the same directory",
			current: join("/usr/bin", "/opt/bin/"),
			dir:     "/opt/bin",
			want:    join("/usr/bin", "/opt/bin/"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, added := appendPath(tc.current, tc.dir)
			if added != tc.added {
				t.Errorf("added = %v, want %v", added, tc.added)
			}
			if got != tc.want {
				t.Errorf("PATH = %q, want %q", got, tc.want)
			}
		})
	}
}

// Windows compares paths case-insensitively and Linux does not, and treating
// them alike would be wrong on one of the two.
func TestCaseFoldingFollowsThePlatform(t *testing.T) {
	current := join(`C:\Users\Me\bin`)
	_, added := appendPath(current, `c:\users\me\BIN`)

	if filepath.Separator == '\\' {
		if added {
			t.Error("the same directory in another case was added a second time")
		}
		return
	}
	if !added {
		t.Error("two directories differing in case were treated as one, which they are not here")
	}
}

func TestRemoveFromPath(t *testing.T) {
	t.Run("every occurrence, not the first", func(t *testing.T) {
		current := join("/usr/bin", "/opt/bin", "/bin", "/opt/bin")
		got, removed := removeFromPath(current, "/opt/bin")
		if !removed {
			t.Fatal("nothing was removed")
		}
		if got != join("/usr/bin", "/bin") {
			t.Errorf("PATH = %q", got)
		}
	})

	t.Run("absent is not an error and changes nothing", func(t *testing.T) {
		current := join("/usr/bin", "/bin")
		got, removed := removeFromPath(current, "/opt/bin")
		if removed {
			t.Error("it claimed to remove something that was not there")
		}
		if got != current {
			t.Errorf("PATH = %q, want it untouched", got)
		}
	})

	// An empty entry in PATH means the current directory. Dropping one while
	// tidying up would change what the user's shell resolves -- quietly, and
	// in a way that looks like nothing happened.
	t.Run("an empty entry in the middle survives", func(t *testing.T) {
		sep := string(filepath.ListSeparator)
		current := "/usr/bin" + sep + sep + "/opt/bin"
		got, removed := removeFromPath(current, "/opt/bin")
		if !removed {
			t.Fatal("nothing was removed")
		}
		if got != "/usr/bin"+sep {
			t.Errorf("PATH = %q, want the empty entry kept", got)
		}
	})
}

func join(parts ...string) string {
	return strings.Join(parts, string(filepath.ListSeparator))
}
