package session

import (
	"github.com/lhns/remote-docker/core/notify"
	"github.com/lhns/remote-docker/dircache"
)

// cacheObserver hands the watcher's changes to the cache, which cannot name
// core/notify itself (ADR 0021).
type cacheObserver struct{ cache *dircache.Cache }

func (o cacheObserver) Observe(event notify.Event) {
	o.cache.Observe(dircache.Event{
		Share: event.Export,
		Path:  event.Path,
		Op:    cacheOp(event.Op),
		Dir:   event.Dir,
	})
}

// cacheOp maps one bitset to the other bit by bit, never by dircache.Op(op),
// which would keep compiling after either side reordered a constant.
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
