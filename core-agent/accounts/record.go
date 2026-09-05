package accounts

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// The state directory's records, one line per entry, readable with `cat`:
// uidmap, clientports and the workspace id all have this shape.

// ReadRecord hands every entry of a record to parse, skipping blank lines and
// comments. A record that does not exist is empty rather than an error.
func ReadRecord(path string, parse func(line string)) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parse(line)
	}
	return scanner.Err()
}

// WriteRecord replaces a record atomically, through a temporary file in the
// same directory and a rename.
//
// Never in place: each of these records is the durable half of something a
// running workspace derives, so one truncated by a crash mid-write costs more
// than the write. A truncated uidmap reallocates uids on the next start, which
// changes every account's port and orphans the ownership of everything on disk;
// a truncated workspace id adopts no daemon and orphans every one of them.
func WriteRecord(path string, lines []string, mode os.FileMode) error {
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}
