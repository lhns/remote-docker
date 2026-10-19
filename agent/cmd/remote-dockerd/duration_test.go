package main

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"7d":    7 * 24 * time.Hour,
		"36h":   36 * time.Hour,
		"0":     0,
		"0d":    0,
		"90s":   90 * time.Second,
		" 2m ":  2 * time.Minute,
		"1h30m": 90 * time.Minute,
	} {
		got, err := parseDuration(in)
		if err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "garbage", "1.5d", "-1d", "-2h", "7 d", "99999d"} {
		if got, err := parseDuration(in); err == nil {
			t.Errorf("parseDuration(%q) = %v, want an error", in, got)
		}
	}
}

func TestEphemeralGraceTakesDays(t *testing.T) {
	_, grace, err := ephemeralLimits("", "1d")
	if err != nil || grace != 24*time.Hour {
		t.Errorf("a grace of 1d = %v, %v", grace, err)
	}
	for _, bad := range []string{"0", "0d", "-1d", "soon"} {
		if _, _, err := ephemeralLimits("", bad); err == nil {
			t.Errorf("a grace of %q was accepted", bad)
		}
	}
}
