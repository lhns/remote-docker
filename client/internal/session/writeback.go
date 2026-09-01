package session

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lhns/remote-docker/client/internal/writeback"
)

// Carrying a delegated container's writes back to this machine (ADR 0044).
//
// The one part of this mode that writes into a user's own directory, so what it
// does is decided in client/internal/writeback -- a pure function with the
// rules and their tests -- and what happens here is only the fetching and the
// writing.
//
// Two things it will not do, both because the cost of being wrong is somebody's
// source tree:
//
//   - nothing at all while the cache is incomplete. A file the fill never sent
//     looks exactly like one the container created.
//   - nothing silently. Every conflict is reported by path, whichever way it
//     resolved.

// writeBackEvery is how often a share's changes are collected.
//
// A poll rather than a subscription: the container's writes land in a layer the
// workspace can read at any time, and nothing pushes. Five seconds is short
// enough that a build's output is there before somebody goes looking, and long
// enough not to walk a cache layer continuously.
const writeBackEvery = 5 * time.Second

// writeBackTimeout bounds one round.
const writeBackTimeout = 2 * time.Minute

// watchWriteBack carries container writes back for as long as the session runs.
func (s *Session) watchWriteBack(ctx context.Context) {
	ticker := time.NewTicker(writeBackEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, export := range s.cachedShares() {
				s.writeBackShare(ctx, export)
			}
		}
	}
}

// cachedShares is every delegated share this session holds.
func (s *Session) cachedShares() []string {
	s.fills.mu.Lock()
	defer s.fills.mu.Unlock()

	out := make([]string, 0, len(s.fills.roots))
	for export := range s.fills.roots {
		out = append(out, export)
	}
	return out
}

// writeBackShare collects one share's container writes and applies them here.
func (s *Session) writeBackShare(ctx context.Context, export string) {
	state, ok := s.fills.get(export)
	local, hasRoot := s.cachedShare(export)
	if !ok || !hasRoot {
		return
	}

	// Still filling means there is nothing settled to compare against. Whether
	// the cache is COMPLETE is the other half, and it goes to Decide, which is
	// where the rule and its tests live.
	if !state.Done {
		return
	}

	live := s.liveCache()
	if live == nil {
		// Nothing to ask over. The changes stay in the cache and the next
		// round collects them.
		return
	}

	ctx, cancel := context.WithTimeout(ctx, writeBackTimeout)
	defer cancel()

	changes, err := live.Changes(ctx, export)
	if errors.Is(err, errShareGone) {
		// Released, because nothing is bound to it any more (ADR 0044). The
		// cache went with it, so there is nothing to carry back and nothing to
		// compare against: stop polling rather than ask again every five
		// seconds for the life of the session.
		s.fills.forget(export)
		return
	}
	if err != nil {
		s.logQuiet(ctx, "asking what a container changed", "export", export, "err", err)
		return
	}
	if len(changes) == 0 {
		return
	}

	actions := writeback.Decide(s.manifestOf(export), changes, s.localFile(local), s.skew(), state.Cached)
	if len(actions) == 0 {
		return
	}

	for _, conflict := range writeback.Conflicts(actions) {
		// Reported whichever way it resolved. Choosing silently is the one
		// thing this must not do.
		s.log().Warn("a file changed in both places",
			"path", strings.TrimPrefix(conflict.Path, "/"),
			"kind", conflict.Kind, "outcome", conflict.Why)
	}

	for _, paths := range chunkPaths(writeback.Writes(actions)) {
		body, err := live.Pull(ctx, export, paths)
		if err != nil {
			s.logQuiet(ctx, "fetching what a container wrote", "export", export, "err", err)
			return
		}
		if err := extractInto(local, bytes.NewReader(body)); err != nil {
			s.logQuiet(ctx, "writing back what a container wrote", "export", export, "err", err)
			return
		}
	}

	for _, p := range writeback.Deletes(actions) {
		target := filepath.Join(local, filepath.FromSlash(strings.TrimPrefix(p, "/")))
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logQuiet(ctx, "removing what a container deleted", "path", target, "err", err)
		}
	}

	// The manifest moves with the files: what was just written back is now what
	// both sides agree on, so the next round starts from it rather than seeing
	// the same change again.
	s.rebaseManifest(export, local, actions)
}

// localFile answers what this machine currently has at a share-relative path.
func (s *Session) localFile(root string) writeback.Local {
	return func(p string) (os.FileInfo, bool) {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(p, "/"))))
		if err != nil {
			return nil, false
		}
		return info, true
	}
}

// extractInto writes a tar under root, refusing anything that leaves it.
func extractInto(root string, body io.Reader) error {
	tr := tar.NewReader(body)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}

		// The workspace named these, and the workspace is not this machine's
		// to trust with a path. Checked on the RESULT, because filepath.Join
		// cleans and "../.." looks like an ordinary path afterwards.
		target := filepath.Join(root, filepath.FromSlash(header.Name))
		if !strings.HasPrefix(target, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator)) {
			return errors.New("session: a written-back path leaves the share")
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, header.FileInfo().Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		// The time the CONTAINER wrote it, which is what a plain mount would
		// have shown, and what the next round compares against.
		if !header.ModTime.IsZero() {
			_ = os.Chtimes(target, time.Time{}, header.ModTime)
		}
	}
}

// manifestOf is a copy of what the fill sent for a share.
//
// Copied rather than handed out: the fill may still be adding to it while a
// round of write-back is deciding, and a map read under one lock and used under
// none is how a decision about somebody's files goes wrong at random.
func (s *Session) manifestOf(export string) map[string]writeback.Baseline {
	s.fills.mu.Lock()
	defer s.fills.mu.Unlock()

	out := make(map[string]writeback.Baseline, len(s.fills.manifests[export]))
	for p, b := range s.fills.manifests[export] {
		out[p] = b
	}
	return out
}

// rebaseManifest records what both sides now agree on.
//
// Without this the same change is decided again on the next round: the file
// here would still differ from what the fill sent, so a write-back would look
// like a conflict with itself.
func (s *Session) rebaseManifest(export, local string, actions []writeback.Action) {
	s.fills.mu.Lock()
	defer s.fills.mu.Unlock()

	manifest := s.fills.manifests[export]
	if manifest == nil {
		return
	}
	for _, a := range actions {
		name := strings.TrimPrefix(a.Path, "/")
		switch {
		case a.Kind == writeback.Delete:
			delete(manifest, a.Path)
		case a.Kind == writeback.Write || (a.Kind == writeback.Conflict && a.Wins):
			info, err := os.Stat(filepath.Join(local, filepath.FromSlash(name)))
			if err != nil {
				delete(manifest, a.Path)
				continue
			}
			manifest[a.Path] = writeback.Baseline{Size: info.Size(), ModTime: info.ModTime()}
		}
	}
}

// skew is the workspace's clock minus this machine's, as measured when the
// connection was made.
//
// Used for one comparison only: which side wrote last when both changed the
// same file. Zero when the workspace does not report a clock, which is an agent
// that predates the field and reads as "assume they agree" -- the old behaviour
// and the best guess available.
func (s *Session) skew() time.Duration {
	live, ok := s.gate.currentLive()
	if !ok || live == nil || live.info.Now == 0 {
		return 0
	}
	return live.clockSkew
}
