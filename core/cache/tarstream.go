package cache

// Building the tar that carries a cache's files, in the one place both ends
// agree on it (ADR 0044).
//
// Here rather than in either module because the two directions must produce the
// SAME thing: write-back compares a change's modification time against the
// baseline the fill recorded, so "the mtime survives the round trip, to the
// second" is behaviour both ends depend on. It was asserted independently in two modules,
// which is the drift ADR 0021 exists to prevent.
//
// The stream only. Extracting is deliberately NOT shared: the agent creates
// directories and replaces symlinks, and the client refuses everything but a
// regular file because it is writing into somebody's source tree.

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TarFile is one entry to carry: the name it takes in the archive, and where to
// read it from on this machine.
type TarFile struct {
	// Name is the path inside the share, without a leading slash.
	Name string

	// Path is where the bytes are, on whichever side is building.
	Path string
}

// TarFilesFrom pairs share-relative names with their location under root.
func TarFilesFrom(root string, names []string) []TarFile {
	out := make([]TarFile, 0, len(names))
	for _, n := range names {
		name := strings.TrimPrefix(n, "/")
		out = append(out, TarFile{
			Name: name,
			Path: filepath.Join(root, filepath.FromSlash(name)),
		})
	}
	return out
}

// Read is one file as a tar carries it: its size and modification time when it
// was opened, which is the version the entry holds. The entry's own time is
// this one rounded to the second (archive/tar does that unless a format is
// chosen), so a stat taken afterwards may describe a version the tar never saw.
type Read struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// WriteTar writes the files as a tar, and answers with each file it carried.
//
// A file that cannot be opened or stat'ed, or that is not regular, is SKIPPED
// rather than failing the archive. On the client that is a file locked by
// another process, which is ordinary on Windows; on the agent it is a file the
// container removed between being listed and being read. Either way one absent
// file is better than a batch nobody gets.
func WriteTar(files []TarFile, w io.Writer) ([]Read, error) {
	return WriteTarFrom(os.Open, files, w)
}

// WriteTarFrom is WriteTar opening each Path with open, for a caller that must
// not follow a symlink out of a directory: the agent passes os.Root.Open.
func WriteTarFrom(open func(string) (*os.File, error), files []TarFile, w io.Writer) ([]Read, error) {
	tw := tar.NewWriter(w)
	read := make([]Read, 0, len(files))

	for _, file := range files {
		// Opened BEFORE its header is written. Once the header is out the entry
		// has to be filled with something, and what it would be filled with is
		// NULs: a file that is there and is wrong.
		f, err := open(file.Path)
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
		header.Name = file.Name
		// Ownership is the sending machine's and means nothing on the other
		// side, where the account's uid is what the files must belong to.
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""

		if err := tw.WriteHeader(header); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("workspace: writing the header for %s: %w", file.Name, err)
		}
		written, err := io.Copy(tw, io.LimitReader(f, header.Size))
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("workspace: writing %s: %w", file.Name, err)
		}
		// A file that shrank while it was read leaves the entry short, which
		// the writer reports on the NEXT header rather than here. Pad instead:
		// a truncated file in a cache is served correctly again once it is
		// invalidated, and a failed batch costs every file in it.
		if pad := header.Size - written; pad > 0 {
			if _, err := tw.Write(make([]byte, pad)); err != nil {
				return nil, fmt.Errorf("workspace: padding %s: %w", file.Name, err)
			}
		}
		read = append(read, Read{Name: file.Name, Size: info.Size(), ModTime: info.ModTime()})
	}

	return read, tw.Close()
}

// Unreadable is one file WriteTarNaming left out, and why.
type Unreadable struct {
	Name string
	Err  error
}

// WriteTarNaming is WriteTarFrom for a pull under Carry: a file that is there
// and cannot be read is left out and NAMED, never failing the archive, so one
// bad file does not starve every file asked for beside it. A file that is gone
// is the ordinary case and is not named.
//
// The archive goes to a file, not a buffer, so a large file is never held in
// memory: each entry is copied by the size its header declares, and one that
// fails to read part way is cut back out by truncating to where its entry
// began. That is why it needs a file and not a writer.
func WriteTarNaming(open func(string) (*os.File, error), files []TarFile, spool *os.File) ([]Unreadable, error) {
	var unreadable []Unreadable

	for _, file := range files {
		start, err := spool.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, fmt.Errorf("workspace: positioning the archive: %w", err)
		}
		err = writeEntry(open, file, spool)
		if err == nil {
			continue
		}
		var skip *skipError
		if !errors.As(err, &skip) {
			return nil, err
		}
		if err := spool.Truncate(start); err != nil {
			return nil, fmt.Errorf("workspace: cutting %s from the archive: %w", file.Name, err)
		}
		if _, err := spool.Seek(start, io.SeekStart); err != nil {
			return nil, fmt.Errorf("workspace: positioning the archive: %w", err)
		}
		if skip.err != nil && !errors.Is(skip.err, os.ErrNotExist) {
			unreadable = append(unreadable, Unreadable{Name: file.Name, Err: skip.err})
		}
	}

	// The end of an archive is two empty blocks, which a writer with no
	// entries closes into.
	return unreadable, tar.NewWriter(spool).Close()
}

// skipError is a file left out of the archive, as opposed to an archive that
// cannot be written. err is nil for one that is simply not a regular file.
type skipError struct{ err error }

func (e *skipError) Error() string { return fmt.Sprint("skipped: ", e.err) }

// writeEntry appends one regular file as a whole tar entry: header, body and
// padding. Opened before its header is written, for the reason in WriteTarFrom.
func writeEntry(open func(string) (*os.File, error), file TarFile, w io.Writer) error {
	f, err := open(file.Path)
	if err != nil {
		return &skipError{err}
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return &skipError{err}
	}
	if !info.Mode().IsRegular() {
		return &skipError{}
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return &skipError{err}
	}
	header.Name = file.Name
	header.Uid, header.Gid = 0, 0
	header.Uname, header.Gname = "", ""

	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("workspace: writing the header for %s: %w", file.Name, err)
	}
	// A failure reading the file is the file's, and can be cut out and carried
	// on from; a failure writing the archive is everyone's.
	body := &sourceReader{r: io.LimitReader(f, header.Size)}
	written, err := io.Copy(tw, body)
	if err != nil {
		if body.err != nil {
			return &skipError{body.err}
		}
		return fmt.Errorf("workspace: writing %s: %w", file.Name, err)
	}
	// A file that shrank while it was read: padded, as WriteTarFrom does.
	if pad := header.Size - written; pad > 0 {
		if _, err := tw.Write(make([]byte, pad)); err != nil {
			return fmt.Errorf("workspace: padding %s: %w", file.Name, err)
		}
	}
	// Pads the entry to its block; the trailer is the caller's.
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("workspace: writing %s: %w", file.Name, err)
	}
	return nil
}

// sourceReader remembers the error its reader returned, so a failed copy can be
// told from a failed write.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return n, err
}
