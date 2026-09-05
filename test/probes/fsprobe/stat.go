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

// probe is one run of the transcript: the output, the inode labels and the
// mtimes seen so far (both per group, the mtimes keyed by path). Labels are
// assigned in order of first sight, so they are reset at the start of every
// group: one extra file in an early group must not shift every label after it.
type probe struct {
	out   io.Writer
	root  string // DIR/fsprobe
	large bool

	mu     sync.Mutex
	inos   map[[2]uint64]int
	mtimes map[string]unix.Timespec
}

// group is one running group: its directory, its name and the two things its
// steps print differently. owner is the whole group opting into the mode, uid
// and gid fields; noIno is it opting out of the inode label. Both come from the
// groups table, so nothing reads a group's name to decide what to print.
type group struct {
	p     *probe
	name  string
	dir   string
	owner bool
	noIno bool
}

// step is the context handed to one step function: it collects the optional
// stat suffix and knows which group it prints under. owner is whether its stat
// prints mode, uid and gid; see format.
type step struct {
	g     *group
	stat  string
	owner bool
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

// runOwner is run for a step whose stat prints mode, uid and gid where its
// group otherwise would not. One step does: create/create, so that a fresh
// file's ownership and mode are in the transcript once outside the attrs group.
func (g *group) runOwner(name, op string, f func(s *step) (value string, err error)) {
	g.run(name, op, func(s *step) (string, error) {
		s.owner = true
		return f(s)
	})
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
	s.stat = s.format(s.g.name+"/"+name, st)
}

// format normalises a stat: no device, the inode as a label assigned on first
// sight in this group, and the mtime relative to the previous stat of the same
// path in the same group.
//
// mode, uid and gid are printed only where the step asked for them, which is
// the attrs group and create/create. A share reports the workspace account as
// owner with wide bits (ADR 0046), so printing them everywhere puts one
// deliberate policy on most lines of the transcript and buries every other
// finding under it; printed once per group that is about them, it is still
// compared. The inode label is dropped where the group's steps are independent
// of one another (names), because there it numbers nothing a reader can use and
// drifts as soon as one host refuses a name another accepts.
//
// The mtime is reported as new, older or not-older, and NOT as same/advanced.
// A filesystem's timestamp granularity is coarse (ext4 stamps with a jiffy),
// so two mutations a few hundred microseconds apart may or may not land in the
// same tick: `same` and `advanced` are then the same run twice giving different
// transcripts. Where a write bumping the mtime is the thing being measured, the
// step separates the two by more than a second itself and reports the answer as
// its own value (`attrs/mtime-on-write.write`, `workload/tar.touch`).
func (s *step) format(key string, st *unix.Stat_t) string {
	p := s.g.p

	var b strings.Builder
	b.WriteString("[" + fileType(st.Mode))
	if !s.g.noIno {
		fmt.Fprintf(&b, " ino#%d", p.label(st))
	}
	fmt.Fprintf(&b, " nlink=%d size=%d", uint64(st.Nlink), st.Size) //nolint:unconvert // Nlink's width differs per arch
	if s.owner {
		fmt.Fprintf(&b, " mode=%04o uid=%d gid=%d", st.Mode&0o7777, st.Uid, st.Gid)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	mtime := "new"
	if prev, ok := p.mtimes[key]; ok {
		mtime = "not-older"
		if st.Mtim.Sec < prev.Sec || (st.Mtim.Sec == prev.Sec && st.Mtim.Nsec < prev.Nsec) {
			mtime = "older"
		}
	}
	p.mtimes[key] = st.Mtim

	fmt.Fprintf(&b, " mtime=%s]", mtime)
	return b.String()
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
			g := &group{p: p, name: name, dir: filepath.Join(p.root, name), owner: gr.owner, noIno: gr.noIno}
			p.inos = map[[2]uint64]int{} // labels are per group; see probe
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
