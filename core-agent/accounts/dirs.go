package accounts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CheckDirs refuses keys directories Sync could not read unambiguously: one
// named twice, or an enrolled directory that is, contains or sits inside an
// operator one, where the agent would write into the operator's files or read
// its own as the operator's (ADR 0052).
func CheckDirs(operator []string, enrolled string) error {
	seen := map[string]string{}
	for _, dir := range operator {
		r := resolve(dir)
		if other, dup := seen[r]; dup {
			return fmt.Errorf("the keys directory %s is named twice (as %s and %s)", r, other, dir)
		}
		seen[r] = dir
	}
	if enrolled == "" {
		return nil
	}
	e := resolve(enrolled)
	for _, dir := range operator {
		o := resolve(dir)
		switch {
		case e == o:
			return fmt.Errorf("the enrolled keys directory %s is also an operator keys directory", enrolled)
		case within(e, o):
			return fmt.Errorf("the enrolled keys directory %s is inside the operator keys directory %s", enrolled, dir)
		case within(o, e):
			return fmt.Errorf("the enrolled keys directory %s contains the operator keys directory %s", enrolled, dir)
		}
	}
	return nil
}

// resolve is a directory's one spelling: absolute, clean, and through its
// symlinks, or its parent's for a directory not created yet.
func resolve(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(parent, filepath.Base(abs))
	}
	return abs
}

// within reports whether path sits strictly inside dir.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CheckWritable reports whether the enrolled directory can take a key, by
// creating and removing a file in it.
func (s *Store) CheckWritable() error {
	if s.EnrolledDir == "" {
		return errNoEnrolledDir
	}
	f, err := os.CreateTemp(s.EnrolledDir, ".probe-*")
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", s.EnrolledDir, err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}
