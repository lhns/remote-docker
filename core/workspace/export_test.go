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
	volume := VolumeNameForID("", id)
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
	opts := NFSVolumeOptions(30000, "/m/00112233445566ff", ReadDirect)

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

// The read mode varies the attribute caching and nothing else, which is what
// makes a mode switch a volume recreation rather than a migration.
func TestNFSVolumeOptionsVaryOnlyTheAttributeCache(t *testing.T) {
	live := NFSVolumeOptions(30000, "/m/00112233445566ff", ReadDirect)["o"]
	cached := NFSVolumeOptions(30000, "/m/00112233445566ff", ReadCached)["o"]

	if live == cached {
		t.Fatal("cached produced the same mount options as consistent")
	}
	if !strings.Contains(live, "actimeo=1,") || strings.Contains(live, "nocto") {
		t.Errorf("consistent options = %q, want the short attribute cache and no nocto", live)
	}
	if !strings.Contains(cached, "actimeo=60,") || !strings.Contains(cached, "nocto") {
		t.Errorf("cached options = %q, want a long attribute cache and nocto", cached)
	}

	// Everything a mount needs to work at all is the same in both, so the two
	// differ in caching and in nothing that could break one of them.
	for _, want := range []string{"addr=127.0.0.1", "port=30000", "nfsvers=3", "soft", "rsize=1048576"} {
		if !strings.Contains(cached, want) {
			t.Errorf("cached options %q are missing %q", cached, want)
		}
	}

	// Unset is what a mount that named nothing gets, and it must not be a
	// third behaviour: the workspace default decides before this is reached.
	if unset := NFSVolumeOptions(30000, "/m/00112233445566ff", ReadUnset)["o"]; unset != live {
		t.Errorf("unset options = %q, want the same as consistent %q", unset, live)
	}
}

// A volume belongs to the machine that created it, because the daemon is
// shared between an account's machines and the files behind a share are on one
// of them.
//
// Without this both machines derive `rd-cwd` for their own working directory,
// the second create silently returns the first's volume, and a container comes
// up reading somebody else's project.
func TestVolumeNamesCarryTheClient(t *testing.T) {
	id := ShareID("/home/alice/project")

	phone := VolumeNameForID("aabbccdd", id)
	pc := VolumeNameForID("11223344", id)
	if phone == pc {
		t.Fatalf("two machines derived the same volume name for one path: %q", phone)
	}
	for _, name := range []string{phone, pc} {
		if !IsManagedVolume(name) {
			t.Errorf("%q is not recognised as ours", name)
		}
	}

	// And the working directory, which is the one that actually collided.
	phoneCWD, err := VolumeNameForExport("aabbccdd", ExportCWD)
	if err != nil {
		t.Fatal(err)
	}
	pcCWD, err := VolumeNameForExport("11223344", ExportCWD)
	if err != nil {
		t.Fatal(err)
	}
	if phoneCWD == pcCWD {
		t.Errorf("both machines named the working directory volume %q", phoneCWD)
	}
}

func TestParseVolumeName(t *testing.T) {
	id := ShareID("/home/alice/project")

	for _, tc := range []struct {
		name         string
		wantClient   string
		wantShare    string
		wantAccepted bool
	}{
		{VolumeNameForID("aabbccdd", id), "aabbccdd", id, true},
		{VolumeNameForID("aabbccdd", "cwd"), "aabbccdd", "cwd", true},

		// From before clients were named. Still ours, still collectable, and
		// reported with no client rather than refused.
		{VolumeNameForID("", id), "", id, true},
		{"rd-cwd", "", "cwd", true},

		// A volume somebody else named. The prefix alone is not enough: a user
		// is entitled to call a volume rd-backups.
		{"rd-backups", "", "", false},
		{"rd-", "", "", false},
		{"postgres-data", "", "", false},
		{"rd-aabbccdd-nothex", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, share, ok := ParseVolumeName(tc.name)
			if ok != tc.wantAccepted {
				t.Fatalf("ParseVolumeName(%q) accepted = %v, want %v", tc.name, ok, tc.wantAccepted)
			}
			if !ok {
				return
			}
			if client != tc.wantClient || share != tc.wantShare {
				t.Errorf("ParseVolumeName(%q) = %q, %q; want %q, %q",
					tc.name, client, share, tc.wantClient, tc.wantShare)
			}
		})
	}
}

// The same key is the same machine, and a different key is a different one.
func TestClientID(t *testing.T) {
	a := ClientID([]byte("ssh-ed25519 AAAA...alice-laptop"))
	b := ClientID([]byte("ssh-ed25519 AAAA...alice-phone"))

	if a == b {
		t.Error("two keys produced one client id")
	}
	if a != ClientID([]byte("ssh-ed25519 AAAA...alice-laptop")) {
		t.Error("one key produced two client ids")
	}
	if len(a) != clientIDLen {
		t.Errorf("client id %q is %d characters, want %d", a, len(a), clientIDLen)
	}
	// It goes in a volume name, so it has to survive being read back out.
	if _, _, ok := ParseVolumeName(VolumeNameForID(a, "cwd")); !ok {
		t.Errorf("a volume named for client %q does not parse", a)
	}
}

// A share's cache volume is that share's, as far as anything asking is
// concerned. A name the collector could not attribute would be a volume nothing
// claims and everything leaves alone, which is how disk disappears quietly.
func TestCacheVolumesBelongToTheirShare(t *testing.T) {
	const client = "aabbccdd"
	id := ShareID("/home/alice/project")

	share := VolumeNameForID(client, id)
	cache := VolumeNameForCache(client, id)

	if share == cache {
		t.Fatalf("the share and its cache have one name: %q", share)
	}
	if !IsManagedVolume(cache) {
		t.Errorf("%q is not recognised as ours", cache)
	}
	if !IsCacheVolume(cache) || IsCacheVolume(share) {
		t.Errorf("the cache role is not told apart: %q, %q", cache, share)
	}

	gotClient, gotShare, ok := ParseVolumeName(cache)
	if !ok {
		t.Fatalf("ParseVolumeName(%q) did not recognise it", cache)
	}
	if gotClient != client {
		t.Errorf("client = %q, want %q", gotClient, client)
	}
	if gotShare != id {
		t.Errorf("share = %q, want %q, the same share the volume backs", gotShare, id)
	}

	// The working directory is the commonest share of all and has no hex id.
	cwd, err := VolumeNameForExport(client, ExportCWD)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ParseVolumeName(cwd + "-" + CacheRole); !ok {
		t.Errorf("the cwd share's cache is not recognised: %q", cwd+"-"+CacheRole)
	}
}
