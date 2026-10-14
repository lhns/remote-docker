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

func TestDiffSettings(t *testing.T) {
	running := Config{}.SessionSettings()
	ours := Config{Prefetch: "eager", Watch: "partial", DaemonIdle: time.Minute}.SessionSettings()
	got := DiffSettings(running, ours)
	want := []SettingChange{
		{"daemon-idle", "0s", "1m0s"},
		{"prefetch", "off", "eager"},
		{"watch", "off", "partial"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if d := DiffSettings(map[string]string{"prefetch": "off"}, ours); len(d) != 1 || d[0].Name != "prefetch" {
		t.Errorf("a setting the session does not report must not be compared: %v", d)
	}
	if d := DiffSettings(running, running); len(d) != 0 {
		t.Errorf("equal settings differ: %v", d)
	}
}

func TestNonDefaultSettings(t *testing.T) {
	got := NonDefaultSettings(Config{Prefetch: "eager", ShareOwner: "1000"}.SessionSettings())
	want := []string{"prefetch=eager", "share-owner=1000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
