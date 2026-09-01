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

	"github.com/lhns/remote-docker/core-agent/notify"
	"github.com/lhns/remote-docker/core-agent/union"
	"github.com/lhns/remote-docker/core/workspace"
)

// Filling and emptying a share's cache, always THROUGH the merged mount.
//
// Never into the cache layer directly, and that is not a preference: overlayfs
// leaves the result undefined when a layer changes underneath a mounted union,
// and it was measured -- a file written straight into the cache stays invisible
// to a container that had already looked for it and missed
// (test/union-probe.sh section 4).
//
// Going through the union has a second consequence that is worth the whole
// feature: the write is a real filesystem operation in the container's own
// view, so its inotify fires natively. That is how ADR 0014 closes for these
// shares -- not as a poke that approximates an event, but as the event.
//
// The agent writes through /proc/<pid>/root, which resolves in the daemon's
// mount namespace without entering it, exactly as core-agent/notify already
// reaches a volume it cannot otherwise see.

// Apply extracts a tar into a share's union, decoding it first when the client
// compressed it.
//
// The codec is checked again here rather than trusted from Validate: a stream
// that names an encoding this version does not have must not be handed to the
// archive reader, which would fail somewhere inside it with a message about a
// corrupt header instead of about the codec.
func (m *Manager) Apply(ctx context.Context, account, export, codec string, body io.Reader) error {
	l, root, err := m.mergedRoot(ctx, account, export)
	if err != nil {
		// The payload still has to be drained, or the next frame is read out
		// of the middle of a tar. The caller cannot do it: only here is it
		// known that nothing consumed the body.
		_, _ = io.Copy(io.Discard, body)
		return err
	}

	body, done, err := decoded(codec, body)
	if err != nil {
		_, _ = io.Copy(io.Discard, body)
		return err
	}
	defer done()

	tr := tar.NewReader(body)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("unions: reading the batch for %s: %w", export, err)
		}
		info, err := writeEntry(root, header, tr)
		if err != nil {
			return err
		}
		if info != nil {
			// As it LANDED, not as it was asked for: a filesystem that keeps
			// coarser timestamps than the tar carries would otherwise make
			// this record match nothing, and every filled file would be
			// reported as a container write for the rest of the session.
			l.noteApplied("/"+strings.TrimPrefix(header.Name, "/"), info.Size(), info.ModTime())
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
func writeEntry(root string, header *tar.Header, body io.Reader) (os.FileInfo, error) {
	target, err := within(root, header.Name)
	if err != nil {
		return nil, err
	}

	switch header.Typeflag {
	case tar.TypeDir:
		return nil, os.MkdirAll(target, header.FileInfo().Mode().Perm())

	case tar.TypeSymlink:
		// Replaced rather than merged: a symlink that changed target is a
		// different link, and there is no way to edit one in place.
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("unions: replacing the link %s: %w", header.Name, err)
		}
		return nil, os.Symlink(header.Linkname, target)

	case tar.TypeReg:
		if err := os.MkdirAll(path.Dir(target), 0o755); err != nil {
			return nil, fmt.Errorf("unions: creating the directory for %s: %w", header.Name, err)
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, header.FileInfo().Mode().Perm())
		if err != nil {
			return nil, fmt.Errorf("unions: writing %s: %w", header.Name, err)
		}
		if _, err := io.Copy(f, body); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("unions: writing %s: %w", header.Name, err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("unions: writing %s: %w", header.Name, err)
		}
		// The client's own modification time, so the cache and the tree it
		// came from agree about when a file was last written. Write-back
		// compares those, and a file stamped with the moment it was copied
		// would look like something the container had just changed.
		if !header.ModTime.IsZero() {
			_ = os.Chtimes(target, time.Time{}, header.ModTime)
		}
		info, err := os.Stat(target)
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
	case workspace.CodecNone:
		return body, func() {}, nil

	case workspace.CodecZstd:
		zr, err := zstd.NewReader(body)
		if err != nil {
			return body, func() {}, fmt.Errorf("unions: reading a %s batch: %w", codec, err)
		}
		return zr, zr.Close, nil

	default:
		return body, func() {}, fmt.Errorf("unions: a batch arrived encoded as %q, which this workspace cannot read", codec)
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
func (m *Manager) Drop(ctx context.Context, account, export string, paths []string) error {
	l, root, err := m.mergedRoot(ctx, account, export)
	if err != nil {
		return err
	}

	for _, p := range paths {
		target, err := within(root, p)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("unions: dropping %s from %s: %w", p, export, err)
		}
		l.forgetApplied(p)
	}
	return nil
}

// mergedRoot is where a share's union can be written, as the AGENT can reach
// it, and it refuses a share that is not serving.
func (m *Manager) mergedRoot(ctx context.Context, account, export string) (*live, string, error) {
	m.mu.Lock()
	l, ok := m.shares[key(account, export)]
	m.mu.Unlock()

	if !ok {
		return nil, "", fmt.Errorf("unions: %s has no cache; prepare it first", export)
	}
	if err := union.Alive(ctx, l.spec); err != nil {
		return nil, "", err
	}

	root, err := notify.Relocate(l.spec.Merged(), func() (string, error) { return l.spec.Root(), nil })
	if err != nil {
		return nil, "", fmt.Errorf("unions: locating the cache for %s: %w", export, err)
	}
	return l, root, nil
}

// within resolves a path inside the share and refuses one that leaves it.
//
// The client validated it too, and that is not a reason to skip this: the
// stream tells a root process which files to write and which to remove. The
// check is on the RESULT rather than on the input, because path.Join CLEANS --
// "/a/../../etc" becomes "/etc" and looks like an ordinary path by the time
// anything opens it. Exactly the reasoning in notify.relocate.
func within(root, name string) (string, error) {
	if err := workspace.ValidSharePath("/" + strings.TrimPrefix(name, "/")); err != nil {
		return "", fmt.Errorf("unions: %w", err)
	}
	target := path.Join(root, name)
	if target != root && !strings.HasPrefix(target, strings.TrimSuffix(root, "/")+"/") {
		return "", fmt.Errorf("unions: %q leaves the share", name)
	}
	return target, nil
}
