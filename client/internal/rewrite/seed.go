package rewrite

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Reading a local tree as a tar stream, for the delegated consistency
// (ADR 0043).
//
// The client reads its own disk sequentially, which is the fastest path there
// is, and the result goes to the workspace as one stream rather than as a file
// per round trip. That is the whole reason delegated is faster than any mount
// can be.

// tarTree streams the tree at root as a tar, rooted at the base name the
// caller will extract under.
//
// Streamed rather than built: a project is not something to hold in memory,
// and the reader is the request body, so the walk runs exactly as fast as the
// connection drains it. The error, if any, arrives at the reader as a read
// error, which is what makes a half-written seed a failed one.
func tarTree(root string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeTar(pw, root))
	}()
	return pr
}

// writeTar walks root and writes every regular file, directory and symlink
// under it.
//
// What is skipped is what a tar cannot carry to another machine: sockets,
// devices and named pipes are NAMES on a filesystem rather than objects that
// travel, which is the same reason the NFS export refuses a socket (ADR 0039).
// Skipping is right rather than failing: one socket in a project directory --
// and a running dev server leaves one -- must not stop the whole tree.
func writeTar(w io.Writer, root string) error {
	tw := tar.NewWriter(w)

	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("rewrite: reading %s: %w", root, err)
	}
	if !info.IsDir() {
		if err := writeEntry(tw, root, filepath.Base(root), info); err != nil {
			return err
		}
		return tw.Close()
	}

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that cannot be read is reported and stepped over:
			// the alternative is that one unreadable path in a project stops
			// the container from starting at all.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name, ok := relativeName(root, p)
		if !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		return writeEntry(tw, p, name, info)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// relativeName is the name an entry gets inside the tar: slash-separated,
// relative to the root, and never the root itself.
//
// Slashes by hand rather than filepath.ToSlash alone, because a tar name is
// always "/"-separated whatever the client's OS spells paths with.
func relativeName(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func writeEntry(tw *tar.Writer, p, name string, info fs.FileInfo) error {
	link := ""
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		if err != nil {
			return nil
		}
		link = filepath.ToSlash(target)
	}

	// Opened BEFORE the header is written, so a file this machine cannot read
	// -- no permission, or locked by another process, which is ordinary on
	// Windows -- is simply absent from the copy. Writing the header first
	// meant the entry had to be filled with something, and what it was filled
	// with was NULs: a file that is there and is wrong.
	var f *os.File
	if info.Mode().IsRegular() {
		opened, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer func() { _ = opened.Close() }()
		f = opened

		// The open file's own size, rather than the walk's: between the two a
		// build can have rewritten it, and a header that disagrees with what
		// follows is a corrupt stream rather than a stale file.
		if current, err := f.Stat(); err == nil {
			info = current
		}
	}

	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		// Not a file type tar can describe: a socket, a device, a pipe.
		return nil
	}
	header.Name = name
	if info.IsDir() {
		header.Name = name + "/"
	}
	// Ownership is the client's and means nothing on the workspace, where the
	// account's uid is what the files must belong to. Extraction assigns that;
	// carrying a Windows-shaped uid would only be noise.
	header.Uid, header.Gid = 0, 0
	header.Uname, header.Gname = "", ""

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("rewrite: writing %s to the seed: %w", name, err)
	}
	if f == nil {
		return nil
	}

	written, err := io.Copy(tw, io.LimitReader(f, header.Size))
	if err != nil {
		return fmt.Errorf("rewrite: reading %s: %w", p, err)
	}
	// A file that shrank while it was being read leaves the entry short, which
	// the tar writer reports on the next header. Pad it rather than fail: the
	// copy has a truncated file, and the alternative is no copy at all.
	return padEntry(tw, header.Size-written)
}

func padEntry(tw *tar.Writer, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := io.CopyN(tw, zeroes{}, n)
	return err
}

// zeroes is an endless run of NULs, for padding an entry whose file shrank.
type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
