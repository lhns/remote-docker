package prune

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func ids(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Image.ID)
	}
	return out
}

func images() []Image {
	return []Image{
		{ID: "sha256:aaaa1111", Tags: []string{"alpine:3", "alpine:latest"}},
		{ID: "sha256:bbbb2222", Tags: []string{"app:1"}},
		{ID: "sha256:cccc3333", Digests: []string{"reg/x@sha256:ff"}},
		{ID: "sha256:dddd4444"},
	}
}

func TestRefs(t *testing.T) {
	want := [][]string{{"alpine:3", "alpine:latest"}, {"app:1"}, {"reg/x@sha256:ff"}, {"sha256:dddd4444"}}
	for i, img := range images() {
		if got := img.Refs(); !slices.Equal(got, want[i]) {
			t.Errorf("Refs of %s = %v, want %v", img.ID, got, want[i])
		}
	}
}

func TestResolve(t *testing.T) {
	imgs := images()
	for name, want := range map[string]string{
		"alpine:3":         "sha256:aaaa1111",
		"alpine":           "sha256:aaaa1111",
		"docker.io/alpine": "sha256:aaaa1111",
		"app:1":            "sha256:bbbb2222",
		"reg/x@sha256:ff":  "sha256:cccc3333",
		"sha256:dddd4444":  "sha256:dddd4444",
		"dddd4444":         "sha256:dddd4444",
		"dddd":             "sha256:dddd4444",
		"app:2":            "",
		"app":              "",
		"zz":               "",
	} {
		got, ok := Resolve(imgs, name)
		if got != want || ok != (want != "") {
			t.Errorf("Resolve(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
}

func rec(seen map[string]time.Duration) *Record {
	r := NewRecord()
	for id, ago := range seen {
		r.Seen[id] = t0.Add(-ago)
	}
	return r
}

const day = 24 * time.Hour

func TestAgeImages(t *testing.T) {
	imgs := images()
	r := rec(map[string]time.Duration{
		"sha256:aaaa1111": 8 * day, // old, unused, two tags
		"sha256:bbbb2222": 9 * day, // old but a stopped container uses it
		"sha256:cccc3333": 3 * day, // young
		// dddd4444 is unknown: met now
	})
	containers := []Container{{ID: "c1", ImageID: "sha256:bbbb2222", Status: "exited"}}

	got := ids(AgeImages(r, imgs, containers, nil, 7*day, t0))
	if want := []string{"sha256:aaaa1111"}; !slices.Equal(got, want) {
		t.Errorf("AgeImages = %v, want %v", got, want)
	}
	if refs := AgeImages(r, imgs, containers, nil, 7*day, t0)[0].Image.Refs(); len(refs) != 2 {
		t.Errorf("a multi-tagged image should yield every tag, got %v", refs)
	}

	// Protected is never a candidate, however old.
	got = ids(AgeImages(r, imgs, containers, map[string]bool{"sha256:aaaa1111": true}, 7*day, t0))
	if len(got) != 0 {
		t.Errorf("a protected image was a candidate: %v", got)
	}
}

func TestPressureImages(t *testing.T) {
	imgs := images()
	r := rec(map[string]time.Duration{
		"sha256:aaaa1111": 5 * time.Hour,
		"sha256:bbbb2222": 3 * day,
		"sha256:cccc3333": 30 * time.Minute, // within the floor
		"sha256:dddd4444": 2 * time.Hour,
	})
	got := ids(PressureImages(r, imgs, nil, nil, time.Hour, t0))
	want := []string{"sha256:bbbb2222", "sha256:aaaa1111", "sha256:dddd4444"}
	if !slices.Equal(got, want) {
		t.Errorf("PressureImages = %v, want oldest first without the floor's: %v", got, want)
	}
}

func TestAgeContainers(t *testing.T) {
	cs := []Container{
		{ID: "old", Name: "/job", Status: "exited", Finished: t0.Add(-8 * day), Created: t0.Add(-20 * day)},
		{ID: "recent", Name: "/job2", Status: "exited", Finished: t0.Add(-1 * day), Created: t0.Add(-20 * day)},
		{ID: "running", Name: "/web", Status: "running", Created: t0.Add(-20 * day)},
		{ID: "never", Name: "/n", Status: "created", Finished: time.Time{}, Created: t0.Add(-9 * day)},
		{ID: "neverfresh", Name: "/n2", Status: "created", Created: t0.Add(-1 * day)},
		{ID: "daemon", Name: "/rd-dind-alice", Status: "exited", Finished: t0.Add(-30 * day)},
	}
	var got []string
	for _, c := range AgeContainers(cs, 7*day, t0) {
		got = append(got, c.ID)
	}
	if want := []string{"old", "never"}; !slices.Equal(got, want) {
		t.Errorf("AgeContainers = %v, want %v", got, want)
	}
}

func TestObserve(t *testing.T) {
	imgs := images()
	r := rec(map[string]time.Duration{
		"sha256:aaaa1111": 8 * day,
		"sha256:bbbb2222": 8 * day,
		"sha256:cccc3333": 8 * day,
		"sha256:gone":     8 * day,
	})
	containers := []Container{{ID: "c", ImageID: "sha256:bbbb2222", Status: "exited"}}

	r.Observe(t0, imgs, containers, []string{"alpine:3", "no-such:1"})

	if got := r.Seen["sha256:bbbb2222"]; !got.Equal(t0) {
		t.Errorf("an image a container references was not refreshed: %v", got)
	}
	if got := r.Seen["sha256:aaaa1111"]; !got.Equal(t0) {
		t.Errorf("a create event did not refresh the image it named: %v", got)
	}
	if got := r.Seen["sha256:cccc3333"]; !got.Equal(t0.Add(-8 * day)) {
		t.Errorf("an unused image was refreshed: %v", got)
	}
	if got := r.Seen["sha256:dddd4444"]; !got.Equal(t0) {
		t.Errorf("a new image should be met now: %v", got)
	}
	if _, ok := r.Seen["sha256:gone"]; ok {
		t.Error("an entry for an image that no longer exists was kept")
	}
}

func TestRecordSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "alice")
	r := rec(map[string]time.Duration{"sha256:aaaa1111": 8 * day})
	r.Swept = t0
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Seen["sha256:aaaa1111"].Equal(r.Seen["sha256:aaaa1111"]) || !got.Swept.Equal(t0) {
		t.Errorf("reloaded %+v, want %+v", got, r)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("a temporary file was left behind: %v", entries)
	}
}

func TestUnusableRecordIsNow(t *testing.T) {
	dir := t.TempDir()

	missing, err := Load(filepath.Join(dir, "none"))
	if err != nil || len(missing.Seen) != 0 {
		t.Errorf("a missing record = %+v, %v; want empty and no error", missing, err)
	}

	for name, content := range map[string]string{
		"garbage": "{not json",
		"version": `{"version": 99, "seen": {"sha256:aaaa1111": "2020-01-01T00:00:00Z"}}`,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		r, err := Load(path)
		if err == nil {
			t.Errorf("%s: no error to warn about", name)
		}
		if got := r.LastSeen("sha256:aaaa1111", t0); !got.Equal(t0) {
			t.Errorf("%s: an unreadable record made an image older than now: %v", name, got)
		}
		if got := AgeImages(r, images(), nil, nil, 7*day, t0); len(got) != 0 {
			t.Errorf("%s: an unreadable record removed %v", name, ids(got))
		}
	}
}
