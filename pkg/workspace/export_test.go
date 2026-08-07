package workspace

import (
	"strings"
	"testing"
)

// canonicalKeyFor takes the platform explicitly so the Windows rules are
// tested on Linux CI runners and the POSIX rules are tested on the Windows
// development machine. Neither set would otherwise ever run.
func TestCanonicalKeyForWindows(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain path", `C:\projects\myapp`, "c:/projects/myapp"},
		{"case folded", `C:\Projects\MyApp`, "c:/projects/myapp"},
		{"drive letter folded", `d:\data`, "d:/data"},
		{"forward slashes accepted", "C:/projects/myapp", "c:/projects/myapp"},
		{"mixed separators", `C:\projects/myapp\src`, "c:/projects/myapp/src"},
		{"trailing separator", `C:\projects\myapp\`, "c:/projects/myapp"},
		{"dot segment", `C:\projects\.\myapp`, "c:/projects/myapp"},
		{"parent segment", `C:\projects\other\..\myapp`, "c:/projects/myapp"},
		{"repeated separators", `C:\\projects\\\myapp`, "c:/projects/myapp"},
		{"drive root", `C:\`, "c:/"},
		{"unc share", `\\server\share\dir`, "//server/share/dir"},
		{"unc case folded", `\\Server\Share\Dir`, "//server/share/dir"},
		{"unc with dot segment", `\\server\share\.\dir`, "//server/share/dir"},
		{"extended length", `\\?\C:\projects\myapp`, "c:/projects/myapp"},
		{"extended length unc", `\\?\UNC\server\share\dir`, "//server/share/dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalKeyFor("windows", tt.in); got != tt.want {
				t.Errorf("canonicalKeyFor(windows, %q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCanonicalKeyForPOSIX(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain path", "/home/alice/project", "/home/alice/project"},
		{"trailing separator", "/home/alice/project/", "/home/alice/project"},
		{"dot segment", "/home/alice/./project", "/home/alice/project"},
		{"parent segment", "/home/alice/other/../project", "/home/alice/project"},
		{"repeated separators", "/home//alice///project", "/home/alice/project"},
		// Case is significant on a case-sensitive filesystem, so folding it
		// would merge two genuinely different directories into one share.
		{"case preserved", "/home/Alice/Project", "/home/Alice/Project"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalKeyFor("linux", tt.in); got != tt.want {
				t.Errorf("canonicalKeyFor(linux, %q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A stable id across runs is what lets a reconnecting client keep its remote
// volumes instead of orphaning one per session.
func TestShareIDIsStable(t *testing.T) {
	const p = "/home/alice/project"
	first := ShareID(p)
	for range 100 {
		if got := ShareID(p); got != first {
			t.Fatalf("ShareID(%q) is not stable: %q then %q", p, first, got)
		}
	}
	if len(first) != idLen {
		t.Errorf("ShareID length = %d, want %d", len(first), idLen)
	}
}

func TestShareIDDistinguishesDifferentPaths(t *testing.T) {
	seen := map[string]string{}
	for _, p := range []string{
		"/home/alice/project",
		"/home/alice/other",
		"/home/bob/project",
		"/home/alice/project/sub",
		"/",
	} {
		id := ShareID(p)
		if prev, ok := seen[id]; ok {
			t.Errorf("ShareID collision: %q and %q both yield %q", prev, p, id)
		}
		seen[id] = p
	}
}

func TestExportAndVolumeNamesRoundTrip(t *testing.T) {
	id := ShareID("/home/alice/project")

	export := ExportPathForID(id)
	if !strings.HasPrefix(export, ExportMountPrefix) {
		t.Errorf("ExportPathForID(%q) = %q, want the %q prefix", id, export, ExportMountPrefix)
	}
	volume := VolumeNameForID(id)
	if !IsManagedVolume(volume) {
		t.Errorf("VolumeNameForID(%q) = %q, which IsManagedVolume rejects", id, volume)
	}

	for _, s := range []string{export, volume} {
		got, err := ParseID(s)
		if err != nil {
			t.Fatalf("ParseID(%q): %v", s, err)
		}
		if got != id {
			t.Errorf("ParseID(%q) = %q, want %q", s, got, id)
		}
	}
}

// Deleting a volume we did not create would destroy a user's data, so the
// prefix check has to be strict about what it claims.
func TestIsManagedVolumeRejectsForeignNames(t *testing.T) {
	for _, name := range []string{
		"postgres-data",
		"myapp_node_modules",
		"",
		"rd",
		"RD-0011223344556677",
	} {
		if IsManagedVolume(name) {
			t.Errorf("IsManagedVolume(%q) = true, want false", name)
		}
	}
}

func TestParseIDRejects(t *testing.T) {
	for _, name := range []string{
		"/cwd",
		"/m/",
		"/m/tooshort",
		"/m/00112233445566778899",
		"/m/zzzzzzzzzzzzzzzz",
		"rd-tooshort",
		"postgres-data",
		"",
	} {
		if _, err := ParseID(name); err == nil {
			t.Errorf("ParseID(%q) = nil error, want an error", name)
		}
	}
}

func TestNFSVolumeOptions(t *testing.T) {
	opts := NFSVolumeOptions(30000, "/m/00112233445566ff")

	if opts["type"] != "nfs" {
		t.Errorf("type = %q, want nfs", opts["type"])
	}
	if opts["device"] != ":/m/00112233445566ff" {
		t.Errorf("device = %q, want :/m/00112233445566ff", opts["device"])
	}

	o := opts["o"]
	// Each of these is load-bearing and the reason is in NFSVolumeOptions'
	// doc comment; losing one silently changes failure behaviour rather than
	// breaking the mount outright, which is why they are asserted here.
	for _, want := range []string{
		"addr=127.0.0.1",
		"port=30000",
		"mountport=30000",
		"nfsvers=3",
		"nolock",
		"soft",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("options %q are missing %q", o, want)
		}
	}
}
