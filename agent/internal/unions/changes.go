package unions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

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
	defer func() { _ = upper.Close() }()

	return changesIn(l, upper, export, carry, times)
}

// changesIn walks one share's cache layer, every entry reached through upper so
// that a link the account planted in its own filesystem leads nowhere.
func changesIn(l *live, upper *os.Root, export string, carry, times bool) ([]cache.Change, error) {
	// Asked before the walk, because WalkDir reports a root it cannot read
	// through the same callback as any other entry -- so an upper that is not
	// there at all comes back as no changes and no error, which is
	// indistinguishable from a container that wrote nothing. The failure that
	// hides is a share whose write-back silently never happens.
	if _, err := upper.Stat("."); err != nil {
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
	err := fs.WalkDir(upper.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || rel == "." {
			return nil //nolint:nilerr // an unreadable entry is one this pass does not report
		}
		name := "/" + rel

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
			if carry && !opaque[name] && opaqueDir(upper, rel) {
				opaque[name] = true
				change := cache.Change{Path: name, Opaque: true}
				if times {
					change.Changed = markerCtime(upper, rel)
				}
				out = append(out, change)
				return nil
			}
			// An empty one may be a directory the container made, which
			// nothing else would ever report. Whether this machine has it is
			// the client's to say from its own disk: asking the lower here
			// would be an NFS round trip per directory a chown -R copied up.
			if times && emptyDir(upper, rel) {
				out = append(out, stamped(cache.Change{Path: name, Dir: true}, info))
			}
			return nil
		case info.Mode()&fs.ModeSymlink != 0:
			// Never followed: WalkDir does not enter a linked directory, and
			// the target is read as text and sent as it is.
			if !carry {
				return nil
			}
			target, err := upper.Readlink(rel)
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
func markerCtime(r *os.Root, dir string) int64 {
	var earliest int64
	for _, name := range []string{opaqueMarker, opaqueMarkerDev} {
		info, err := r.Lstat(path.Join(dir, name))
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
func emptyDir(r *os.Root, dir string) bool {
	f, err := r.Open(dir)
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
func (m *Manager) Pull(account, client, export string, paths []string, carry bool) (*Spool, []string, error) {
	_, upper, err := m.upperRoot(account, client, export)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = upper.Close() }()

	spool, skipped, err := pull(upper, paths, carry)
	if len(skipped) > 0 {
		logx.Or(m.Log).Warn("files a container wrote cannot be read to carry them back; they are left out",
			"export", export, "files", len(skipped), "first", skipped[0].Name, "err", skipped[0].Err)
	}
	return spool, names(skipped), err
}

// Spool is a pull's tar, written to a file in the agent's temporary directory
// so that a large file is never held in memory. It is positioned at the start
// and Close removes it.
type Spool struct {
	*os.File
	// Size is the length of the tar, which the reply announces.
	Size int64
}

// Close closes the file and removes it, if pull could not already.
func (s *Spool) Close() error {
	err := s.File.Close()
	_ = os.Remove(s.Name())
	return err
}

// openRead opens a file for reading without waiting on it: a FIFO a container
// made in its share would otherwise block the open until somebody writes to it,
// and the cache channel answers one request at a time. What is not a regular
// file is then left out by the tar writer, which checks.
func openRead(r *os.Root) func(string) (*os.File, error) {
	return func(name string) (*os.File, error) {
		return r.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	}
}

// pull is the named paths under upper as a tar. A file that has gone since it
// was reported is skipped, which is ordinary: the container is still running.
func pull(upper *os.Root, paths []string, carry bool) (*Spool, []cache.Unreadable, error) {
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

	f, err := os.CreateTemp("", "rd-pull-*.tar")
	if err != nil {
		return nil, nil, fmt.Errorf("unions: spooling what the container wrote: %w", err)
	}
	// Unlinked at once, so an agent killed mid-pull leaves no copy of the
	// container's files in /tmp; the open descriptor keeps it readable. Close
	// removes it by name as well, for a platform that refuses to unlink an
	// open file.
	_ = os.Remove(f.Name())
	spool := &Spool{File: f}
	fail := func(err error) (*Spool, []cache.Unreadable, error) {
		_ = spool.Close()
		return nil, nil, err
	}

	if !carry {
		if _, err := cache.WriteTarFrom(openRead(upper), files, spool); err != nil {
			return fail(err)
		}
	} else {
		unreadable, err := cache.WriteTarNaming(openRead(upper), files, spool.File)
		if err != nil {
			return fail(err)
		}
		for _, u := range unreadable {
			skipped = append(skipped, cache.Unreadable{Name: "/" + u.Name, Err: u.Err})
		}
	}

	size, err := spool.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = spool.Seek(0, io.SeekStart)
	}
	if err != nil {
		return fail(fmt.Errorf("unions: rewinding the spooled archive: %w", err))
	}
	spool.Size = size
	return spool, skipped, nil
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
	defer func() { _ = root.Close() }()
	return present(root, paths)
}

func present(r *os.Root, paths []string) ([]string, error) {
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

// upperRoot is the cache layer of a share, opened inside the daemon's own
// filesystem (openIn). The caller closes it.
func (m *Manager) upperRoot(account, client, export string) (*live, *os.Root, error) {
	l, ok := m.share(account, client, export)
	if !ok {
		return nil, nil, fmt.Errorf("unions: %s has no cache: %w", export, ErrNoShare)
	}
	root, err := openIn(l.spec, l.spec.Upper())
	if err != nil {
		return nil, nil, fmt.Errorf("unions: opening the cache layer of %s: %w", export, err)
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
