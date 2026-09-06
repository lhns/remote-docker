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

// probe is one run of the transcript: the output and the inode labels seen so
// far. Labels are assigned in order of first sight and reset at the start of
// every group: one extra file in an early group must not shift every label
// after it.
type probe struct {
	out  io.Writer
	root string // DIR/fsprobe

	mu   sync.Mutex
	inos map[[2]uint64]int
}

// group is one running group: its directory, its name and the two stat fields
// its steps print where other groups do not. mode is the group opting into the
// mode word (attrs), nlink into the link count (links). Both come from the
// groups table, so nothing reads a group's name to decide what to print.
type group struct {
	p     *probe
	name  string
	dir   string
	mode  bool
	nlink bool
}

// step is the context handed to one step function: it collects the optional
// stat suffix and knows which group it prints under.
type step struct {
	g    *group
	stat string

	// owner adds uid and gid (and the mode) to this step's stat; see format.
	owner bool

	// noIno drops the inode label for one step whose own value already
	// reports the identity: a recreated name may or may not reuse the inode,
	// and the label would differ between two runs on one filesystem.
	noIno bool
}

// result is an error carrying its own transcript text, where a step knows more
// than the bare errno: `EINVAL at create` names which call in a sequence
// refused.
type result string

func (r result) Error() string { return string(r) }

// resultOf turns an error into the transcript's <result> field.
func resultOf(err error) string {
	if err == nil {
		return "ok"
	}
	var own result
	if errors.As(err, &own) {
		return string(own)
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

// runOwner is run for a step whose stat prints uid and gid as well as the
// mode. Two do: create/create and attrs/create, so that a freshly created
// file's ownership is in the transcript once per group that is about it.
func (g *group) runOwner(name, op string, f func(s *step) (value string, err error)) {
	g.run(name, op, func(s *step) (string, error) {
		s.owner = true
		return f(s)
	})
}

func (g *group) path(name string) string { return filepath.Join(g.dir, name) }

// Stat records the stat of name (relative to the group directory) as the
// line's suffix. A failing stat is reported as `[stat:<errno>]`.
func (s *step) Stat(name string) {
	var st unix.Stat_t
	s.record(unix.Stat(s.g.path(name), &st), &st)
}

// Lstat is Stat without following a final symlink.
func (s *step) Lstat(name string) {
	var st unix.Stat_t
	s.record(unix.Lstat(s.g.path(name), &st), &st)
}

// Fstat is Stat through an open descriptor.
func (s *step) Fstat(fd int) {
	var st unix.Stat_t
	s.record(unix.Fstat(fd, &st), &st)
}

func (s *step) record(err error, st *unix.Stat_t) {
	if err != nil {
		s.stat = "[stat:" + resultOf(err) + "]"
		return
	}
	s.stat = s.format(st)
}

// format prints only the fields that carry a filesystem difference: no device,
// no timestamp, the inode as a label assigned on first sight in this group, and
// nothing for a directory beyond its type, because a directory's size and link
// count are the host filesystem's (ext4 says size=4096 nlink=2, xfs and overlay
// say otherwise) and would invalidate every directory line at once. mode, uid,
// gid and nlink are printed only where the group or the step asked for them;
// see the group and step structs.
func (s *step) format(st *unix.Stat_t) string {
	var b strings.Builder
	typ := fileType(st.Mode)
	b.WriteString("[" + typ)
	if !s.noIno {
		fmt.Fprintf(&b, " ino#%d", s.g.p.label(st))
	}
	if typ != "dir" {
		if s.g.nlink {
			fmt.Fprintf(&b, " nlink=%d", uint64(st.Nlink)) //nolint:unconvert // Nlink's width differs per arch
		}
		fmt.Fprintf(&b, " size=%d", st.Size)
	}
	switch {
	case s.owner:
		fmt.Fprintf(&b, " mode=%04o uid=%d gid=%d", st.Mode&0o7777, st.Uid, st.Gid)
	case s.g.mode:
		fmt.Fprintf(&b, " mode=%04o", st.Mode&0o7777)
	}
	b.WriteString("]")
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
func run(dir string, names []string, out io.Writer) error {
	if len(names) == 0 {
		for _, gr := range groups {
			names = append(names, gr.name)
		}
	}
	p := &probe{out: out, root: filepath.Join(dir, "fsprobe")}
	defer func() { _ = os.RemoveAll(p.root) }()

	for _, name := range names {
		var found bool
		for _, gr := range groups {
			if gr.name != name {
				continue
			}
			found = true
			g := &group{p: p, name: name, dir: filepath.Join(p.root, name), mode: gr.mode, nlink: gr.nlink}
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
