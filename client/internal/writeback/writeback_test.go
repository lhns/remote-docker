package writeback

import (
	"io/fs"
	"testing"
	"time"

	"github.com/lhns/remote-docker/core/workspace"
)

var (
	sentAt    = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	laterHere = sentAt.Add(time.Minute)
	laterYet  = sentAt.Add(2 * time.Minute)
)

// stub is a local file as this machine currently has it.
type stub struct {
	size int64
	mod  time.Time
}

func (s stub) Name() string       { return "" }
func (s stub) Size() int64        { return s.size }
func (s stub) Mode() fs.FileMode  { return 0o644 }
func (s stub) ModTime() time.Time { return s.mod }
func (s stub) IsDir() bool        { return false }
func (s stub) Sys() any           { return nil }

func localFrom(files map[string]stub) Local {
	return func(p string) (fs.FileInfo, bool) {
		f, ok := files[p]
		return f, ok
	}
}

func kindOf(actions []Action, path string) (Kind, bool) {
	for _, a := range actions {
		if a.Path == path {
			return a.Kind, true
		}
	}
	return 0, false
}

// The table this whole mode's safety rests on. Each side is compared against
// its own baseline, so only one case needs a clock at all.
func TestDecide(t *testing.T) {
	manifest := map[string]Baseline{
		"/untouched.go":         {Size: 10, ModTime: sentAt},
		"/yours.go":             {Size: 10, ModTime: sentAt},
		"/theirs.go":            {Size: 10, ModTime: sentAt},
		"/both.go":              {Size: 10, ModTime: sentAt},
		"/deleted.go":           {Size: 10, ModTime: sentAt},
		"/deleted-but-yours.go": {Size: 10, ModTime: sentAt},
	}
	local := localFrom(map[string]stub{
		"/untouched.go":         {size: 10, mod: sentAt},
		"/yours.go":             {size: 99, mod: laterHere},
		"/theirs.go":            {size: 10, mod: sentAt},
		"/both.go":              {size: 99, mod: laterHere},
		"/deleted.go":           {size: 10, mod: sentAt},
		"/deleted-but-yours.go": {size: 99, mod: laterHere},
	})

	changes := []workspace.CacheChange{
		{Path: "/theirs.go", Size: 20, ModTime: laterHere.UnixNano()},
		{Path: "/both.go", Size: 20, ModTime: laterYet.UnixNano()},
		{Path: "/deleted.go", Deleted: true},
		{Path: "/deleted-but-yours.go", Deleted: true},
		{Path: "/created.go", Size: 5, ModTime: laterHere.UnixNano()},
	}

	actions := Decide(manifest, changes, local, 0, true)

	for path, want := range map[string]Kind{
		"/theirs.go":            Write,
		"/both.go":              Conflict,
		"/deleted.go":           Delete,
		"/deleted-but-yours.go": Conflict,
		"/created.go":           Write,
	} {
		got, ok := kindOf(actions, path)
		if !ok {
			t.Errorf("%s produced no action", path)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}

	// A file only YOU changed comes back with nothing: the container never
	// touched it, so there is nothing to write, and reporting it as an action
	// would be noise.
	if _, ok := kindOf(actions, "/yours.go"); ok {
		t.Error("a file only this machine changed produced an action")
	}
	if _, ok := kindOf(actions, "/untouched.go"); ok {
		t.Error("a file nobody changed produced an action")
	}
}

// Nothing is written back from a cache that is not complete. A file the fill
// never sent looks exactly like one the container created, and the cost of
// that mistake is content appearing in somebody's source tree that they never
// wrote.
func TestDecideRefusesAnIncompleteCache(t *testing.T) {
	changes := []workspace.CacheChange{{Path: "/anything.go", Size: 1, ModTime: sentAt.UnixNano()}}

	if got := Decide(nil, changes, localFrom(nil), 0, false); len(got) != 0 {
		t.Errorf("decided %v on an incomplete cache, want nothing", got)
	}
}

// The one place a clock is used, and the offset between the two machines is
// applied rather than assumed away.
func TestDecideResolvesAConflictWithTheMeasuredSkew(t *testing.T) {
	manifest := map[string]Baseline{"/both.go": {Size: 10, ModTime: sentAt}}
	local := localFrom(map[string]stub{"/both.go": {size: 99, mod: laterYet}})

	// The workspace's clock runs an hour ahead. Its file was written a minute
	// BEFORE the local one, and only the correction reveals that.
	const skew = time.Hour
	changes := []workspace.CacheChange{
		{Path: "/both.go", Size: 20, ModTime: laterHere.Add(skew).UnixNano()},
	}

	uncorrected := Decide(manifest, changes, local, 0, true)
	if len(uncorrected) != 1 || !uncorrected[0].Wins {
		t.Fatalf("without the offset the container should look newer: %+v", uncorrected)
	}

	corrected := Decide(manifest, changes, local, skew, true)
	if len(corrected) != 1 || corrected[0].Wins {
		t.Errorf("with the offset applied this machine wrote last: %+v", corrected)
	}
	if corrected[0].Why == "" {
		t.Error("a conflict resolved without saying why")
	}
}

// A path the fill never sent cannot be told from one the container created, so
// a file that exists here is left alone rather than overwritten by a guess.
func TestDecideLeavesUnsentPathsAlone(t *testing.T) {
	local := localFrom(map[string]stub{"/mine.go": {size: 7, mod: laterHere}})
	changes := []workspace.CacheChange{
		{Path: "/mine.go", Size: 20, ModTime: laterYet.UnixNano()},
		{Path: "/gone.go", Deleted: true},
	}

	if got := Decide(map[string]Baseline{}, changes, local, 0, true); len(got) != 0 {
		t.Errorf("decided %v, want nothing for paths the fill never sent", got)
	}
}

// What the caller has to fetch, remove and report.
func TestPartitionsForTheCaller(t *testing.T) {
	actions := []Action{
		{Path: "/a", Kind: Write},
		{Path: "/b", Kind: Delete},
		{Path: "/c", Kind: Conflict, Wins: true},
		{Path: "/d", Kind: Conflict, Wins: false},
	}

	// A conflict the container won is fetched like any other write; one it lost
	// is only reported.
	if got := Writes(actions); len(got) != 2 || got[0] != "/a" || got[1] != "/c" {
		t.Errorf("Writes = %v", got)
	}
	if got := Deletes(actions); len(got) != 1 || got[0] != "/b" {
		t.Errorf("Deletes = %v", got)
	}
	if got := Conflicts(actions); len(got) != 2 {
		t.Errorf("Conflicts = %v, want both reported whichever way they resolved", got)
	}
}
