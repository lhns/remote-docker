package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// A setting is honoured from every source that claims to carry it, or this
// fails and names the field.
//
// Adding one setting touches six places today: a Workspace field, a clause in
// applyWorkspace, an Env constant, a clause in applyEnv, sometimes an Overrides
// field and a clause in applyOverrides. Four functions enumerating one list is
// how a setting ends up readable from the config file and silently ignored from
// the environment, which fails as "I set the variable and nothing happened",
// with nothing on screen and nothing in a log.
//
// The table-driven rewrite that would make this structurally impossible needs
// generics or reflection in the IMPLEMENTATION, where the explicit clauses are
// currently readable and correct. So the enumeration stays and the drift is
// caught here instead: reflection in a test costs nothing at runtime and fails
// loudly at the moment the sixth place is forgotten.

// settingSources is what each Workspace field must be reachable through.
//
// A field named here with no entry fails the test below; that is the point. If
// a setting genuinely belongs in the file and nowhere else, say so with an
// explicit zero-valued entry and the reason.
var settingSources = map[string]struct {
	env      string // the environment variable, "" if none
	override bool   // whether Overrides carries it

	// record marks a field that is not a setting at all: something this
	// program WROTE about what it built, rather than something a user
	// provides from a source. It still has to be declared here -- the point of
	// this table is that a new field cannot be added silently -- but there is
	// no source to honour it from, so the tests below skip it and say why.
	record bool
	// sample is what to set it to, in the string form the file and the
	// environment both use.
	sample string
	// want reads the resolved value back for comparison.
	want func(Config) string
}{
	"Host": {
		env: EnvHost, override: true, sample: "workspace.example",
		want: func(c Config) string { return c.Host },
	},
	"Port": {
		env: EnvPort, override: true, sample: "2299",
		want: func(c Config) string { return itoa(c.Port) },
	},
	"CAFile": {
		env: EnvCAFile, override: true, sample: "/etc/ca.pem",
		want: func(c Config) string { return c.CAFile },
	},
	"Insecure": {
		env: EnvInsecure, override: true, sample: "true",
		want: func(c Config) string {
			if c.Insecure {
				return "true"
			}
			return ""
		},
	},
	"User": {
		env: EnvUser, override: true, sample: "alice",
		want: func(c Config) string { return c.User },
	},
	"Endpoint": {
		env: EnvEndpoint, override: true, sample: "/tmp/rd.sock",
		want: func(c Config) string { return c.Endpoint },
	},
	"Watch": {
		env: EnvWatch, override: true, sample: "partial",
		want: func(c Config) string { return c.Watch },
	},
	"WatchBudget": {
		env: EnvWatchBudget, override: false, sample: "4096",
		want: func(c Config) string { return itoa(c.WatchBudget) },
	},
	"WatchExclude": {
		env: EnvWatchExclude, override: false, sample: "node_modules",
		want: func(c Config) string {
			if len(c.WatchExclude) == 0 {
				return ""
			}
			return c.WatchExclude[0]
		},
	},
	// The samples are spelled the way time.Duration prints them, so a value
	// that arrived can be compared with the string that set it.
	"IdleTimeout": {
		env: EnvIdleTimeout, override: false, sample: "1m30s",
		want: func(c Config) string { return dur(c.IdleTimeout) },
	},
	"DaemonIdle": {
		env: EnvDaemonIdle, override: false, sample: "45m0s",
		want: func(c Config) string { return dur(c.DaemonIdle) },
	},

	// Not a setting. `machine create` writes it and `rm` reads it to know
	// there is a machine to destroy; no environment variable or flag provides
	// it, and Resolve does not carry it, because the commands that care read
	// the file directly.
	"Machine": {record: true},
}

// Every field of Workspace is accounted for. A new setting fails here first,
// with its own name in the message, rather than in a bug report.
func TestEverySettingHasASource(t *testing.T) {
	ws := reflect.TypeOf(Workspace{})
	for i := range ws.NumField() {
		name := ws.Field(i).Name
		if _, ok := settingSources[name]; !ok {
			t.Errorf("Workspace.%s has no entry in settingSources: "+
				"add one saying which sources honour it, and check applyWorkspace, "+
				"applyEnv and applyOverrides actually do", name)
		}
	}
}

// The config file's flat form sets it.
func TestEverySettingIsHonouredFromTheFile(t *testing.T) {
	for name, src := range settingSources {
		t.Run(name, func(t *testing.T) {
			if src.record {
				t.Skip("a record of what was built, not a setting any source provides")
			}
			path := writeConfigJSON(t, map[string]any{jsonName(name): typed(name, src.sample)})

			cfg, err := Resolve(Overrides{}, path)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got := src.want(cfg); got != src.sample {
				t.Errorf("%s from the file = %q, want %q", name, got, src.sample)
			}
		})
	}
}

// And a keyed workspace sets it, over the flat form.
func TestEverySettingIsHonouredFromAKeyedWorkspace(t *testing.T) {
	for name, src := range settingSources {
		t.Run(name, func(t *testing.T) {
			if src.record {
				t.Skip("a record of what was built, not a setting any source provides")
			}
			path := writeConfigJSON(t, map[string]any{
				"workspaces": map[string]any{
					"dev": map[string]any{jsonName(name): typed(name, src.sample)},
				},
				"default": "dev",
			})

			cfg, err := Resolve(Overrides{}, path)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got := src.want(cfg); got != src.sample {
				t.Errorf("%s from a keyed workspace = %q, want %q", name, got, src.sample)
			}
		})
	}
}

// The environment sets it, and this is the half that goes missing: the file
// clause and the env clause are different functions, and only the file one is
// obvious when a setting is added.
func TestEverySettingIsHonouredFromTheEnvironment(t *testing.T) {
	for name, src := range settingSources {
		if src.env == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv(src.env, src.sample)

			cfg, err := Resolve(Overrides{}, filepath.Join(t.TempDir(), "absent.json"))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got := src.want(cfg); got != src.sample {
				t.Errorf("%s from %s = %q, want %q", name, src.env, got, src.sample)
			}
		})
	}
}

// Every Overrides field lands on the Config, and every setting that claims to
// be overridable has one. The command line is the highest-precedence source, so
// a flag that resolves to nothing is a flag that lies.
func TestOverridesAreHonouredAndComplete(t *testing.T) {
	for name, src := range settingSources {
		if !src.override {
			continue
		}
		if _, ok := reflect.TypeOf(Overrides{}).FieldByName(name); !ok {
			t.Errorf("%s claims to be overridable but Overrides has no such field", name)
		}
	}

	o := Overrides{
		Host:     "flag.example",
		Port:     2244,
		User:     "carol",
		Endpoint: "/tmp/flag.sock",
		Watch:    "coarse",
	}
	// Everything the file says, so a missing override clause shows as the
	// file's value surviving rather than as an empty one.
	path := writeConfigJSON(t, map[string]any{
		"host": "file.example", "port": 22, "user": "alice",
		"endpoint": "/tmp/file.sock", "watch": "off",
	})

	cfg, err := Resolve(o, path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"Host", cfg.Host, o.Host},
		{"Port", itoa(cfg.Port), "2244"},
		{"User", cfg.User, o.User},
		{"Endpoint", cfg.Endpoint, o.Endpoint},
		{"Watch", cfg.Watch, o.Watch},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want the override %q", tc.name, tc.got, tc.want)
		}
	}
}

// The two durations were environment-only until this test said so out loud,
// which is what made it a decision rather than an oversight, and then an
// obviously wrong one: somebody wanting a longer idle on a slow link had to
// export a variable rather than write it next to their host. They are file
// settings now, covered by the table above like everything else.
//
// What remains worth pinning here is the FORM. They are strings in the file
// and durations on the Config, and a duration that will not parse is ignored
// rather than fatal, so a typo costs the setting, never the command.
func TestTheDurationSettingsParseFromTheFile(t *testing.T) {
	path := writeConfigJSON(t, map[string]any{
		"host": "workspace.example", "idleTimeout": "90s", "daemonIdle": "45m",
	})

	cfg, err := Resolve(Overrides{}, path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.IdleTimeout != 90*time.Second {
		t.Errorf("IdleTimeout = %v, want 90s", cfg.IdleTimeout)
	}
	if cfg.DaemonIdle != 45*time.Minute {
		t.Errorf("DaemonIdle = %v, want 45m", cfg.DaemonIdle)
	}
}

// A duration nobody can parse costs the setting and nothing else. `enroll` and
// `workspace ls` connect to nothing and must still work.
func TestAMalformedDurationIsIgnoredRatherThanFatal(t *testing.T) {
	path := writeConfigJSON(t, map[string]any{
		"host": "workspace.example", "idleTimeout": "half an hour",
	})

	cfg, err := Resolve(Overrides{}, path)
	if err != nil {
		t.Fatalf("a malformed duration broke Resolve: %v", err)
	}
	if cfg.IdleTimeout != 0 {
		t.Errorf("IdleTimeout = %v, want the zero that means the default", cfg.IdleTimeout)
	}
	if cfg.Host != "workspace.example" {
		t.Errorf("Host = %q; the rest of the config should have survived", cfg.Host)
	}
}

// dur reports a zero duration as the empty string, so "the setting did not
// arrive" reads the same way as it does for the numeric settings.
func dur(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

// jsonName is the field's spelling in the file, taken from the struct tag
// rather than guessed: the tag is the format, and a rename that forgot the
// tag would otherwise pass by being wrong in both places at once.
func jsonName(field string) string {
	f, ok := reflect.TypeOf(Workspace{}).FieldByName(field)
	if !ok {
		return field
	}
	tag := f.Tag.Get("json")
	for i, r := range tag {
		if r == ',' {
			return tag[:i]
		}
	}
	return tag
}

// typed converts a sample to the JSON type that field holds.
func typed(field, sample string) any {
	f, _ := reflect.TypeOf(Workspace{}).FieldByName(field)
	switch f.Type.Kind() {
	case reflect.Int:
		return atoi(sample)
	case reflect.Bool:
		// The file says true, not "true": a switch that only works when it is
		// spelled as a string would be a bug this test exists to catch.
		return sample == "true"
	case reflect.Slice:
		return []string{sample}
	default:
		return sample
	}
}

func writeConfigJSON(t *testing.T, body map[string]any) string {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling the config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}
	return path
}

// itoa reports a zero as the empty string, so "the setting did not arrive"
// and "the setting arrived as 0" read the same way in a failure message --
// which is correct here, since 0 is what every numeric setting means by unset.
func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
