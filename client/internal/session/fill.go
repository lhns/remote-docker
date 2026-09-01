package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lhns/remote-docker/client/internal/cachefill"
	"github.com/lhns/remote-docker/client/internal/writeback"
	"github.com/lhns/remote-docker/core/workspace"
)

// Filling a delegated share's cache, in the background (ADR 0044).
//
// The container does not wait for this, and that is the whole point of the
// union: what the cache does not hold yet is served from the live export
// underneath, correctly. So a fill that is slow, partial, or still running
// costs speed and never correctness -- which is what makes it safe to start a
// container and fill behind it.

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
		// Before the fill, because it is a REMOVAL and the fill cannot make
		// one: a file deleted here while nothing was running is still in the
		// cache and still visible to a container until something takes it out
		// (ADR 0044).
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

// reconcileDeletions takes out of the cache what this machine no longer has.
//
// The one thing a fill cannot do. It overwrites what changed and adds what is
// new, so a change made while no session ran is carried by the fill itself --
// and a DELETION leaves no trace for it to carry. Without this, a file removed
// here yesterday is still in the container today.
//
// Only paths a previous fill recorded are considered, which is what keeps it
// safe: a path in the cache that no fill put there is a container's own file,
// and this must never remove one of those.
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

// dropDeleted removes from a share's cache every one of the named paths that
// this machine no longer has.
//
// Only paths a fill put there are ever passed in, and that is what keeps it
// safe: a path in the cache that no fill sent is a container's own file, and
// this must never remove one of those.
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
	stats, err := cachefill.Stream(localPath, s.opts.WatchExclude, cachefill.Budget{},
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
	var buf bytes.Buffer

	// The compressor wraps the tar writer, so the tar is written once and the
	// bytes that leave are the encoded ones -- which is what the frame's length
	// has to describe.
	var (
		zw    *gzip.Writer
		sink  io.Writer = &buf
		coded           = codec == workspace.CodecGzip
	)
	if coded {
		zw = gzip.NewWriter(&buf)
		sink = zw
	}

	tw := tar.NewWriter(sink)

	for _, e := range entries {
		p := filepath.Join(root, filepath.FromSlash(e.Path))

		// Opened before its header is written, so a file this machine cannot
		// read -- no permission, or locked by another process, which is
		// ordinary on Windows -- is simply absent from the batch rather than
		// present and full of NULs.
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			continue
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			_ = f.Close()
			continue
		}
		header.Name = e.Path
		// Ownership is this machine's and means nothing on the workspace,
		// where the account's uid is what the files must belong to.
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""

		if err := tw.WriteHeader(header); err != nil {
			_ = f.Close()
			return nil, err
		}
		written, err := io.Copy(tw, io.LimitReader(f, header.Size))
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		// A file that shrank while it was read leaves the entry short, which
		// the writer reports on the next header. Pad rather than fail: one
		// truncated file in the cache is still served correctly from the lower
		// once it is invalidated, and a failed batch costs the whole share.
		if pad := header.Size - written; pad > 0 {
			if _, err := tw.Write(make([]byte, pad)); err != nil {
				return nil, err
			}
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	// Both, in order: the tar's own trailer has to be inside the compressed
	// stream, and the compressor's footer has to be written before the buffer
	// is measured. Closing only one leaves a payload whose length is right and
	// whose contents end early.
	if coded {
		if err := zw.Close(); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}
