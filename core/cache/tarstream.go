package cache

// Building the tar that carries a cache's files, in the one place both ends
// agree on it (ADR 0044).
//
// Here rather than in either module because the two directions must produce the
// SAME thing: write-back compares a change's modification time against the
// baseline the fill recorded, so "the mtime survives the round trip" is
// behaviour both ends depend on. It was asserted independently in two modules,
// which is the drift ADR 0021 exists to prevent.
//
// The stream only. Extracting is deliberately NOT shared: the agent creates
// directories and replaces symlinks, and the client refuses everything but a
// regular file because it is writing into somebody's source tree.

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// WriteTar writes the files as a tar.
//
// A file that cannot be opened or stat'ed, or that is not regular, is SKIPPED
// rather than failing the archive. On the client that is a file locked by
// another process, which is ordinary on Windows; on the agent it is a file the
// container removed between being listed and being read. Either way one absent
// file is better than a batch nobody gets.
func WriteTar(files []TarFile, w io.Writer) error {
	tw := tar.NewWriter(w)

	for _, file := range files {
		// Opened BEFORE its header is written. Once the header is out the entry
		// has to be filled with something, and what it would be filled with is
		// NULs: a file that is there and is wrong.
		f, err := os.Open(file.Path)
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
			return fmt.Errorf("workspace: writing the header for %s: %w", file.Name, err)
		}
		written, err := io.Copy(tw, io.LimitReader(f, header.Size))
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("workspace: writing %s: %w", file.Name, err)
		}
		// A file that shrank while it was read leaves the entry short, which
		// the writer reports on the NEXT header rather than here. Pad instead:
		// a truncated file in a cache is served correctly again once it is
		// invalidated, and a failed batch costs every file in it.
		if pad := header.Size - written; pad > 0 {
			if _, err := tw.Write(make([]byte, pad)); err != nil {
				return fmt.Errorf("workspace: padding %s: %w", file.Name, err)
			}
		}
	}

	return tw.Close()
}
