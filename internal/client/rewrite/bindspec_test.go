package rewrite

import "testing"

func TestParseBind(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want BindSpec
	}{
		{"posix source", "/home/alice/src:/app", BindSpec{Source: "/home/alice/src", Target: "/app"}},
		{"posix with options", "/home/alice/src:/app:ro", BindSpec{Source: "/home/alice/src", Target: "/app", Options: "ro"}},
		{"multiple options", "/src:/app:rw,z", BindSpec{Source: "/src", Target: "/app", Options: "rw,z"}},
		{"named volume", "pgdata:/var/lib/postgresql/data", BindSpec{Source: "pgdata", Target: "/var/lib/postgresql/data"}},
		{"anonymous volume", "/data", BindSpec{Target: "/data"}},
		{"relative source", "./src:/app", BindSpec{Source: "./src", Target: "/app"}},
		{"parent relative source", "../shared:/shared", BindSpec{Source: "../shared", Target: "/shared"}},

		// The reason this parser exists: a colon is both the field separator
		// and part of a drive letter.
		{"windows source", `C:\projects\app:/app`, BindSpec{Source: `C:\projects\app`, Target: "/app"}},
		{"windows source with options", `C:\projects\app:/app:ro`, BindSpec{Source: `C:\projects\app`, Target: "/app", Options: "ro"}},
		{"windows forward slashes", "C:/projects/app:/app", BindSpec{Source: "C:/projects/app", Target: "/app"}},
		{"windows lowercase drive", `d:\data:/data`, BindSpec{Source: `d:\data`, Target: "/data"}},
		{"windows drive root", `C:\:/app`, BindSpec{Source: `C:\`, Target: "/app"}},
		{"unc source", `\\server\share:/mnt`, BindSpec{Source: `\\server\share`, Target: "/mnt"}},

		// A named volume called "c" must not be mistaken for a drive letter.
		// The separator check is what distinguishes them.
		{"single letter volume name", "c:/app", BindSpec{Source: "c", Target: "/app"}},
		{"single letter volume with options", "c:/app:ro", BindSpec{Source: "c", Target: "/app", Options: "ro"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBind(tt.spec)
			if err != nil {
				t.Fatalf("ParseBind(%q): %v", tt.spec, err)
			}
			if got != tt.want {
				t.Errorf("ParseBind(%q)\n   got: source=%q target=%q options=%q\n  want: source=%q target=%q options=%q",
					tt.spec,
					got.Source, got.Target, got.Options,
					tt.want.Source, tt.want.Target, tt.want.Options)
			}
		})
	}
}

// Every spec must survive a parse/render round trip unchanged, or a bind we
// decline to rewrite would come back subtly different from what the user wrote.
func TestParseBindRoundTrips(t *testing.T) {
	for _, spec := range []string{
		"/home/alice/src:/app",
		"/home/alice/src:/app:ro",
		"pgdata:/var/lib/postgresql/data",
		"/data",
		`C:\projects\app:/app`,
		`C:\projects\app:/app:ro`,
		"C:/projects/app:/app",
		`\\server\share:/mnt`,
		"c:/app:rw,z",
	} {
		parsed, err := ParseBind(spec)
		if err != nil {
			t.Fatalf("ParseBind(%q): %v", spec, err)
		}
		if got := parsed.String(); got != spec {
			t.Errorf("round trip of %q produced %q", spec, got)
		}
	}
}

func TestParseBindRejects(t *testing.T) {
	for _, spec := range []string{
		"",
		"   ",
		"/a:/b:ro:extra",
	} {
		if got, err := ParseBind(spec); err == nil {
			t.Errorf("ParseBind(%q) = %+v, want an error", spec, got)
		}
	}
}

func TestIsLocalPath(t *testing.T) {
	local := []string{
		"/home/alice",
		"/",
		".",
		"..",
		"./src",
		"../shared",
		`.\src`,
		`..\shared`,
		`C:\projects`,
		"C:/projects",
		`d:\data`,
		`\\server\share`,
	}
	for _, s := range local {
		if !IsLocalPath(s) {
			t.Errorf("IsLocalPath(%q) = false, want true", s)
		}
	}

	// Named volumes must be left alone. Rewriting one would replace the
	// user's own persistent volume with an NFS export of a directory that
	// does not exist.
	volumes := []string{
		"",
		"pgdata",
		"my_app_node_modules",
		"c",
		"volume-with-dashes",
		"C",
	}
	for _, s := range volumes {
		if IsLocalPath(s) {
			t.Errorf("IsLocalPath(%q) = true, want false", s)
		}
	}
}
