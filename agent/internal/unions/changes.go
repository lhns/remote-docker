package unions

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

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

// whiteoutPrefix starts every name an overlay uses for its own marks, which
// fuse-overlayfs writes as files (see markerOf).
const whiteoutPrefix = ".wh."

// The names that say a directory hides what its lower held. fuse-overlayfs
// writes both, a regular file and a character device, beside the opaque
// attribute (test/union-probe.sh section 13).
const (
	opaqueMarker    = ".wh..wh..opq"
	opaqueMarkerDev = ".wh..opq"
)

// Changes lists what the container did to a share. With carry it also reports
// replaced directories and links, which a client that did not ask would read as
// an ordinary change.
func (m *Manager) Changes(account, client, export string, carry bool) ([]cache.Change, error) {
	l, upper, err := m.upperRoot(account, client, export)
	if err != nil {
		return nil, err
	}

	return changesIn(l, upper, export, carry)
}

// changesIn walks one share's cache layer.
func changesIn(l *live, upper, export string, carry bool) ([]cache.Change, error) {
	// Asked before the walk, because WalkDir reports a root it cannot read
	// through the same callback as any other entry -- so an upper that is not
	// there at all comes back as no changes and no error, which is
	// indistinguishable from a container that wrote nothing. The failure that
	// hides is a share whose write-back silently never happens.
	if _, err := os.Stat(upper); err != nil {
		return nil, fmt.Errorf("unions: the cache layer of %s cannot be read: %w", export, err)
	}

	var out []cache.Change
	// A replaced directory is told by its attribute and by its marker files,
	// and is one change however many of them say so.
	opaque := map[string]bool{}
	err := filepath.WalkDir(upper, func(p string, d fs.DirEntry, err error) error {
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

		// An overlay's own marks are never the container's files. Written into
		// the user's tree they are names the union cannot hold, and the
		// deletions they stand for would never arrive.
		if strings.HasPrefix(d.Name(), whiteoutPrefix) {
			if d.IsDir() {
				return fs.SkipDir
			}
			if change, ok := markerChange(name, info); ok && (change.Deleted || carry) {
				if change.Opaque {
					if opaque[change.Path] {
						return nil
					}
					opaque[change.Path] = true
				}
				out = append(out, change)
			}
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
			// change in itself, unless it hides what the lower held under it.
			// The files inside it are reported on their own.
			if carry && !opaque[name] && opaqueDir(p) {
				opaque[name] = true
				out = append(out, cache.Change{Path: name, Opaque: true})
			}
			return nil
		case info.Mode()&fs.ModeSymlink != 0:
			// Never followed: WalkDir does not enter a linked directory, and
			// the target is read as text and sent as it is.
			if !carry {
				return nil
			}
			target, err := os.Readlink(p)
			if err != nil {
				return nil
			}
			out = append(out, cache.Change{Path: name, Link: target, ModTime: info.ModTime().UnixNano()})
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

// markerChange reads a file named like an overlay mark: .wh.<name> is the
// deletion of <name> beside it, and .wh..wh..opq says the directory it sits in
// was replaced. Any other .wh. name is the overlay's bookkeeping and means
// nothing to a client.
func markerChange(name string, info fs.FileInfo) (cache.Change, bool) {
	dir, base := path.Split(name)
	switch {
	case base == opaqueMarker || base == opaqueMarkerDev:
		dir = strings.TrimSuffix(dir, "/")
		if dir == "" {
			// The share's own root cannot be replaced, and a client told so
			// would delete everything the fill sent.
			return cache.Change{}, false
		}
		return cache.Change{Path: dir, Opaque: true}, true
	case strings.HasPrefix(base, whiteoutPrefix+".wh."):
		return cache.Change{}, false
	default:
		if !info.Mode().IsRegular() && info.Mode()&fs.ModeCharDevice == 0 {
			return cache.Change{}, false
		}
		return cache.Change{Path: dir + strings.TrimPrefix(base, whiteoutPrefix), Deleted: true}, true
	}
}

// Pull streams the named paths out of the cache layer as a tar.
func (m *Manager) Pull(account, client, export string, paths []string) ([]byte, error) {
	_, upper, err := m.upperRoot(account, client, export)
	if err != nil {
		return nil, err
	}
	return pull(upper, paths)
}

// pull is the named paths under upper as a tar. A file that has gone since it
// was reported is skipped, which is ordinary: the container is still running.
func pull(upper string, paths []string) ([]byte, error) {
	r, err := os.OpenRoot(upper)
	if err != nil {
		return nil, fmt.Errorf("unions: opening the cache layer: %w", err)
	}
	defer func() { _ = r.Close() }()

	files := make([]cache.TarFile, 0, len(paths))
	for _, p := range paths {
		name, err := within(p)
		if err != nil {
			return nil, err
		}
		files = append(files, cache.TarFile{Name: name, Path: name})
	}

	var buf bytes.Buffer
	if err := cache.WriteTarFrom(r.Open, files, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// upperRoot is the cache layer of a share, as the AGENT can read it.
func (m *Manager) upperRoot(account, client, export string) (*live, string, error) {
	l, ok := m.share(account, client, export)
	if !ok {
		return nil, "", fmt.Errorf("unions: %s has no cache: %w", export, ErrNoShare)
	}
	root, err := l.relocate(l.spec.Upper())
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
