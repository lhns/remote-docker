package unions

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/lhns/remote-docker/core-agent/notify"
	"github.com/lhns/remote-docker/core/workspace"
)

// What the container changed, read out of the cache layer (ADR 0044).
//
// The upper layer of an overlay IS that record, exactly: it holds what was
// written through the union and nothing else. So telling a container's write
// from a file that was merely copied in needs no heuristic, no timestamps and
// no comparison -- it is the difference between being in the upper layer and
// not.
//
// Read directly rather than through the merged mount, which is the one place
// this design reads a layer instead of the union. Reading is safe where writing
// is not: overlayfs leaves a WRITE underneath a mounted union undefined, and
// says nothing against looking.

// Changes lists what the container did to a share.
func (m *Manager) Changes(ctx context.Context, account, export string) ([]workspace.CacheChange, error) {
	upper, err := m.upperRoot(ctx, account, export)
	if err != nil {
		return nil, err
	}

	var out []workspace.CacheChange
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
			out = append(out, workspace.CacheChange{Path: name, Deleted: true})
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

		out = append(out, workspace.CacheChange{
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
	upper, err := m.upperRoot(ctx, account, export)
	if err != nil {
		return nil, err
	}

	var buf strings.Builder
	tw := tar.NewWriter(&buf)
	for _, p := range paths {
		target, err := within(upper, p)
		if err != nil {
			return nil, err
		}

		f, err := os.Open(target)
		if err != nil {
			// Gone between being reported and being asked for, which is
			// ordinary: the container is still running. The client sees it
			// missing from the tar and leaves its own copy alone.
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
		header.Name = strings.TrimPrefix(p, "/")
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
		if pad := header.Size - written; pad > 0 {
			if _, err := tw.Write(make([]byte, pad)); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// upperRoot is the cache layer of a share, as the AGENT can read it.
func (m *Manager) upperRoot(ctx context.Context, account, export string) (string, error) {
	m.mu.Lock()
	l, ok := m.shares[key(account, export)]
	m.mu.Unlock()

	if !ok {
		return "", fmt.Errorf("unions: %s has no cache", export)
	}
	root, err := notify.Relocate(l.spec.Upper(), func() (string, error) { return l.spec.Root(), nil })
	if err != nil {
		return "", fmt.Errorf("unions: locating the cache layer of %s: %w", export, err)
	}
	return root, nil
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
