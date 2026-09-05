package session

import (
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/notify"
	"github.com/lhns/remote-docker/dircache"
)

// Every op the watcher can report must reach the cache as something.
//
// The two bitsets are independent by design: dircache depends on nothing, so
// nothing but cacheOp knows they correspond, and a numeric conversion between
// them would keep compiling while meaning the wrong thing. This walks every
// bit and uses notify's own String to tell a DEFINED op from an undefined one,
// so adding a constant to notify without teaching cacheOp about it fails here
// rather than silently dropping that op on the floor.
func TestCacheOpMapsEveryNotifyOp(t *testing.T) {
	for bit := 0; bit < 8; bit++ {
		op := notify.Op(1 << bit)
		name := op.String()
		if strings.HasPrefix(name, "unknown") {
			// Not an op notify defines, so having no mapping is correct.
			if got := cacheOp(op); got != 0 {
				t.Errorf("%s is not a defined op but maps to %#x", name, got)
			}
			continue
		}
		if got := cacheOp(op); got == 0 {
			t.Errorf("notify.Op %s reaches the cache as nothing; cacheOp needs a row for it", name)
		}
	}
}

// The mapping has to agree about names as well as about coverage: a rename
// arriving as a write would leave a deleted file cached, which is the one way
// this mode can be wrong rather than slow (ADR 0044).
func TestCacheOpKeepsTheMeaningOfEachBit(t *testing.T) {
	for _, tc := range []struct {
		from notify.Op
		want dircache.Op
	}{
		{notify.OpCreate, dircache.OpCreate},
		{notify.OpWrite, dircache.OpWrite},
		{notify.OpRemove, dircache.OpRemove},
		{notify.OpRename, dircache.OpRename},
		{notify.OpAttrib, dircache.OpAttrib},
	} {
		if got := cacheOp(tc.from); got != tc.want {
			t.Errorf("cacheOp(%s) = %#x, want %#x", tc.from, got, tc.want)
		}
	}

	// A merged set survives whole, which is the ordinary case: an editor
	// saving in place reports create and write together.
	both := cacheOp(notify.OpCreate | notify.OpWrite)
	if both != dircache.OpCreate|dircache.OpWrite {
		t.Errorf("a merged op lost a bit: got %#x", both)
	}
}

// What the cache actually asks of an op is whether the path is gone. That
// question has to survive the boundary, or invalidation stops removing files.
func TestGoneSurvivesTheBoundary(t *testing.T) {
	for _, tc := range []struct {
		op   notify.Op
		gone bool
	}{
		{notify.OpCreate, false},
		{notify.OpWrite, false},
		{notify.OpAttrib, false},
		{notify.OpRemove, true},
		{notify.OpRename, true},
		{notify.OpWrite | notify.OpRemove, true},
	} {
		if got := cacheOp(tc.op).Gone(); got != tc.gone {
			t.Errorf("cacheOp(%s).Gone() = %v, want %v", tc.op, got, tc.gone)
		}
	}
}
