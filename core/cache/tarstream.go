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
// bad file costs itself and not every file asked for beside it. A file that has
// gone, or is no longer a regular file, is left out silently as before: the
// next listing reports what it is now.
//
// Each file is read whole before its header is written, since a header once out
// has to be followed by that many bytes; a read failing half way would leave a
// choice between NULs, which are a wrong file, and failing the archive.
func WriteTarNaming(open func(string) (*os.File, error), files []TarFile, w io.Writer) ([]Unreadable, error) {
	tw := tar.NewWriter(w)
	var unreadable []Unreadable

	for _, file := range files {
		data, info, err := readWhole(open, file.Path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				unreadable = append(unreadable, Unreadable{Name: file.Name, Err: err})
			}
			continue
		}
		if info == nil {
			continue
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			unreadable = append(unreadable, Unreadable{Name: file.Name, Err: err})
			continue
		}
		header.Name = file.Name
		header.Size = int64(len(data))
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""
		if err := tw.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("workspace: writing the header for %s: %w", file.Name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("workspace: writing %s: %w", file.Name, err)
		}
	}
	return unreadable, tw.Close()
}

// readWhole is a regular file's bytes, at most its size when opened, and its
// info; a nil info with no error is a path that is no longer a regular file.
func readWhole(open func(string) (*os.File, error), path string) ([]byte, os.FileInfo, error) {
	f, err := open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, info.Size()))
	if err != nil {
		return nil, nil, err
	}
	return data, info, nil
}
