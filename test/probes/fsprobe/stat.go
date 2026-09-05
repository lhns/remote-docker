//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// probe is one run of the transcript: the output, the inode labels (per run)
// and the mtimes seen so far (per group and path).
type probe struct {
	out   io.Writer
	root  string // DIR/fsprobe
	large bool

	mu     sync.Mutex
	inos   map[[2]uint64]int
	mtimes map[string]unix.Timespec
}

// group is one running group: its directory and name.
type group struct {
	p    *probe
	name string
	dir  string
}

// step is the context handed to one step function: it collects the optional
// stat suffix and knows which group it prints under.
type step struct {
	g    *group
	stat string
}

// resultOf turns an error into the transcript's <result> field.
func resultOf(err error) string {
	if err == nil {
		return "ok"
	}
	var errno unix.Errno
	if errors.As(err, &errno) {
		if name := unix.ErrnoName(errno); name != "" {
			return name
		}
		return fmt.Sprintf("E%d", int(errno))
	}
	msg := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return "ERR:" + msg
}

// run executes one step and prints its line. A panic inside f is printed as
// the result and the run continues. value is the `ok:<value>` payload, empty
// for a bare `ok`; err wins over value.
func (g *group) run(name, op string, f func(s *step) (value string, err error)) {
	s := &step{g: g}
	var result string
	func() {
		defer func() {
			if r := recover(); r != nil {
				result = fmt.Sprintf("PANIC:%v", r)
			}
		}()
		value, err := f(s)
		result = resultOf(err)
		if err == nil && value != "" {
			result = "ok:" + value
		}
	}()
	line := fmt.Sprintf("%s/%s: %s -> %s", g.name, name, op, result)
	if s.stat != "" {
		line += " " + s.stat
	}
	_, _ = fmt.Fprintln(g.p.out, line)
}

// path resolves a name relative to the group directory.
func (g *group) path(name string) string { return filepath.Join(g.dir, name) }

// Stat records the stat of name (relative to the group directory) as the
// line's suffix. A failing stat is reported as `[stat:<errno>]`.
func (s *step) Stat(name string) {
	var st unix.Stat_t
	s.record(name, unix.Stat(s.g.path(name), &st), &st)
}

// Lstat is Stat without following a final symlink.
func (s *step) Lstat(name string) {
	var st unix.Stat_t
	s.record(name, unix.Lstat(s.g.path(name), &st), &st)
}

// Fstat is Stat through an open descriptor; name is the label the mtime is
// tracked under.
func (s *step) Fstat(fd int, name string) {
	var st unix.Stat_t
	s.record(name, unix.Fstat(fd, &st), &st)
}

func (s *step) record(name string, err error, st *unix.Stat_t) {
	if err != nil {
		s.stat = "[stat:" + resultOf(err) + "]"
		return
	}
	s.stat = s.g.p.format(s.g.name+"/"+name, st)
}

// format normalises a stat: no device, the inode as a label assigned on first
// sight in this run, and the mtime relative to the previous stat of the same
// path in the same group.
func (p *probe) format(key string, st *unix.Stat_t) string {
	label := p.label(st)

	p.mu.Lock()
	defer p.mu.Unlock()
	mtime := "new"
	if prev, ok := p.mtimes[key]; ok {
		switch {
		case st.Mtim == prev:
			mtime = "same"
		case st.Mtim.Sec > prev.Sec || (st.Mtim.Sec == prev.Sec && st.Mtim.Nsec > prev.Nsec):
			mtime = "advanced"
		default:
			mtime = "older"
		}
	}
	p.mtimes[key] = st.Mtim

	return fmt.Sprintf("[%s ino#%d nlink=%d size=%d mode=%04o uid=%d gid=%d mtime=%s]",
		fileType(st.Mode), label, uint64(st.Nlink), st.Size, st.Mode&0o7777, st.Uid, st.Gid, mtime) //nolint:unconvert // Nlink's width differs per arch
}

// label returns the inode label of a stat, assigning one on first sight, for
// same-ino/different-ino answers.
func (p *probe) label(st *unix.Stat_t) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	ino := [2]uint64{uint64(st.Dev), st.Ino} //nolint:unconvert // Dev's width differs per arch
	label, ok := p.inos[ino]
	if !ok {
		label = len(p.inos) + 1
		p.inos[ino] = label
	}
	return label
}

func fileType(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return "file"
	case unix.S_IFDIR:
		return "dir"
	case unix.S_IFLNK:
		return "symlink"
	default:
		return "other"
	}
}

// sameIno stats two paths and answers same-ino/different-ino by label.
func (g *group) sameIno(a, b string) (string, error) {
	var sa, sb unix.Stat_t
	if err := unix.Stat(g.path(a), &sa); err != nil {
		return "", err
	}
	if err := unix.Stat(g.path(b), &sb); err != nil {
		return "", err
	}
	if g.p.label(&sa) == g.p.label(&sb) {
		return "same-ino", nil
	}
	return "different-ino", nil
}

// run is the whole transcript: every group, or the named ones, each in a
// fresh subdirectory of dir/fsprobe, removed at the end.
func run(dir string, names []string, large bool, out io.Writer) error {
	if len(names) == 0 {
		for _, gr := range groups {
			names = append(names, gr.name)
		}
	}
	p := &probe{
		out:    out,
		root:   filepath.Join(dir, "fsprobe"),
		large:  large,
		inos:   map[[2]uint64]int{},
		mtimes: map[string]unix.Timespec{},
	}
	defer func() { _ = os.RemoveAll(p.root) }()

	for _, name := range names {
		var found bool
		for _, gr := range groups {
			if gr.name != name {
				continue
			}
			found = true
			g := &group{p: p, name: name, dir: filepath.Join(p.root, name)}
			_ = os.RemoveAll(g.dir)
			if err := os.MkdirAll(g.dir, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", name, err)
			}
			gr.run(g)
			_ = os.RemoveAll(g.dir)
		}
		if !found {
			return fmt.Errorf("unknown group %q", name)
		}
	}
	return nil
}
