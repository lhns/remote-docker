package main

import (
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
)

func TestSettingsMessages(t *testing.T) {
	changes := config.DiffSettings(
		config.Config{}.SessionSettings(),
		config.Config{Prefetch: "eager", Watch: "partial", ShareOwner: "1000"}.SessionSettings(),
		func(string) bool { return true },
	)
	if got, want := describeChanges(changes), "prefetch off -> eager, share-owner unset -> 1000, watch off -> partial"; got != want {
		t.Errorf("describeChanges = %q, want %q", got, want)
	}

	var b strings.Builder
	warnSettingsMismatch(&b, changes[:1])
	for _, want := range []string{
		"the running session has prefetch=off; prefetch=eager here is ignored while it is in use.",
		"\n  fix: `",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("warning lacks %q:\n%s", want, b.String())
		}
	}
}
