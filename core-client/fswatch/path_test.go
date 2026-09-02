package fswatch

import (
	"slices"
	"testing"

	"github.com/lhns/remote-docker/core/notify"
	"github.com/lhns/remote-docker/core/workspace"
)

func TestSplitLocal(t *testing.T) {
	tests := []struct {
		goos string
		in   string
		want []string
	}{
		{"linux", "/home/u/proj", []string{"home", "u", "proj"}},
		{"linux", "/home/u/proj/", []string{"home", "u", "proj"}},
		{"linux", "/home//u///proj", []string{"home", "u", "proj"}},
		{"linux", "/", nil},
		// A backslash is an ordinary filename character on Linux and must not
		// be mistaken for a separator.
		{"linux", `/home/u/a\b`, []string{"home", "u", `a\b`}},

		{"windows", `C:\projects\foo`, []string{"C:", "projects", "foo"}},
		{"windows", `C:/projects/foo`, []string{"C:", "projects", "foo"}},
		{"windows", `C:\`, []string{"C:"}},
		{"windows", `\\?\C:\projects\foo`, []string{"C:", "projects", "foo"}},
		{"windows", `\\?\UNC\server\share\x`, []string{"server", "share", "x"}},
		{"windows", `\\server\share\x`, []string{"server", "share", "x"}},
		// Case is preserved. The result is rejoined into a path that must
		// exist on the workspace's case-sensitive filesystem.
		{"windows", `C:\Projects\Foo`, []string{"C:", "Projects", "Foo"}},

		{"darwin", "/Users/u/proj", []string{"Users", "u", "proj"}},
	}

	for _, tt := range tests {
		got := splitLocal(tt.goos, tt.in)
		if !slices.Equal(got, tt.want) {
			t.Errorf("splitLocal(%q, %q) = %q, want %q", tt.goos, tt.in, got, tt.want)
		}
	}
}

func TestRelativeTo(t *testing.T) {
	tests := []struct {
		name  string
		goos  string
		root  string
		local string
		want  string
		ok    bool
	}{
		{"under root", "linux", "/home/u/proj", "/home/u/proj/src/a.go", "/src/a.go", true},
		{"the root itself", "linux", "/home/u/proj", "/home/u/proj", "/", true},
		{"trailing slash on root", "linux", "/home/u/proj/", "/home/u/proj/a", "/a", true},
		{"outside", "linux", "/home/u/proj", "/home/u/other/a.go", "", false},
		{"shorter than root", "linux", "/home/u/proj", "/home/u", "", false},
		// A sibling whose name merely starts with the root's name. A
		// byte-prefix comparison would wrongly accept this; component-wise
		// matching does not.
		{"sibling prefix", "linux", "/home/u/proj", "/home/u/proj2/a.go", "", false},
		{"case matters on linux", "linux", "/home/u/proj", "/home/u/Proj/a.go", "", false},

		// The Windows case that motivates the whole file: the root came from
		// the command line, the event came from the OS with on-disk casing.
		{"root lowercase, event on-disk", "windows", `c:\projects\foo`, `C:\Projects\Foo\src\a.ts`, "/src/a.ts", true},
		{"root on-disk, event lowercase", "windows", `C:\Projects\Foo`, `c:\projects\foo\src\a.ts`, "/src/a.ts", true},
		// The TAIL keeps the event's casing, not the root's, because the workspace
		// filesystem is case-sensitive.
		{"tail casing preserved", "windows", `c:\projects\foo`, `C:\Projects\Foo\Src\App.TS`, "/Src/App.TS", true},
		{"extended-length root", "windows", `\\?\C:\projects\foo`, `C:\projects\foo\a`, "/a", true},
		{"unc", "windows", `\\server\share`, `\\server\share\a\b`, "/a/b", true},
		{"different drive", "windows", `C:\projects`, `D:\projects\a`, "", false},

		{"darwin folds case", "darwin", "/Users/u/Proj", "/users/u/proj/a.go", "/a.go", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := relativeTo(tt.goos, splitLocal(tt.goos, tt.root), tt.local)
			if ok != tt.ok {
				t.Fatalf("relativeTo(%q, %q, %q) ok = %v, want %v", tt.goos, tt.root, tt.local, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("relativeTo(%q, %q, %q) = %q, want %q", tt.goos, tt.root, tt.local, got, tt.want)
			}
		})
	}
}

// Whatever relativeTo produces goes on the wire, so it has to satisfy the
// validation the agent will apply to it. A path this package can generate but
// the agent refuses would present as "notifications do not work for this one
// file", with nothing in either log saying why.
func TestRelativeToProducesValidWirePaths(t *testing.T) {
	cases := []struct{ goos, root, local string }{
		{"linux", "/home/u/proj", "/home/u/proj"},
		{"linux", "/home/u/proj", "/home/u/proj/src/a.go"},
		{"linux", "/home/u/proj", "/home/u/proj//src///a.go"},
		{"linux", "/home/u/proj", "/home/u/proj/.env"},
		{"linux", "/home/u/proj", "/home/u/proj/..leading/trailing../a...b"},
		{"linux", "/home/u/proj", "/home/u/proj/café/résumé.txt"},
		{"windows", `C:\projects\foo`, `C:\Projects\Foo\Src\App.TS`},
		{"windows", `\\server\share`, `\\server\share\a\b`},
		{"darwin", "/Users/u/Proj", "/users/u/proj/a.go"},
	}

	for _, c := range cases {
		got, ok := relativeTo(c.goos, splitLocal(c.goos, c.root), c.local)
		if !ok {
			t.Fatalf("relativeTo(%q, %q, %q) refused a path under the root", c.goos, c.root, c.local)
		}
		event := notify.Event{Export: workspace.ExportCWD, Path: got, Op: notify.OpWrite}
		if err := event.Validate(); err != nil {
			t.Errorf("relativeTo(%q, %q, %q) = %q, which the agent would reject: %v",
				c.goos, c.root, c.local, got, err)
		}
	}
}

// A "." component is dropped rather than becoming an empty or literal one --
// fsnotify has been seen to report paths built by joining, and a literal "."
// would be refused on the far side.
func TestRelativeToDropsDotComponents(t *testing.T) {
	got, ok := relativeTo("linux", splitLocal("linux", "/home/u/proj"), "/home/u/proj/./src/./a.go")
	if !ok || got != "/src/a.go" {
		t.Errorf(`relativeTo with "." components = %q, %v; want "/src/a.go", true`, got, ok)
	}
}

func TestCaseInsensitive(t *testing.T) {
	for goos, want := range map[string]bool{
		"windows": true, "darwin": true, "linux": false, "freebsd": false,
	} {
		if got := caseInsensitive(goos); got != want {
			t.Errorf("caseInsensitive(%q) = %v, want %v", goos, got, want)
		}
	}
}
