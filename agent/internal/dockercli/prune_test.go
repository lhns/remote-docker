package dockercli

import (
	"slices"
	"testing"
	"time"
)

func TestParseImages(t *testing.T) {
	out := "sha256:aa\t[\"alpine:3\",\"alpine:latest\"]\t[\"alpine@sha256:ff\"]\t7000000\n" +
		"sha256:bb\t[]\t[]\t5\n"
	got, err := parseImages(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !slices.Equal(got[0].Tags, []string{"alpine:3", "alpine:latest"}) ||
		!slices.Equal(got[0].Digests, []string{"alpine@sha256:ff"}) || got[0].Size != 7000000 {
		t.Errorf("first image = %+v", got[0])
	}
	if got[1].ID != "sha256:bb" || len(got[1].Tags) != 0 || !slices.Equal(got[1].Refs(), []string{"sha256:bb"}) {
		t.Errorf("an untagged image = %+v", got[1])
	}
	if _, err := parseImages("one field only"); err == nil {
		t.Error("a malformed line was accepted")
	}
}

func TestParseContainers(t *testing.T) {
	out := "abc\t/job\tsha256:aa\texited\t2026-10-01T10:00:00.123456789Z\t2026-10-01T10:05:00Z\n" +
		"def\t/new\tsha256:bb\tcreated\t2026-10-02T10:00:00Z\t0001-01-01T00:00:00Z\n"
	got, err := parseContainers(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ImageID != "sha256:aa" || got[0].Status != "exited" ||
		got[0].Finished.Sub(got[0].Created) != 5*time.Minute-123456789*time.Nanosecond {
		t.Errorf("first container = %+v", got[0])
	}
	if !got[1].Since().Equal(got[1].Created) {
		t.Errorf("a container that never ran is stopped since %v, want its creation", got[1].Since())
	}
}
