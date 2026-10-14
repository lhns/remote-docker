package config

import (
	"reflect"
	"testing"
	"time"
)

// Every Config field is a session setting or is excluded with a reason, so a
// new setting cannot be forgotten and then ignored by a running session.
func TestEverySettingIsComparedOrExcluded(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	base := Config{}.SessionSettings()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if reason, excluded := notSessionSettings[f.Name]; excluded {
			if reason == "" {
				t.Errorf("Config.%s is excluded without a reason", f.Name)
			}
			continue
		}
		// Perturb one field: the map must change.
		var c Config
		v := reflect.ValueOf(&c).Elem().Field(i)
		switch v.Kind() {
		case reflect.String:
			v.SetString("x")
		case reflect.Int, reflect.Int64:
			v.SetInt(7)
		case reflect.Slice:
			v.Set(reflect.ValueOf([]string{"x"}))
		case reflect.Map:
			v.Set(reflect.ValueOf(map[string]string{"/a": "b"}))
		default:
			t.Fatalf("Config.%s has kind %s: teach this test about it, or exclude it", f.Name, v.Kind())
		}
		if reflect.DeepEqual(base, c.SessionSettings()) {
			t.Errorf("Config.%s changes nothing in SessionSettings: add it, or add it to notSessionSettings with a reason", f.Name)
		}
	}
	for name := range notSessionSettings {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("notSessionSettings names %s, which is not a Config field", name)
		}
	}
}

func TestSessionSettingsAreCanonical(t *testing.T) {
	a := Config{ConsistencyPaths: map[string]string{"/b": "read=cached", "/a": "write=back"}, Prefetch: "eager"}
	b := Config{ConsistencyPaths: map[string]string{"/a": "write=back", "/b": "read=cached"}, Prefetch: "eager", Watch: "off"}
	if !reflect.DeepEqual(a.SessionSettings(), b.SessionSettings()) {
		t.Errorf("map order or an explicit default changed the settings:\n%v\n%v", a.SessionSettings(), b.SessionSettings())
	}
	if got := a.SessionSettings()["consistency-paths"]; got != "/a=write=back;/b=read=cached" {
		t.Errorf("consistency-paths = %q", got)
	}
}

// A value is compared by what it means: a spelling, an alias or the order of
// words must not restart a session or warn on every command.
func TestSessionSettingsCompareMeaningNotSpelling(t *testing.T) {
	same := func(name string, a, b Config) {
		t.Helper()
		if x, y := a.SessionSettings()[name], b.SessionSettings()[name]; x != y {
			t.Errorf("%s: %q != %q", name, x, y)
		}
	}
	same("watch", Config{Watch: "on"}, Config{Watch: "partial"})
	same("watch", Config{Watch: "best-effort"}, Config{Watch: "coarse"})
	same("watch", Config{Watch: "FALSE"}, Config{})
	same("consistency", Config{Consistency: "cached"}, Config{Consistency: "read=cached,write=through"})
	same("consistency", Config{Consistency: "write=back,read=cached"}, Config{Consistency: "read=cached,write=back"})
	same("consistency", Config{Consistency: "delegated"}, Config{Consistency: "read=cached, write=back"})
	same("consistency", Config{Consistency: "consistent"}, Config{})
	same("consistency-paths",
		Config{ConsistencyPaths: map[string]string{"/a": "write=back,read=cached"}},
		Config{ConsistencyPaths: map[string]string{"/a": "read=cached,write=back"}})
	if got := (Config{Watch: "on"}).SessionSettings()["watch"]; got != "partial" {
		t.Errorf("watch on = %q, want partial", got)
	}
	// A value that does not parse is kept as written, so it still differs.
	if got := (Config{Watch: "sometimes"}).SessionSettings()["watch"]; got != "sometimes" {
		t.Errorf("an unparseable watch = %q", got)
	}
}

// A setting is compared only if an environment variable can name it, so every
// one has a variable or is declared file-only.
func TestEverySettingHasAVariableOrIsFileOnly(t *testing.T) {
	fileOnly := map[string]bool{"consistency-paths": true}
	for name := range (Config{}).SessionSettings() {
		if _, ok := settingEnv[name]; !ok && !fileOnly[name] {
			t.Errorf("setting %q has no environment variable in settingEnv; add it, or list it as file-only", name)
		}
	}
	for name := range settingEnv {
		if _, ok := (Config{}).SessionSettings()[name]; !ok {
			t.Errorf("settingEnv names %q, which is not a session setting", name)
		}
	}
}

func TestSetInEnvironment(t *testing.T) {
	t.Setenv(EnvPrefetch, "")
	if SetInEnvironment("prefetch") {
		t.Error("an empty variable is no opinion")
	}
	t.Setenv(EnvPrefetch, "eager")
	if !SetInEnvironment("prefetch") || SetInEnvironment("consistency-paths") {
		t.Error("prefetch is named by its variable, consistency-paths by none")
	}
}

func TestDiffSettings(t *testing.T) {
	running := Config{}.SessionSettings()
	ours := Config{Prefetch: "eager", Watch: "partial", DaemonIdle: time.Minute}.SessionSettings()
	all := func(string) bool { return true }
	got := DiffSettings(running, ours, all)
	want := []SettingChange{
		{"daemon-idle", "0s", "1m0s"},
		{"prefetch", "off", "eager"},
		{"watch", "off", "partial"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if d := DiffSettings(map[string]string{"prefetch": "off"}, ours, all); len(d) != 1 || d[0].Name != "prefetch" {
		t.Errorf("a setting the session does not report must not be compared: %v", d)
	}
	only := func(n string) bool { return n == "watch" }
	if d := DiffSettings(running, ours, only); len(d) != 1 || d[0].Name != "watch" {
		t.Errorf("only a named setting is compared: %v", d)
	}
	if d := DiffSettings(running, running, all); len(d) != 0 {
		t.Errorf("equal settings differ: %v", d)
	}
}

func TestNonDefaultSettings(t *testing.T) {
	got := NonDefaultSettings(Config{Prefetch: "eager", ShareOwner: "1000"}.SessionSettings())
	want := []string{"prefetch=eager", "share-owner=1000:1000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// One owner, however it is spelled, is one setting: a command asking for
// 1000 must not restart a session that runs 1000:1000.
func TestShareOwnerSpellingsAgree(t *testing.T) {
	want := Config{ShareOwner: "1000:1000"}.SessionSettings()["share-owner"]
	for _, in := range []string{"1000", "1000:1000", "1000.1000"} {
		if got := (Config{ShareOwner: in}).SessionSettings()["share-owner"]; got != want {
			t.Errorf("owner %q = %q, want %q", in, got, want)
		}
	}
	if got := (Config{ShareUID: 1000, ShareGID: 1000}).SessionSettings()["share-owner"]; got != want {
		t.Errorf("parsed owner = %q, want %q", got, want)
	}
	if got := (Config{}).SessionSettings()["share-owner"]; got != "0:0" {
		t.Errorf("default owner = %q, want 0:0", got)
	}
}
