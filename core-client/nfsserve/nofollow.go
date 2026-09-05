package nfsserve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/go-git/go-billy/v5"
)

// noFollowFS makes REMOVE and RENAME act on a symlink rather than on what it
// points at.
//
// go-billy's BoundOS resolves a path with securejoin.SecureJoin, which follows
// every component INCLUDING the last, so `rm a` where `a -> b` unlinked b and
// left a dangling, and `mv a c` moved b. Native unlink(2) and rename(2) touch
// the link itself, which is what a container expects and what `rm -rf` of a
// tree with links in it depends on: the alternative deletes files the link was
// only naming.
//
// Only the parent directory is resolved here, and it still goes through
// SecureJoin, so a `..` or a symlink in the directory part cannot leave the
// share. The last element is required to be a single plain name, so nothing
// it holds can be a path either. Every other operation is the bound osfs's.
//
// It is also where every NEW name passes, being the layer directly on the
// osfs, so it is where a name the host could create but never delete is
// refused (checkNewName, which only Windows has a rule for).
type noFollowFS struct {
	billy.Filesystem // a bound osfs, whose Root is the share directory
}

func (n *noFollowFS) Create(name string) (billy.File, error) {
	if err := checkNewName(n.base(name)); err != nil {
		return nil, err
	}
	return n.Filesystem.Create(name)
}

func (n *noFollowFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&os.O_CREATE != 0 {
		if err := checkNewName(n.base(name)); err != nil {
			return nil, err
		}
	}
	return n.Filesystem.OpenFile(name, flag, perm)
}

// MkdirAll checks every component: which of them are new is not known without
// a stat each, and one that exists with a name the rule refuses cannot have
// been made through here, so refusing to descend into it costs nothing real.
func (n *noFollowFS) MkdirAll(name string, perm os.FileMode) error {
	for _, c := range strings.Split(filepath.ToSlash(n.relative(name)), "/") {
		if err := checkNewName(c); err != nil {
			return err
		}
	}
	return n.Filesystem.MkdirAll(name, perm)
}

func (n *noFollowFS) Symlink(target, link string) error {
	if err := checkNewName(n.base(link)); err != nil {
		return err
	}
	return n.Filesystem.Symlink(target, link)
}

func (n *noFollowFS) Remove(name string) error {
	p, err := n.leaf(name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

func (n *noFollowFS) Rename(from, to string) error {
	if err := checkNewName(n.base(to)); err != nil {
		return err
	}
	f, err := n.leaf(from)
	if err != nil {
		return err
	}
	t, err := n.leaf(to)
	if err != nil {
		return err
	}
	return os.Rename(f, t)
}

// Chroot re-wraps, or a subdirectory mount would drop back to the following
// behaviour.
func (n *noFollowFS) Chroot(p string) (billy.Filesystem, error) {
	inner, err := n.Filesystem.Chroot(p)
	if err != nil {
		return nil, err
	}
	return &noFollowFS{Filesystem: inner}, nil
}

// relative is a name as the share sees it: go-nfs joins the components of a
// handle, so names arrive relative, and an absolute one is made relative to
// the root when it is a spelling of a path inside the share, which is how osfs
// treats it too. One outside is returned as it came, for the caller to refuse.
func (n *noFollowFS) relative(name string) string {
	name = filepath.FromSlash(name)
	if !filepath.IsAbs(name) {
		return name
	}
	if rel, err := filepath.Rel(n.Root(), name); err == nil && !leavesRoot(rel) {
		return rel
	}
	return name
}

// base is the last element of a name, the one a create or a rename makes.
func (n *noFollowFS) base(name string) string {
	return filepath.Base(n.relative(name))
}

// leaf turns a share-relative name into an OS path whose directory part is
// resolved inside the share and whose last element is not resolved at all.
func (n *noFollowFS) leaf(name string) (string, error) {
	root := n.Root()
	name = n.relative(name)
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("nfsserve: %q is outside the share", name)
	}

	base := filepath.Base(name)
	if base == "." || base == ".." || strings.ContainsAny(base, `/\`) {
		return "", fmt.Errorf("nfsserve: %q does not name an entry in a directory", name)
	}

	dir, err := securejoin.SecureJoin(root, filepath.Dir(name))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, base), nil
}
