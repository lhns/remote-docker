package session

import (
	"github.com/lhns/remote-docker/core/notify"
	"github.com/lhns/remote-docker/dircache"
)

// cacheObserver hands the watcher's changes to the cache.
//
// An adapter rather than the cache implementing fswatch.Observer directly,
// because dircache depends on nothing at all and therefore cannot name
// core/notify (ADR 0021). This is the whole cost of that, in one direction:
// two field-for-field copies.
//
// Which ops mean a path is GONE is decided by dircache.Op.Gone rather than
// here, so the rule that a rename is a removal of the old name stays beside
// the cache that depends on it.
type cacheObserver struct{ cache *dircache.Cache }

func (o cacheObserver) Observe(event notify.Event) {
	o.cache.Observe(dircache.Event{
		Share: event.Export,
		Path:  event.Path,
		Op:    cacheOp(event.Op),
		Dir:   event.Dir,
	})
}

// cacheOp maps one bitset to the other, bit by bit.
//
// Deliberately not dircache.Op(op), which is a numeric conversion that would
// keep compiling and start lying the moment either side reordered a constant.
// The two sets are independent by design and nothing but this function knows
// they correspond; TestCacheOpMapsEveryNotifyOp pins it.
func cacheOp(op notify.Op) dircache.Op {
	var out dircache.Op
	for _, pair := range []struct {
		from notify.Op
		to   dircache.Op
	}{
		{notify.OpCreate, dircache.OpCreate},
		{notify.OpWrite, dircache.OpWrite},
		{notify.OpRemove, dircache.OpRemove},
		{notify.OpRename, dircache.OpRename},
		{notify.OpAttrib, dircache.OpAttrib},
	} {
		if op&pair.from != 0 {
			out |= pair.to
		}
	}
	return out
}

func (o cacheObserver) Lost(notice notify.Notice) {
	o.cache.Lost(dircache.Notice{Reason: notice.Reason, Dropped: notice.Dropped})
}
