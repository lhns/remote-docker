package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/lhns/remote-docker/core-client/fswatch"
	"github.com/lhns/remote-docker/core-client/nfsserve"
	"github.com/lhns/remote-docker/core-client/tunnelclient"
	"github.com/lhns/remote-docker/core/notify"
	"github.com/lhns/remote-docker/core/workspace"
)

// notifySink writes change frames to the agent over the workspace-notify
// channel.
type notifySink struct {
	stream io.ReadWriteCloser

	mu sync.Mutex
	w  *bufio.Writer
}

// openNotify establishes the change-notification channel and completes the
// version handshake (see greet).
func openNotify(ctx context.Context, client *tunnelclient.Client) (*notifySink, error) {
	stream, _, _, err := greet(ctx, client, notify.Command, notify.MaxFrame, notify.Version,
		func(frame notify.Frame) (int, bool) {
			if frame.Hello == nil {
				return 0, false
			}
			return frame.Hello.Version, true
		})
	if err != nil {
		return nil, err
	}
	return &notifySink{stream: stream, w: bufio.NewWriter(stream)}, nil
}

// Send writes one frame as a single line.
func (s *notifySink) Send(_ context.Context, frame notify.Frame) error {
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(encoded)+1 > notify.MaxFrame {
		// The agent's scanner would truncate it and desynchronise.
		return fmt.Errorf("change frame of %d bytes exceeds the %d byte limit",
			len(encoded)+1, notify.MaxFrame)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return s.w.Flush()
}

func (s *notifySink) Close() error { return s.stream.Close() }

// startNotify attaches the watcher to a connection, once per connection and
// only when some share watches: a session that watches nothing opens no
// channel. It runs at connect and again whenever a mount makes a share watch
// on a connection that is already up (notifyWanted). A workspace without the
// channel is warned about once, not failed.
func (s *Session) startNotify(live *liveConn) {
	w := s.watcher()
	if w == nil || !s.anyWatching() {
		return
	}

	live.notifyMu.Lock()
	if live.notifyTried {
		live.notifyMu.Unlock()
		return
	}
	live.notifyTried = true
	live.notifyMu.Unlock()

	sink, err := openNotify(s.ctx, live.ssh)
	if err != nil {
		s.notifyOnce.Do(func() {
			s.log().Warn("file watchers inside containers will not see your edits (see ADR 0014)", "err", err)
		})
		return
	}

	live.notifyMu.Lock()
	if live.shut {
		live.notifyMu.Unlock()
		_ = sink.Close()
		return
	}
	live.notify = sink
	live.notifyMu.Unlock()
	w.SetSink(sink)
}

// notifyWanted opens the channel on the current connection, if there is one
// and it has none. Off the caller's goroutine: it is called from share
// registration, which holds the volume guard, and opening the channel is a
// round trip. A connection made later is covered by connect.
func (s *Session) notifyWanted() {
	if live, ok := s.gate.currentLive(); ok && live != nil {
		go s.startNotify(live)
	}
}

func sharesOf(registry *nfsserve.Registry) []fswatch.Share {
	all := registry.Shares()
	out := make([]fswatch.Share, 0, len(all))
	for _, share := range all {
		out = append(out, fswatch.Share{
			ExportPath: share.ExportPath,
			LocalPath:  share.LocalPath,
			File:       share.File,
			Mode:       share.Words().Watch,
		})
	}
	return out
}

// anyWatching reports whether some registered share is watched.
func (s *Session) anyWatching() bool {
	for _, share := range s.registry.Shares() {
		if share.Words().Watch.Watching() {
			return true
		}
	}
	return false
}

// watcher is the file watcher, or nil while none has been started. It starts
// with the first mount that needs it when the session's own default is off.
func (s *Session) watcher() *fswatch.Watcher {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	return s.watch
}

// startWatcher starts the watcher if it is not running. A failure refuses the
// mount that needed it and leaves the session open.
func (s *Session) startWatcher() error {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()

	if s.watch != nil {
		return nil
	}
	if s.watchClosed || s.ctx.Err() != nil {
		return fmt.Errorf("the session is closing")
	}
	if !s.opts.Role.hosting() {
		return fmt.Errorf("this session does not serve mounts")
	}
	w, err := fswatch.New(fswatch.Options{
		Budget:  s.opts.WatchBudget,
		Exclude: fswatch.ExcludesOr(s.opts.WatchExclude),
		Log:     s.opts.Log,
	})
	if err != nil {
		return err
	}
	// Sees every change before the mode filters any: a deletion the cache
	// misses leaves it serving a file that is gone (ADR 0044).
	w.SetObserver(cacheObserver{cache: s.cache})
	s.watch = w
	// A connection that is already up has no sink for it yet.
	return nil
}

// closeWatcher stops the watcher, and keeps a late mount from starting another.
func (s *Session) closeWatcher() {
	s.watchMu.Lock()
	w := s.watch
	s.watchClosed = true
	s.watchMu.Unlock()
	if w != nil {
		_ = w.Close()
	}
}

// shareRestored is told of a share brought back from the record, which no
// mount registered: a union goes to the cache, and if it watches, the watcher
// must exist and know it now, or a container started against it is served
// unwatched until the next reconcile.
func (s *Session) shareRestored(share *nfsserve.Share) {
	// Attached with the words it was recorded with, prefetch included: its fill
	// resumes, and its write-back starts again.
	if share.Mode.Union() {
		s.attachShare(share.ExportPath, share.LocalPath, share.Mode, share.Words())
	}
	if !share.Words().Watch.Watching() {
		return
	}
	if err := s.startWatcher(); err != nil {
		s.log().Warn("a restored share needs the file watcher, and it would not start; edits to "+share.LocalPath+" will not reach the workspace", "err", err)
		return
	}
	s.syncWatch()
}

// restoreShare answers a mount for an export this session has not registered,
// with the variant and words the record held. A record from before a word
// existed takes the session's default for it.
func (s *Session) restoreShare(export string) (string, workspace.Mode, workspace.ShareWords, bool) {
	path, mode, words, ok := s.shares.restore(export)
	if !ok {
		return "", workspace.ModeUnset, workspace.ShareWords{}, false
	}
	if words.Watch == workspace.WatchUnset {
		words.Watch = s.opts.Watch
	}
	if words.Prefetch == workspace.PrefetchUnset {
		words.Prefetch = prefetchWord(s.prefetch)
	}
	return path, mode, words, true
}

// reconcileShares periodically resyncs the watcher, covering shares registered
// without notifying it, such as a share restored from the record.
func (s *Session) reconcileShares(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Syncing while dormant would restore the watches Standby dropped.
			if s.isDormant() {
				continue
			}
			s.syncWatch()
		}
	}
}

// reattachUnions brings back the shares a running container still binds, as
// soon as the session is connected, and does not wait for the container to touch
// its mount.
//
// A restored share is attached to the cache and so is write-back for it
// (shareRestored), but restoring waits for a MOUNT or a root handle the kernel
// presents, and a container that only writes into its union presents neither: a
// restarted client then never carried its writes back. The workspace knows which
// unions are mounted, and the record knows which of them are this machine's, so
// the two together name exactly the shares to restore. A share the workspace
// holds no union for is left to the lazy path, and a workspace too old to answer
// is left alone.
func (s *Session) reattachUnions(ctx context.Context, live *liveConn) {
	if s.shares == nil || s.cache == nil {
		return
	}
	// Only when a union is recorded: a session with none must never ask, so it
	// stays silent against a workspace too old for the channel.
	recorded := s.shares.unionExports()
	if len(recorded) == 0 {
		return
	}
	// Through the connection being built, not the gate: this runs inside the
	// connect, before the gate holds the connection.
	ch, err := s.ensureCacheChan(ctx, live)
	if err == nil {
		var names []string
		names, err = ch.Mounted(ctx)
		if err == nil {
			s.restoreMounted(live, recorded, names)
			return
		}
	}
	s.logQuiet(ctx, "could not ask which unions are mounted; a recorded union is restored when its container touches it", "err", err)
}

// restoreMounted restores the recorded unions whose cache volume is in names.
func (s *Session) restoreMounted(live *liveConn, recorded, names []string) {
	mounted := make(map[string]bool, len(names))
	for _, n := range names {
		mounted[n] = true
	}
	restored := 0
	for _, export := range recorded {
		volume, err := workspace.CacheVolumeForExport(live.clientID, export)
		if err != nil || !mounted[volume] {
			continue
		}
		if _, _, ok := s.registry.LookupOrRestore(export); ok {
			restored++
		}
	}
	s.log().Info("looked for recorded unions the workspace still has mounted",
		"recorded", len(recorded), "mounted", len(names), "restored", restored)
}
