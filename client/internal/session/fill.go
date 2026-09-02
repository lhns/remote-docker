package session

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/lhns/remote-docker/core-client/cachefill"
	"github.com/lhns/remote-docker/core-client/writeback"
	"github.com/lhns/remote-docker/core/workspace"
)

// Filling a delegated share's cache, in the background (ADR 0044).

// fillState is what a share's fill has done so far, for reporting.
type fillState struct {
	Stats  cachefill.Stats
	Sent   int
	Done   bool
	Err    error
	Cached bool // whether the cache holds everything the plan chose
}

// fills tracks the background fills of this session.
type fills struct {
	mu    sync.Mutex
	state map[string]*fillState

	// roots is where each delegated share's files are on this machine, which
	// is what invalidation needs to read a changed file back.
	roots map[string]string

	// manifests are what the fill put in each cache: for every path, the size
	// and modification time it had HERE when it was sent. That is the baseline
	// write-back compares both sides against, which is what lets it decide
	// almost every case without comparing two machines' clocks (ADR 0044).
	manifests map[string]map[string]writeback.Baseline
}

func (f *fills) set(export, localPath string, s *fillState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		f.state = map[string]*fillState{}
		f.roots = map[string]string{}
		f.manifests = map[string]map[string]writeback.Baseline{}
	}
	f.state[export] = s
	f.roots[export] = localPath
	f.manifests[export] = map[string]writeback.Baseline{}
}

// forget drops a share, so a later container against the same directory fills
// it from scratch rather than against a manifest for a cache that is gone.
func (f *fills) forget(export string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.state, export)
	delete(f.roots, export)
	delete(f.manifests, export)
}

func (f *fills) get(export string) (fillState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[export]
	if !ok {
		return fillState{}, false
	}
	return *s, true
}

// Fill starts filling a share's cache and returns at once.
//
// Started once per share per session: a second container against the same
// directory finds the fill already running or finished, and re-sending the tree
// would cost the link twice for nothing.
func (s *Session) Fill(export, localPath string) {
	if _, running := s.fills.get(export); running {
		return
	}
	state := &fillState{}
	s.fills.set(export, localPath, state)

	go func() {
		// Before the fill, because a fill cannot remove anything. See cached.go.
		s.reconcileDeletions(export, localPath)

		err := s.fill(export, localPath, state)
		s.fills.mu.Lock()
		state.Done, state.Err = true, err
		// What the tree held against what the cache got, which is the question
		// write-back turns on. Not "did every batch we sent arrive", which is
		// the same number counted twice: a budget that stops the fill short
		// leaves batches that all succeeded and a cache that is missing files.
		state.Cached = err == nil && state.Stats.Complete()
		s.fills.mu.Unlock()

		if err != nil {
			// Not fatal to anything: the share works, it is just slower than
			// it would have been. Said once rather than per batch.
			s.logQuiet(s.ctx, "a share's cache could not be filled", "export", export, "err", err)
		}

		// What the NEXT session has to reconcile against. Recorded even on a
		// partial fill, because it names what is in the cache, and what is in
		// the cache is what a later deletion has to be able to remove.
		if s.cached != nil {
			s.cached.record(export, s.manifestPaths(export))
		}
	}()
}

// reconcileDeletions takes out of the cache what this machine no longer has,
// from what a previous fill recorded (ADR 0044; see cached.go).
func (s *Session) reconcileDeletions(export, localPath string) {
	if s.cached == nil {
		return
	}
	filled, ok := s.cached.filled(export)
	if !ok {
		return
	}
	s.dropDeleted(export, localPath, filled)
}

// dropDeleted removes from a share's cache the named paths this machine no
// longer has. Callers pass only paths a fill recorded: a path in the cache that
// no fill sent is a container's own file, and this must never remove one.
func (s *Session) dropDeleted(export, localPath string, filled []string) {
	gone := deletedSince(localPath, filled)
	if len(gone) == 0 {
		return
	}

	live := s.liveCache()
	if live == nil {
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, invalidateTimeout)
	defer cancel()

	for _, paths := range chunkPaths(gone) {
		if err := live.Drop(ctx, export, paths); err != nil {
			s.logQuiet(ctx, "removing from a cache what this machine no longer has",
				"export", export, "err", err)
			return
		}
	}
	s.log().Info("took deleted files out of a share's cache",
		"export", export, "files", len(gone))
}

// manifestPaths is what this session's fill put in a share's cache.
func (s *Session) manifestPaths(export string) []string {
	s.fills.mu.Lock()
	defer s.fills.mu.Unlock()

	paths := make([]string, 0, len(s.fills.manifests[export]))
	for p := range s.fills.manifests[export] {
		paths = append(paths, p)
	}
	return paths
}

// fill scans the tree and uploads from it at the same time, cheapest first.
//
// The policy is cachefill.Stream's; this supplies what it cannot know: where
// the bytes go.
func (s *Session) fill(export, localPath string, state *fillState) error {
	budget := cachefill.Budget{Files: s.opts.Config.CacheFiles, Bytes: s.opts.Config.CacheBytes}
	stats, err := cachefill.Stream(localPath, s.opts.WatchExclude, budget,
		func(batch []cachefill.Entry) error {
			return s.sendBatch(export, localPath, batch, state)
		})

	s.fills.mu.Lock()
	state.Stats = stats
	s.fills.mu.Unlock()
	return err
}

// sendBatch builds one tar and applies it.
func (s *Session) sendBatch(export, localPath string, entries []cachefill.Entry, state *fillState) error {
	live := s.liveCache()
	if live == nil {
		// No connection to send it over. Not an error: the share is served
		// from the lower meanwhile, and the next fill starts from scratch.
		return nil
	}

	body, err := tarOf(localPath, entries, live.Codec())
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(s.ctx, fillBatchTimeout)
	defer cancel()
	if err := live.Apply(ctx, export, int64(len(body)), bytes.NewReader(body)); err != nil {
		return err
	}

	s.fills.mu.Lock()
	state.Sent += len(entries)
	s.noteSent(export, localPath, entries)
	s.fills.mu.Unlock()
	return nil
}

// noteSent records what a batch put in the cache. The caller holds the lock.
//
// Read from disk again rather than taken from the walk: what matters is the
// state of the file as it was SENT, and a file rewritten between the walk and
// the read would otherwise be recorded as something it never was -- which
// write-back would later read as the container having changed it.
func (s *Session) noteSent(export, localPath string, entries []cachefill.Entry) {
	manifest := s.fills.manifests[export]
	if manifest == nil {
		return
	}
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(localPath, filepath.FromSlash(e.Path)))
		if err != nil {
			continue
		}
		manifest["/"+e.Path] = writeback.Baseline{Size: info.Size(), ModTime: info.ModTime()}
	}
}

// fillBatchTimeout bounds one send. Generous, because a batch is up to
// DefaultBatchBytes over whatever link the workspace is on, and a fill that
// gives up early leaves a share slower rather than broken.
const fillBatchTimeout = 10 * time.Minute

// tarOf builds the batch.
//
// In memory because the channel frames a payload by length: the workspace has
// to be told how many bytes follow before they are sent. cachefill.Batches is
// what keeps that bounded.
func tarOf(root string, entries []cachefill.Entry, codec string) ([]byte, error) {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Path)
	}

	var buf bytes.Buffer
	if codec != workspace.CodecZstd {
		// Written before the buffer is read: `return buf.Bytes(), WriteTar(...)`
		// evaluates the bytes first and hands back an empty slice.
		if err := workspace.WriteTar(workspace.TarFilesFrom(root, names), &buf); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	// The compressor wraps the tar writer, so the tar is written once and the
	// bytes that leave are the encoded ones, which is what the frame's length
	// has to describe. Default level: a source tree compresses hard enough that
	// the link, not the CPU, is what the fill waits on.
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		return nil, err
	}
	if err := workspace.WriteTar(workspace.TarFilesFrom(root, names), zw); err != nil {
		_ = zw.Close()
		return nil, err
	}
	// Closed before the buffer is measured, or the payload's length is right
	// and its contents end early.
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
