package unions

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/lhns/remote-docker/core-agent/replay"
	"github.com/lhns/remote-docker/core/cache"
)

// What the container changed, read out of the cache layer (ADR 0044).
//
// The layer holds what the client's stream wrote as well as what the container
// did, which is what live.applied exists to separate; its field comment has the
// argument.
//
// Read directly rather than through the merged mount, which is the one place
// this design reads a layer instead of the union. Reading is safe where writing
// is not: overlayfs leaves a WRITE underneath a mounted union undefined, and
// says nothing against looking.

// Changes lists what the container did to a share.
func (m *Manager) Changes(ctx context.Context, account, export string) ([]cache.Change, error) {
	l, upper, err := m.upperRoot(ctx, account, export)
	if err != nil {
		return nil, err
	}

	// Asked before the walk, because WalkDir reports a root it cannot read
	// through the same callback as any other entry -- so an upper that is not
	// there at all comes back as no changes and no error, which is
	// indistinguishable from a container that wrote nothing. The failure that
	// hides is a share whose write-back silently never happens.
	if _, err := os.Stat(upper); err != nil {
		return nil, fmt.Errorf("unions: the cache layer of %s cannot be read: %w", export, err)
	}

	var out []cache.Change
	err = filepath.WalkDir(upper, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == upper {
			return nil //nolint:nilerr // an unreadable entry is one this pass does not report
		}
		rel, err := filepath.Rel(upper, p)
		if err != nil {
			return nil
		}
		name := "/" + filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return nil
		}

		switch {
		case isWhiteout(info):
			// A character device 0:0 is how an overlay records a deletion, and
			// it is the only way to tell a file the container removed from one
			// that was never cached.
			out = append(out, cache.Change{Path: name, Deleted: true})
			return nil
		case info.IsDir():
			// A directory in the upper is where a copy-up happened, not a
			// change in itself. The files inside it are reported on their own.
			return nil
		case !info.Mode().IsRegular():
			// A socket or device the container made is not something to carry
			// back to another machine, for the same reason it is not carried
			// in (ADR 0039).
			return nil
		}

		// Exactly what the client's own stream wrote through this union, so
		// not a container change at all. Left in, an idle session is told
		// about the whole cached tree every few seconds forever.
		if l.isApplied(name, info.Size(), info.ModTime()) {
			return nil
		}

		out = append(out, cache.Change{
			Path:    name,
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("unions: reading what changed in %s: %w", export, err)
	}
	return out, nil
}

// Pull streams the named paths out of the cache layer as a tar.
func (m *Manager) Pull(ctx context.Context, account, export string, paths []string) ([]byte, error) {
	_, upper, err := m.upperRoot(ctx, account, export)
	if err != nil {
		return nil, err
	}

	// Resolved one at a time rather than through TarFilesFrom, because each
	// path comes from the client and `within` is what refuses one that leaves
	// the share. A file that has gone since it was reported is skipped by
	// WriteTar, which is ordinary here: the container is still running.
	files := make([]cache.TarFile, 0, len(paths))
	for _, p := range paths {
		target, err := within(upper, p)
		if err != nil {
			return nil, err
		}
		files = append(files, cache.TarFile{
			Name: strings.TrimPrefix(p, "/"),
			Path: target,
		})
	}

	var buf bytes.Buffer
	if err := cache.WriteTar(files, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// upperRoot is the cache layer of a share, as the AGENT can read it.
func (m *Manager) upperRoot(_ context.Context, account, export string) (*live, string, error) {
	m.mu.Lock()
	l, ok := m.shares[key(account, export)]
	m.mu.Unlock()

	if !ok {
		return nil, "", fmt.Errorf("unions: %s has no cache: %w", export, ErrNoShare)
	}
	root, err := replay.Relocate(l.spec.Upper(), func() (string, error) { return l.spec.Root(), nil })
	if err != nil {
		return nil, "", fmt.Errorf("unions: locating the cache layer of %s: %w", export, err)
	}
	return l, root, nil
}

// isWhiteout reports whether an entry is an overlay's record of a deletion.
//
// A character device with major and minor both zero. Checked by mode and
// device number rather than by name, because a file legitimately called
// anything may exist beside it.
func isWhiteout(info fs.FileInfo) bool {
	if info.Mode()&fs.ModeCharDevice == 0 {
		return false
	}
	return rdev(info) == 0
}
