package session

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lhns/remote-docker/client/internal/cachefill"
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
}

func (f *fills) set(export string, s *fillState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		f.state = map[string]*fillState{}
	}
	f.state[export] = s
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
	s.fills.set(export, state)

	go func() {
		err := s.fill(export, localPath, state)
		s.fills.mu.Lock()
		state.Done, state.Err = true, err
		state.Cached = err == nil && state.Sent == state.Stats.Files
		s.fills.mu.Unlock()

		if err != nil {
			// Not fatal to anything: the share works, it is just slower than
			// it would have been. Said once rather than per batch.
			s.logQuiet(s.ctx, "a share's cache could not be filled", "export", export, "err", err)
		}
	}()
}

// fill walks the tree and sends it, cheapest first.
func (s *Session) fill(export, localPath string, state *fillState) error {
	entries, stats := cachefill.Plan(localPath, s.opts.WatchExclude, cachefill.Budget{})

	s.fills.mu.Lock()
	state.Stats = stats
	s.fills.mu.Unlock()

	live := s.liveCache()
	if live == nil {
		return nil
	}

	for _, batch := range cachefill.Batches(entries, cachefill.DefaultBatchBytes) {
		body, err := tarOf(localPath, batch)
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(s.ctx, fillBatchTimeout)
		err = live.Apply(ctx, export, int64(len(body)), bytes.NewReader(body))
		cancel()
		if err != nil {
			return err
		}

		s.fills.mu.Lock()
		state.Sent += len(batch)
		s.fills.mu.Unlock()
	}
	return nil
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
func tarOf(root string, entries []cachefill.Entry) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

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
	return buf.Bytes(), nil
}
