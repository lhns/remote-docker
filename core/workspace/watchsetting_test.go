package workspace

import "testing"

func TestParseWatchSettingAcceptsTheAliases(t *testing.T) {
	for in, want := range map[string]Watch{
		"": WatchOff, "off": WatchOff, "OFF": WatchOff, "false": WatchOff, "0": WatchOff, "no": WatchOff,
		"partial": WatchPartial, "on": WatchPartial, "true": WatchPartial, "1": WatchPartial, "yes": WatchPartial, " partial ": WatchPartial,
		"coarse": WatchCoarse, "best-effort": WatchCoarse,
	} {
		got, err := ParseWatchSetting(in)
		if err != nil || got != want {
			t.Errorf("ParseWatchSetting(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseWatchSetting("sometimes"); err == nil {
		t.Error("ParseWatchSetting accepted an unknown mode")
	}
	// A mount's word is canonical only.
	if _, err := ParseWatch("on"); err == nil {
		t.Error("ParseWatch accepted an alias")
	}
}

// What `status` prints can be given back as configuration.
func TestWatchStringRoundTrips(t *testing.T) {
	for w, want := range map[Watch]string{WatchUnset: "off", WatchOff: "off", WatchPartial: "partial", WatchCoarse: "coarse"} {
		if got := w.String(); got != want {
			t.Errorf("Watch(%q).String() = %q, want %q", string(w), got, want)
		}
		back, err := ParseWatchSetting(want)
		if err != nil || back.String() != want {
			t.Errorf("ParseWatchSetting(%q) = %q, %v", want, back, err)
		}
	}
}
