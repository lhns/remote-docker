package unions

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/core/workspace"
)

// Filling and emptying a share's cache, always THROUGH the merged mount.
//
// Never into the cache layer directly: overlayfs leaves a write under a mounted
// union undefined, and a file written straight into the layer stays invisible
// to a container that already missed on it (test/union-probe.sh section 4).
// Going through the union is also what makes the container's own inotify fire
// natively, which is how ADR 0014 closes for these shares.
//
// The agent writes through /proc/<pid>/root, as core-agent/replay does.

// Apply extracts a tar into a share's union, decoding it first when the client
// compressed it.
//
// The codec is checked again here rather than trusted from Validate: a stream
// that names an encoding this version does not have must not be handed to the
// archive reader, which would fail somewhere inside it with a message about a
// corrupt header instead of about the codec.
//
// With carry, an entry that cannot be written is left out and named in the
// answer, and the rest of the batch lands (cache.Request.Carry); without it the
// first such entry fails the batch, as it always did.
func (m *Manager) Apply(ctx context.Context, account, client, export, codec string, carry bool, body io.Reader) ([]string, error) {
	l, root, err := m.mergedRoot(ctx, account, client, export)
	if err != nil {
		// The payload still has to be drained, or the next frame is read out
		// of the middle of a tar. The caller cannot do it: only here is it
		// known that nothing consumed the body.
		_, _ = io.Copy(io.Discard, body)
		return nil, err
	}

	decoded, done, err := decoded(codec, body)
	if err != nil {
		_, _ = io.Copy(io.Discard, body)
		return nil, err
	}
	defer done()

	return extractWith(root, decoded, carry, func(name string, info os.FileInfo) {
		l.noteApplied(name, info.Size(), info.ModTime())
	})
}

// extract writes a tar into the share rooted at root, telling landed about
// each regular file as it LANDED rather than as it was asked for: a filesystem
// keeping coarser timestamps than the tar would otherwise match nothing, and
// every filled file would read as a container write for the rest of the
// session.
func extract(root string, body io.Reader, landed func(name string, info os.FileInfo)) error {
	_, err := extractWith(root, body, false, landed)
	return err
}

// extractWith is extract that, when skip is set, leaves out an entry it cannot
// write and returns its path instead of failing. A file this entry CREATED and
// could not finish is removed, so a container is not served it short. One that
// was already there is left: the root is the merged mount, so a removal would
// be a whiteout, which hides the live file instead of falling through to it. The
// tar reader steps past what is unread of the entry, so the batch stays in
// frame.
func extractWith(root string, body io.Reader, skip bool, landed func(name string, info os.FileInfo)) ([]string, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("unions: opening the share: %w", err)
	}
	defer func() { _ = r.Close() }()

	var skipped []string
	tr := tar.NewReader(body)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return skipped, nil
		}
		if err != nil {
			return skipped, fmt.Errorf("unions: reading a batch: %w", err)
		}
		created := false
		info, err := writeEntry(r, header, tr, &created)
		if err != nil {
			if !skip {
				return nil, err
			}
			if target, werr := within(header.Name); werr == nil && created {
				_ = r.Remove(target)
			}
			skipped = append(skipped, "/"+strings.TrimPrefix(header.Name, "/"))
			continue
		}
		if info != nil {
			landed("/"+strings.TrimPrefix(header.Name, "/"), info)
		}
	}
}

// writeEntry puts one tar entry into the union, answering with the file it left
// behind for a regular file and nil for anything else.
//
// Only the three kinds a shared tree is made of. A device, a socket or a fifo
// is skipped rather than refused: the client does not send them, and a batch
// that failed because of one would leave the cache half applied for a file
// nothing can use anyway.
func writeEntry(r *os.Root, header *tar.Header, body io.Reader, created *bool) (os.FileInfo, error) {
	target, err := within(header.Name)
	if err != nil {
		return nil, err
	}

	switch header.Typeflag {
	case tar.TypeDir:
		return nil, r.MkdirAll(target, header.FileInfo().Mode().Perm())

	case tar.TypeSymlink:
		// Replaced rather than merged: a symlink that changed target is a
		// different link, and there is no way to edit one in place.
		if err := r.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("unions: replacing the link %s: %w", header.Name, err)
		}
		return nil, r.Symlink(header.Linkname, target)

	case tar.TypeReg:
		if err := r.MkdirAll(path.Dir(target), 0o755); err != nil {
			return nil, fmt.Errorf("unions: creating the directory for %s: %w", header.Name, err)
		}
		// Never O_TRUNC (ADR 0044). A container may have the file open or
		// mapped (git maps its index, packs and the files it hashes), and
		// emptying it first is SIGBUS for a mapping and a short read for
		// everything else. Overwritten from the start instead, and cut only if
		// the new content is shorter, so a refill of unchanged content (the
		// usual one) never changes the size a reader sees.
		//
		// Not a temporary renamed over the target: fuse-overlayfs forgets that
		// a renamed-in file hides one in the lower, so a later `rm` of it
		// shows the lower's file again (test/union-probe.sh section 6d).
		//
		// Created exclusively when it is not there, so the entry knows whether
		// the file is its own: only a file this entry created is ever removed
		// after a failed write. A rename of a temporary would give the same
		// guarantee and is the thing the paragraph above rules out. A file that
		// appeared since (the container made it) is opened as an existing one.
		f, err := r.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, header.FileInfo().Mode().Perm())
		if err == nil {
			*created = true
		} else if errors.Is(err, os.ErrExist) {
			f, err = r.OpenFile(target, os.O_WRONLY|os.O_CREATE, header.FileInfo().Mode().Perm())
		}
		if err != nil {
			return nil, fmt.Errorf("unions: writing %s: %w", header.Name, err)
		}
		if _, err := io.Copy(f, body); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("unions: writing %s: %w", header.Name, err)
		}
		if st, err := f.Stat(); err == nil && st.Size() > header.Size {
			if err := f.Truncate(header.Size); err != nil {
				_ = f.Close()
				return nil, fmt.Errorf("unions: shortening %s: %w", header.Name, err)
			}
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("unions: writing %s: %w", header.Name, err)
		}
		// The client's own modification time, so the cache and the tree it
		// came from agree about when a file was last written. Write-back
		// compares those, and a file stamped with the moment it was copied
		// would look like something the container had just changed.
		if !header.ModTime.IsZero() {
			_ = r.Chtimes(target, time.Time{}, header.ModTime)
		}
		info, err := r.Stat(target)
		if err != nil {
			// Written, and this pass cannot say with what timestamp. Reported
			// as a change once and settled by the client's manifest.
			return nil, nil //nolint:nilerr // the file is there; only the record is missing
		}
		return info, nil

	default:
		return nil, nil
	}
}

// decoded wraps a payload in its codec's reader, and answers with the closer.
//
// An unknown codec is refused rather than read as a plain tar: the client was
// told what this agent accepts in the greeting, so one arriving here is a bug
// on that side, and reading it anyway would corrupt the cache with whatever the
// bytes happened to look like.
func decoded(codec string, body io.Reader) (io.Reader, func(), error) {
	switch codec {
	case cache.CodecNone:
		return body, func() {}, nil

	case cache.CodecZstd:
		zr, err := zstd.NewReader(body)
		if err != nil {
			return nil, nil, fmt.Errorf("unions: reading a %s batch: %w", codec, err)
		}
		return zr, zr.Close, nil

	default:
		return nil, nil, fmt.Errorf("unions: a batch arrived encoded as %q, which this workspace cannot read", codec)
	}
}

// Drop removes paths from a share's union.
//
// This is what a deletion on the client becomes, and it is the reason the agent
// exists in this design at all: the Docker API can write into a volume and can
// never remove from one, so no client-side answer exists.
//
// Removing through the union leaves overlayfs's whiteout, which is correct
// here: the lower has lost the file too, so there is nothing the whiteout could
// wrongly hide.
func (m *Manager) Drop(ctx context.Context, account, client, export string, paths []string) error {
	l, root, err := m.mergedRoot(ctx, account, client, export)
	if err != nil {
		return err
	}

	return remove(root, paths, l.forgetApplied)
}

// remove deletes paths from the share rooted at root, telling gone about each.
func remove(root string, paths []string, gone func(string)) error {
	if len(paths) == 0 {
		return nil
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("unions: opening the share: %w", err)
	}
	defer func() { _ = r.Close() }()

	for _, p := range paths {
		target, err := within(p)
		if err != nil {
			return err
		}
		if err := r.RemoveAll(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("unions: dropping %s: %w", p, err)
		}
		gone(p)
	}
	return nil
}

// mergedRoot is where a share's union can be written, as the AGENT can reach
// it, and it refuses a share that is not serving.
func (m *Manager) mergedRoot(ctx context.Context, account, client, export string) (*live, string, error) {
	l, ok := m.share(account, client, export)
	if !ok {
		return nil, "", fmt.Errorf("unions: %s has no cache; prepare it first: %w", export, ErrNoShare)
	}
	if err := m.aliveErr(ctx, l.spec); err != nil {
		return nil, "", err
	}

	root, err := l.relocate(l.spec.Merged())
	if err != nil {
		return nil, "", fmt.Errorf("unions: locating the cache for %s: %w", export, err)
	}
	return l, root, nil
}

// within is a share path as a name inside an os.Root, which is what keeps a
// symlink the container left in the share from steering this root process out
// of it: followed through /proc/<pid>/root, an absolute target resolves against
// the AGENT's root. The client validated the path too, and that is not a
// reason to skip this.
func within(name string) (string, error) {
	p := "/" + strings.TrimPrefix(name, "/")
	if err := workspace.ValidSharePath(p); err != nil {
		return "", fmt.Errorf("unions: %w", err)
	}
	if p == "/" {
		return ".", nil
	}
	return p[1:], nil
}
