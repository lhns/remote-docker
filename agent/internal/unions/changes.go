package unions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/core/logx"
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
// an ordinary change; with times, each entry's ctime and mode and the empty
// directories (cache.Request.Times).
func (m *Manager) Changes(account, client, export string, carry, times bool) ([]cache.Change, error) {
	l, upper, err := m.upperRoot(account, client, export)
	if err != nil {
		return nil, err
	}

	return changesIn(l, upper, export, carry, times)
}

// changesIn walks one share's cache layer.
func changesIn(l *live, upper, export string, carry, times bool) ([]cache.Change, error) {
	// Asked before the walk, because WalkDir reports a root it cannot read
	// through the same callback as any other entry -- so an upper that is not
	// there at all comes back as no changes and no error, which is
	// indistinguishable from a container that wrote nothing. The failure that
	// hides is a share whose write-back silently never happens.
	if _, err := os.Stat(upper); err != nil {
		return nil, fmt.Errorf("unions: the cache layer of %s cannot be read: %w", export, err)
	}

	// stamped adds what a client that asked for times is told.
	stamped := func(ch cache.Change, info fs.FileInfo) cache.Change {
		if times {
			ch.Changed = ctimeOf(info)
			ch.Mode = uint32(info.Mode().Perm())
		}
		return ch
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
		// deletions they stand for would never arrive. A mark's ctime is when
		// the deletion or the replace happened.
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
				if times {
					change.Changed = ctimeOf(info)
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
			change := cache.Change{Path: name, Deleted: true}
			if times {
				change.Changed = ctimeOf(info)
			}
			out = append(out, change)
			return nil
		case info.IsDir():
			// A directory in the upper is where a copy-up happened, not a
			// change in itself, unless it hides what the lower held under it.
			// The files inside it are reported on their own.
			if carry && !opaque[name] && opaqueDir(p) {
				opaque[name] = true
				change := cache.Change{Path: name, Opaque: true}
				if times {
					change.Changed = markerCtime(p)
				}
				out = append(out, change)
				return nil
			}
			// An empty one may be a directory the container made, which
			// nothing else would ever report. Whether this machine has it is
			// the client's to say from its own disk: asking the lower here
			// would be an NFS round trip per directory a chown -R copied up.
			if times && emptyDir(p) {
				out = append(out, stamped(cache.Change{Path: name, Dir: true}, info))
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
			out = append(out, stamped(cache.Change{Path: name, Link: target, ModTime: info.ModTime().UnixNano()}, info))
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
		if l.isApplied(name, info.Size(), info.ModTime(), ctimeOf(info)) {
			return nil
		}

		out = append(out, stamped(cache.Change{
			Path:    name,
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
		}, info))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("unions: reading what changed in %s: %w", export, err)
	}
	return out, nil
}

// markerCtime is when a directory was replaced: the earliest ctime of the
// marker files fuse-overlayfs writes into it (test/union-probe.sh section 13).
// Never the directory's own, which moves with every file the container makes
// in it afterwards and would make an edit here look older than the replace.
// 0, not known, when there is no marker to read.
func markerCtime(dir string) int64 {
	var earliest int64
	for _, name := range []string{opaqueMarker, opaqueMarkerDev} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if c := ctimeOf(info); c != 0 && (earliest == 0 || c < earliest) {
			earliest = c
		}
	}
	return earliest
}

// emptyDir reports whether a directory has no entries at all, overlay marks
// included: one holding only a whiteout is a copy-up, not a directory the
// container made empty.
func emptyDir(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(1)
	return len(names) == 0 && errors.Is(err, io.EOF)
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

// Pull streams the named paths out of the cache layer as a tar. With carry, a
// path it cannot name or a file it cannot read is left out, named in the
// returned list and logged, where without it the whole request fails as it
// always did: one bad file must not starve every file asked for beside it.
func (m *Manager) Pull(account, client, export string, paths []string, carry bool) ([]byte, []string, error) {
	_, upper, err := m.upperRoot(account, client, export)
	if err != nil {
		return nil, nil, err
	}
	tarball, skipped, err := pull(upper, paths, carry)
	if len(skipped) > 0 {
		logx.Or(m.Log).Warn("files a container wrote cannot be read to carry them back; they are left out",
			"export", export, "files", len(skipped), "first", skipped[0].Name, "err", skipped[0].Err)
	}
	return tarball, names(skipped), err
}

// pull is the named paths under upper as a tar. A file that has gone since it
// was reported is skipped, which is ordinary: the container is still running.
func pull(upper string, paths []string, carry bool) ([]byte, []cache.Unreadable, error) {
	r, err := os.OpenRoot(upper)
	if err != nil {
		return nil, nil, fmt.Errorf("unions: opening the cache layer: %w", err)
	}
	defer func() { _ = r.Close() }()

	var skipped []cache.Unreadable
	files := make([]cache.TarFile, 0, len(paths))
	for _, p := range paths {
		name, err := within(p)
		if err != nil {
			if !carry {
				return nil, nil, err
			}
			skipped = append(skipped, cache.Unreadable{Name: p, Err: err})
			continue
		}
		files = append(files, cache.TarFile{Name: name, Path: name})
	}

	var buf bytes.Buffer
	if !carry {
		if err := cache.WriteTarFrom(r.Open, files, &buf); err != nil {
			return nil, nil, err
		}
		return buf.Bytes(), nil, nil
	}
	unreadable, err := cache.WriteTarNaming(r.Open, files, &buf)
	if err != nil {
		return nil, nil, err
	}
	for _, u := range unreadable {
		skipped = append(skipped, cache.Unreadable{Name: "/" + u.Name, Err: u.Err})
	}
	return buf.Bytes(), skipped, nil
}

// names are the share paths of what a pull left out.
func names(skipped []cache.Unreadable) []string {
	out := make([]string, 0, len(skipped))
	for _, s := range skipped {
		out = append(out, s.Name)
	}
	return out
}

// Present answers which of the paths exist in the merged view of a share, by
// lstat and never following a link. The merged view is what a container sees:
// a file a client wrote through the union is there although the cache layer
// listing hides it, and a file the container deleted is not.
func (m *Manager) Present(ctx context.Context, account, client, export string, paths []string) ([]string, error) {
	_, root, err := m.mergedRoot(ctx, account, client, export)
	if err != nil {
		return nil, err
	}
	return present(root, paths)
}

func present(root string, paths []string) ([]string, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("unions: opening the share: %w", err)
	}
	defer func() { _ = r.Close() }()

	var out []string
	for _, p := range paths {
		name, err := within(p)
		if err != nil {
			// Cannot be asked about, so it is not claimed absent.
			out = append(out, p)
			continue
		}
		if _, err := r.Lstat(name); err == nil || !errors.Is(err, os.ErrNotExist) {
			// Present, or an error that is not "absent": keep.
			out = append(out, p)
		}
	}
	return out, nil
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
