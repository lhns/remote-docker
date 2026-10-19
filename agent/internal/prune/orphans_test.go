package prune

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestOrphans(t *testing.T) {
	found := []Footprint{
		{Account: "bob", Container: true, Volume: true},
		{Account: "alice", Container: true, Running: true, Volume: true},
		{Account: "carol", Volume: true},
		{Account: "twow", Container: true, Volume: true},
		{Account: ""},
	}
	enrolled := map[string]bool{"alice": true}
	rec := &OrphanRecord{}
	grace := 14 * 24 * time.Hour

	got := Orphans(rec, found, enrolled, grace, t0)
	var names []string
	for _, o := range got {
		names = append(names, o.Account)
		if o.Due {
			t.Errorf("%s is due at once", o.Account)
		}
	}
	if want := []string{"bob", "carol", "twow"}; !slices.Equal(names, want) {
		t.Fatalf("orphans = %v, want %v (enrolled and nameless are not)", names, want)
	}

	// Inside the grace, nothing is due; at it, a container is, a lone volume is not.
	for _, o := range Orphans(rec, found, enrolled, grace, t0.Add(grace-time.Second)) {
		if o.Due {
			t.Errorf("%s is due before the grace is over", o.Account)
		}
	}
	due := map[string]bool{}
	for _, o := range Orphans(rec, found, enrolled, grace, t0.Add(grace)) {
		due[o.Account] = o.Due
		if !o.Since.Equal(t0) {
			t.Errorf("%s since %v, want the first sighting %v", o.Account, o.Since, t0)
		}
	}
	if !due["bob"] || !due["twow"] || due["carol"] {
		t.Errorf("due = %v, want bob and twow but not the volume alone", due)
	}
}

func TestOrphansReturnStartsOver(t *testing.T) {
	found := []Footprint{{Account: "bob", Container: true}}
	rec := &OrphanRecord{}
	grace := time.Hour
	Orphans(rec, found, map[string]bool{}, grace, t0)

	// Enrolled again: forgotten. Gone again later: the grace starts then.
	if got := Orphans(rec, found, map[string]bool{"bob": true}, grace, t0.Add(30*time.Minute)); len(got) != 0 || len(rec.Since) != 0 {
		t.Fatalf("an enrolled account is an orphan: %v %v", got, rec.Since)
	}
	got := Orphans(rec, found, map[string]bool{}, grace, t0.Add(2*time.Hour))
	if len(got) != 1 || got[0].Due || !got[0].Since.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("a returning orphan = %+v, want a fresh grace", got)
	}

	// Gone altogether: the record forgets it.
	Orphans(rec, nil, map[string]bool{}, grace, t0.Add(3*time.Hour))
	if len(rec.Since) != 0 {
		t.Errorf("record still holds %v", rec.Since)
	}
}

func TestOrphanRecordFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prune", "_orphans.json")
	rec, err := LoadOrphans(path)
	if err != nil || len(rec.Since) != 0 {
		t.Fatalf("missing file = %v, %v", rec, err)
	}
	rec.Since = map[string]time.Time{"bob": t0}
	if err := rec.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadOrphans(path)
	if err != nil || !back.Since["bob"].Equal(t0) {
		t.Errorf("round trip = %v, %v", back, err)
	}
	if err := writeAtomic(path, []byte("{")); err != nil {
		t.Fatal(err)
	}
	if back, err := LoadOrphans(path); err == nil || len(back.Since) != 0 {
		t.Errorf("a corrupt record = %v, %v; want empty and an error", back, err)
	}
}
