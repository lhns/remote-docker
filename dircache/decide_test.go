package dircache

import (
	"io/fs"
	"testing"
	"time"
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

func localFrom(files map[string]stub) localAt {
	return func(p string) (fs.FileInfo, bool) {
		f, ok := files[p]
		return f, ok
	}
}

func kindOf(actions []action, path string) (kind, bool) {
	for _, a := range actions {
		if a.Path == path {
			return a.kind, true
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

	changes := []Change{
		{Path: "/theirs.go", Size: 20, ModTime: laterHere.UnixNano()},
		{Path: "/both.go", Size: 20, ModTime: laterYet.UnixNano()},
		{Path: "/deleted.go", Deleted: true},
		{Path: "/deleted-but-yours.go", Deleted: true},
		{Path: "/created.go", Size: 5, ModTime: laterHere.UnixNano()},
	}

	actions := decide(manifest, nil, changes, local, nil, 0, 0)

	for path, want := range map[string]kind{
		"/theirs.go":            kindWrite,
		"/both.go":              kindConflict,
		"/deleted.go":           kindDelete,
		"/deleted-but-yours.go": kindConflict,
		"/created.go":           kindWrite,
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

// A file the fill sent, changed in the container and deleted here. decide read
// the missing local file's mtime and panicked, in a write-back goroutine that
// nothing recovers, which took the client down. The changed side wins, as it
// does when the container deleted and you changed.
func TestDecideDeletedHereChangedThere(t *testing.T) {
	manifest := map[string]Baseline{"/gone.go": {Size: 10, ModTime: sentAt}}
	missing := func(string) (fs.FileInfo, bool) { return nil, false }
	changes := []Change{{Path: "/gone.go", Size: 20, ModTime: laterHere.UnixNano()}}

	actions := decide(manifest, nil, changes, missing, nil, 0, 0)

	if len(actions) != 1 || actions[0].kind != kindConflict || !actions[0].Wins {
		t.Errorf("decided %+v, want one conflict the container's version wins", actions)
	}
}

// A file the container made that this machine never had comes back, whatever
// the fill sent: decide no longer asks whether the cache is complete (ADR 0044).
// One the record says was deleted here does not.
func TestDecideCarriesANewFileWithNothingSent(t *testing.T) {
	changes := []Change{
		{Path: "/made-there.go", Size: 1, ModTime: sentAt.UnixNano()},
		{Path: "/deleted-here.go", Size: 1, ModTime: sentAt.UnixNano()},
	}
	got := decide(nil, map[string]bool{"/deleted-here.go": true}, changes, localFrom(nil), nil, 0, 0)

	if k, ok := kindOf(got, "/made-there.go"); !ok || k != kindWrite {
		t.Errorf("/made-there.go = %v %v, want a write", k, ok)
	}
	if _, ok := kindOf(got, "/deleted-here.go"); ok {
		t.Errorf("a file deleted here came back: %+v", got)
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
	changes := []Change{
		{Path: "/both.go", Size: 20, ModTime: laterHere.Add(skew).UnixNano()},
	}

	uncorrected := decide(manifest, nil, changes, local, nil, 0, 0)
	if len(uncorrected) != 1 || !uncorrected[0].Wins {
		t.Fatalf("without the offset the container should look newer: %+v", uncorrected)
	}

	corrected := decide(manifest, nil, changes, local, nil, skew, 0)
	if len(corrected) != 1 || corrected[0].Wins {
		t.Errorf("with the offset applied this machine wrote last: %+v", corrected)
	}
	if corrected[0].Why == "" {
		t.Error("a conflict resolved without saying why")
	}
}

// ctimesFrom is this machine's change times, by path.
func ctimesFrom(times map[string]time.Time) ctimeAt {
	return func(p string) (time.Time, bool) {
		t, ok := times[p]
		return t, ok
	}
}

// A file the cache never held is decided as a bind mount would have it: the
// side that changed it last wins, by the container's change time against this
// machine's, and the side that lost is said. Unknown times never write or
// delete (ADR 0044).
func TestDecideAFileTheCacheNeverHeldByWhoWroteLast(t *testing.T) {
	local := localFrom(map[string]stub{
		"/theirs-later.go":   {size: 7, mod: sentAt},
		"/yours-later.go":    {size: 7, mod: sentAt},
		"/deleted-there.go":  {size: 7, mod: sentAt},
		"/deleted-mine.go":   {size: 7, mod: sentAt},
		"/deleted-old-ws.go": {size: 7, mod: sentAt},
		"/old-ws-changed.go": {size: 7, mod: sentAt},
		"/no-ctime.go":       {size: 7, mod: sentAt},
	})
	ctimes := ctimesFrom(map[string]time.Time{
		"/theirs-later.go":   sentAt,
		"/yours-later.go":    laterYet,
		"/deleted-there.go":  sentAt,
		"/deleted-mine.go":   laterYet,
		"/deleted-old-ws.go": sentAt,
		"/old-ws-changed.go": sentAt,
	})
	changes := []Change{
		{Path: "/theirs-later.go", Size: 20, ModTime: sentAt.UnixNano(), Changed: laterHere.UnixNano()},
		{Path: "/yours-later.go", Size: 20, ModTime: laterYet.Add(time.Hour).UnixNano(), Changed: laterHere.UnixNano()},
		{Path: "/deleted-there.go", Deleted: true, Changed: laterHere.UnixNano()},
		{Path: "/deleted-mine.go", Deleted: true, Changed: laterHere.UnixNano()},
		// An older workspace sends no change time: a changed file goes by its
		// modification time, and a deletion is kept.
		{Path: "/deleted-old-ws.go", Deleted: true},
		{Path: "/old-ws-changed.go", Size: 20, ModTime: laterHere.UnixNano()},
		{Path: "/no-ctime.go", Size: 20, ModTime: laterHere.UnixNano(), Changed: laterHere.UnixNano()},
	}

	actions := decide(map[string]Baseline{}, nil, changes, local, ctimes, 0, 0)
	for path, want := range map[string]struct {
		kind kind
		wins bool
	}{
		"/theirs-later.go":   {kindWrite, false},
		"/yours-later.go":    {kindConflict, false},
		"/deleted-there.go":  {kindDelete, false},
		"/deleted-mine.go":   {kindConflict, false},
		"/deleted-old-ws.go": {kindConflict, false},
		"/old-ws-changed.go": {kindWrite, false},
		"/no-ctime.go":       {kindConflict, false},
	} {
		var got *action
		for i := range actions {
			if actions[i].Path == path {
				got = &actions[i]
			}
		}
		if got == nil {
			t.Errorf("%s produced no action", path)
			continue
		}
		if got.kind != want.kind || got.Wins != want.wins {
			t.Errorf("%s = %v wins=%v, want %v wins=%v", path, got.kind, got.Wins, want.kind, want.wins)
		}
		if got.kind == kindConflict && got.Why == "" {
			t.Errorf("%s was kept without saying why", path)
		}
	}
}

// A copy is not a change, and a mode is carried only by its execute bit: the
// share reports every other bit wide (ADR 0046), so a copy-up's 0666 against a
// 0644 here is not a chmod, and on Windows (exec 0) nothing is compared.
func TestDecideACopyAndTheExecuteBit(t *testing.T) {
	type file struct {
		mode  fs.FileMode
		ctime time.Time
	}
	here := map[string]file{
		"/copy.go":       {0o644, sentAt},
		"/chmodded.sh":   {0o644, sentAt},
		"/unchmodded.sh": {0o755, sentAt},
		"/yours.sh":      {0o644, laterYet},
	}
	local := func(p string) (fs.FileInfo, bool) {
		f, ok := here[p]
		return modeStub{stub{size: 7, mod: sentAt}, f.mode}, ok
	}
	ctimes := func(p string) (time.Time, bool) { return here[p].ctime, true }
	at := laterHere.UnixNano()
	changes := []Change{
		{Path: "/copy.go", Size: 7, ModTime: sentAt.UnixNano(), Changed: at, Mode: 0o666},
		{Path: "/chmodded.sh", Size: 7, ModTime: sentAt.UnixNano(), Changed: at, Mode: 0o777},
		{Path: "/unchmodded.sh", Size: 7, ModTime: sentAt.UnixNano(), Changed: at, Mode: 0o666},
		{Path: "/yours.sh", Size: 7, ModTime: sentAt.UnixNano(), Changed: at, Mode: 0o777},
	}

	actions := decide(map[string]Baseline{}, nil, changes, local, ctimes, 0, 0o111)
	byPath := map[string]action{}
	for _, a := range actions {
		byPath[a.Path] = a
	}
	if a := byPath["/copy.go"]; a.kind != kindAdopt {
		t.Errorf("a copy decided %+v, want it adopted and nothing changed", a)
	}
	if a := byPath["/chmodded.sh"]; a.kind != kindChmod || a.Exec != 1 {
		t.Errorf("chmod +x decided %+v, want the bit set", a)
	}
	if a := byPath["/unchmodded.sh"]; a.kind != kindChmod || a.Exec != -1 {
		t.Errorf("chmod -x decided %+v, want the bit cleared", a)
	}
	if a := byPath["/yours.sh"]; a.kind != kindConflict || a.Wins {
		t.Errorf("a chmod older than this machine's change decided %+v, want yours kept", a)
	}

	windows := decide(map[string]Baseline{}, nil, changes, local, ctimes, 0, 0)
	for _, a := range windows {
		if a.kind != kindAdopt {
			t.Errorf("with no execute bit to carry, %s decided %v, want only copies", a.Path, a.kind)
		}
	}
}

// modeStub is a stub with a mode.
type modeStub struct {
	stub
	mode fs.FileMode
}

func (m modeStub) Mode() fs.FileMode { return m.mode }

// What the caller has to fetch, remove and report.
func TestPartitionsForTheCaller(t *testing.T) {
	actions := []action{
		{Path: "/a", kind: kindWrite},
		{Path: "/b", kind: kindDelete},
		{Path: "/c", kind: kindConflict, Wins: true},
		{Path: "/d", kind: kindConflict, Wins: false},
	}

	// A conflict the container won is fetched like any other write; one it lost
	// is only reported.
	if got := writes(actions); len(got) != 2 || got[0] != "/a" || got[1] != "/c" {
		t.Errorf("writes = %v", got)
	}
	if got := deletes(actions); len(got) != 1 || got[0] != "/b" {
		t.Errorf("deletes = %v", got)
	}
	if got := ofKind(actions, kindConflict); len(got) != 2 {
		t.Errorf("conflicts = %v, want both reported whichever way they resolved", got)
	}
}

// The cache is filled THROUGH the union, so the fill's own copy of every file
// is in the layer that records what the container changed. Left unfiltered,
// every round asks the workspace for the whole tree back -- which is not a
// small waste but a stream large enough to be refused, so nothing is ever
// written back at all.
func TestDecideIgnoresWhatTheFillItselfWrote(t *testing.T) {
	manifest := map[string]Baseline{
		"/filled.go":  {Size: 10, ModTime: sentAt},
		"/written.go": {Size: 10, ModTime: sentAt},
	}
	local := localFrom(map[string]stub{
		"/filled.go":  {size: 10, mod: sentAt},
		"/written.go": {size: 10, mod: sentAt},
	})

	// Both are in the cache layer. Only the second differs from what the fill
	// put there.
	changes := []Change{
		{Path: "/filled.go", Size: 10, ModTime: sentAt.UnixNano()},
		{Path: "/written.go", Size: 20, ModTime: laterHere.UnixNano()},
	}

	actions := decide(manifest, nil, changes, local, nil, 0, 0)

	if _, ok := kindOf(actions, "/filled.go"); ok {
		t.Error("the fill's own copy was decided to be a container write")
	}
	if got, ok := kindOf(actions, "/written.go"); !ok || got != kindWrite {
		t.Errorf("/written.go = %v %v, want a write", got, ok)
	}
}

// Same size, different content, and the container's write lands within the
// same timestamp: the filter must key on the pair rather than on either half.
func TestDecideCarriesBackASameSizedRewrite(t *testing.T) {
	manifest := map[string]Baseline{"/same-size.go": {Size: 10, ModTime: sentAt}}
	local := localFrom(map[string]stub{"/same-size.go": {size: 10, mod: sentAt}})

	changes := []Change{
		{Path: "/same-size.go", Size: 10, ModTime: laterHere.UnixNano()},
	}

	if got, ok := kindOf(decide(manifest, nil, changes, local, nil, 0, 0), "/same-size.go"); !ok || got != kindWrite {
		t.Errorf("/same-size.go = %v %v, want a write", got, ok)
	}
}
