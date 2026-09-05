package union

import (
	"slices"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

func testSpec() Spec {
	return Spec{
		PID:      4242,
		Export:   "/m/00112233445566ff",
		Port:     30001,
		CacheDir: "/var/lib/docker/volumes/rd-aabbccdd-00112233445566ff/_data",
		Read:     workspace.ReadCached,
	}
}

// Every path a share uses is derived from its export, so two shares cannot
// collide and the same share resolves to the same places on every reconnect.
func TestSpecPaths(t *testing.T) {
	s := testSpec()

	if got, want := s.Lower(), "/run/rd-union/00112233445566ff/lower"; got != want {
		t.Errorf("Lower() = %q, want %q", got, want)
	}
	if got, want := s.Merged(), "/run/rd-union/00112233445566ff/merged"; got != want {
		t.Errorf("Merged() = %q, want %q", got, want)
	}

	// The cache layer lives in the volume, because the kernel refuses a union
	// upper on overlayfs and a dind's own root is overlayfs.
	for _, p := range []string{s.Upper(), s.Work()} {
		if !strings.HasPrefix(p, s.CacheDir+"/") {
			t.Errorf("%q is not inside the cache volume %q", p, s.CacheDir)
		}
	}
	if s.Upper() == s.Work() {
		t.Error("the upper and the work directory are the same path")
	}

	// The working-directory share is the commonest of all -- it is what
	// `-v .:/app` becomes -- and it has no id to strip.
	cwd := Spec{Export: workspace.ExportCWD}
	if got, want := cwd.Merged(), "/run/rd-union/cwd/merged"; got != want {
		t.Errorf("the cwd share landed at %q, want %q", got, want)
	}
}

// The lower is the same mount a share's volume would have been given, asked of
// the contract rather than copied, so the two cannot drift.
//
// Split into the two halves mount(2) takes, which that list is not already in:
// it is written for docker's local volume driver, and the driver separates
// kernel FLAGS from filesystem options before it calls mount(2). Handed over
// whole, the NFS client's parser refuses the entire list and reports EINVAL --
// `invalid argument`, about a list whose every word is valid on its own. It is
// why the union never mounted at all.
func TestSpecLowerMount(t *testing.T) {
	source, fstype, options, flags := testSpec().LowerMount()

	if fstype != "nfs" {
		t.Errorf("fstype = %q, want nfs", fstype)
	}
	if source != ":/m/00112233445566ff" {
		t.Errorf("source = %q, want the export path", source)
	}
	for _, want := range []string{"addr=127.0.0.1", "port=30001", "mountport=30001",
		"nfsvers=3", "soft", "nolock", "rsize=1048576"} {
		if !strings.Contains(options, want) {
			t.Errorf("options %q are missing %q", options, want)
		}
	}

	// noatime is MS_NOATIME and not something the NFS client parses.
	if strings.Contains(options, "noatime") {
		t.Errorf("options %q still carry a kernel mount flag", options)
	}
	if !slices.Contains(flags, "noatime") {
		t.Errorf("noatime was dropped rather than carried as a flag: %v", flags)
	}

	// An empty element is an EINVAL of its own, and splitting is where one
	// would be introduced.
	for _, part := range strings.Split(options, ",") {
		if part == "" {
			t.Errorf("options %q have an empty element", options)
		}
	}

	// The lower carries the share's read mode (ADR 0044).
	if !strings.Contains(options, "actimeo=60") || !strings.Contains(options, "nocto") {
		t.Errorf("options %q do not carry the cached read mode the spec asked for", options)
	}

	direct := testSpec()
	direct.Read = workspace.ReadDirect
	_, _, options, _ = direct.LowerMount()
	if !strings.Contains(options, "actimeo=1,") || strings.Contains(options, "nocto") {
		t.Errorf("options %q do not carry the direct read mode the spec asked for", options)
	}
}

// -f is load-bearing. Without it fuse-overlayfs daemonises, the agent's child
// exits at once, and that is indistinguishable from the union dying on the
// spot -- which would make supervision impossible.
func TestSpecArgsStayInTheForeground(t *testing.T) {
	args := testSpec().Args()

	if args[0] != Binary {
		t.Errorf("args[0] = %q, want %q", args[0], Binary)
	}
	foreground := false
	for _, a := range args {
		if a == "-f" {
			foreground = true
		}
	}
	if !foreground {
		t.Errorf("args = %q, want -f so the process can be supervised", args)
	}
	if args[len(args)-1] != testSpec().Merged() {
		t.Errorf("args = %q, want the merged path last", args)
	}
}

// The spec is what the agent assembled, and every field becomes a privileged
// mount inside somebody's daemon.
func TestSpecValidate(t *testing.T) {
	if err := testSpec().Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}

	for _, c := range []struct {
		name string
		spec Spec
		want string
	}{
		{"an export this program does not serve", Spec{Export: "/etc", Port: 1, CacheDir: "/x", Read: workspace.ReadCached}, "export"},
		{"no port to reach the client on", Spec{Export: workspace.ExportCWD, CacheDir: "/x", Read: workspace.ReadCached}, "port"},
		{"a cache that is not a path", Spec{Export: workspace.ExportCWD, Port: 1, CacheDir: "relative", Read: workspace.ReadCached}, "cache directory"},
		{"a cache that is the root", Spec{Export: workspace.ExportCWD, Port: 1, CacheDir: "/", Read: workspace.ReadCached}, "cache directory"},
		// The read mode becomes the lower's attribute cache, and a value
		// nobody defined would be mounted as whatever the parser made of it.
		{"a read mode that is not one", Spec{Export: workspace.ExportCWD, Port: 1, CacheDir: "/x", Read: "fast"}, "read mode"},
		{"no read mode at all", Spec{Export: workspace.ExportCWD, Port: 1, CacheDir: "/x"}, "read mode"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.Validate()
			if err == nil {
				t.Fatalf("Validate() accepted %+v", c.spec)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("Validate() = %v, want it to name %q", err, c.want)
			}
		})
	}
}

// The child is handed its spec through the environment and validates it again
// on arrival: it runs as root inside somebody's daemon, and "the parent
// checked" is not a property this side can see.
func TestEnvRoundTrip(t *testing.T) {
	spec := testSpec()
	env := map[string]string{}
	for _, kv := range Env(spec) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	getenv := func(k string) string { return env[k] }

	got, mode, err := FromEnv(getenv)
	if err != nil {
		t.Fatalf("FromEnv() = %v", err)
	}
	if mode != ModeServe {
		t.Errorf("mode = %q, want %q", mode, ModeServe)
	}
	if got != spec {
		t.Errorf("FromEnv() = %+v, want %+v", got, spec)
	}
}

func TestFromEnvRefusesWhatItCannotUse(t *testing.T) {
	base := map[string]string{}
	for _, kv := range Env(testSpec()) {
		k, v, _ := strings.Cut(kv, "=")
		base[k] = v
	}

	for _, c := range []struct{ name, key, value string }{
		{"no mode at all", "RD_UNION_MODE", ""},
		{"a mode this agent does not have", "RD_UNION_MODE", "promote"},
		{"a port that is not a number", "RD_UNION_PORT", "thirty thousand"},
		{"an export nothing serves", "RD_UNION_EXPORT", "/etc"},
		{"a cache directory that is not a path", "RD_UNION_CACHE", "relative"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			env[c.key] = c.value

			if _, _, err := FromEnv(func(k string) string { return env[k] }); err == nil {
				t.Errorf("FromEnv accepted %s=%q", c.key, c.value)
			}
		})
	}
}
