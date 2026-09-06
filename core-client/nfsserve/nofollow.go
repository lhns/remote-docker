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
// go-billy's BoundOS resolves the last component too, so `rm a` where `a -> b`
// unlinked b and left a dangling, and `mv a c` moved b: files deleted for
// removing a name that only pointed at them. Containment is secureLeaf's;
// every other operation is the bound osfs's.
//
// It is also the layer directly on the osfs, so it is where every NEW name
// passes and where one the host could create but never delete is refused
// (checkNewName, which only Windows has a rule for).
type noFollowFS struct {
	billy.Filesystem // a bound osfs, whose Root is the share directory
}

// Stat and Lstat refuse a name the host cannot spell, so a lookup of `nul`
// finds the DOS device rather than nothing: without this the probe's create of
// `nul` answers EEXIST from a device that then cannot be unlinked (EIO), where
// the same name is refused on the create path. checkNewName is a no-op off
// Windows, so everywhere else these two delegate and nothing more.
func (n *noFollowFS) Stat(name string) (os.FileInfo, error) {
	if n.unspellable(name) {
		return nil, os.ErrNotExist
	}
	return n.Filesystem.Stat(name)
}

func (n *noFollowFS) Lstat(name string) (os.FileInfo, error) {
	if n.unspellable(name) {
		return nil, os.ErrNotExist
	}
	return n.Filesystem.Lstat(name)
}

// unspellable reports whether a lookup names something the host cannot spell.
// The share root and the two directory names have no base of their own and are
// always spellable; everything else is the create-side rule.
func (n *noFollowFS) unspellable(name string) bool {
	base := n.base(name)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return false
	}
	return checkNewName(base) != nil
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

func (n *noFollowFS) relative(name string) string { return shareRelative(n.Root(), name) }

func (n *noFollowFS) base(name string) string { return shareBase(n.Root(), name) }

func (n *noFollowFS) leaf(name string) (string, error) { return secureLeaf(n.Root(), name) }

// shareRelative is a name as the share sees it: go-nfs joins the components of
// a handle, so names arrive relative, and an absolute one is made relative to
// the root when it is a spelling of a path inside the share, which is how osfs
// treats it too. One outside is returned as it came, for the caller to refuse.
func shareRelative(root, name string) string {
	name = filepath.FromSlash(name)
	if !filepath.IsAbs(name) {
		return name
	}
	if rel, err := filepath.Rel(root, name); err == nil && !leavesRoot(rel) {
		return rel
	}
	return name
}

// shareBase is the last element of a name, the one a create or a rename makes.
func shareBase(root, name string) string {
	return filepath.Base(shareRelative(root, name))
}

// secureLeaf turns a share-relative name into an OS path whose directory part
// is resolved inside the share and whose last element is not resolved at all.
//
// It is the one containment answer in this package: noFollowFS's REMOVE and
// RENAME and attrChange's CHMOD and LINK all reach the disk through it, so a
// `..` or a symlink in the directory part resolves back inside the share
// rather than out of it. A lexical join cannot do that job: `escape/x` where
// `escape -> /etc` looks contained and is not.
func secureLeaf(root, name string) (string, error) {
	name = shareRelative(root, name)
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("nfsserve: %q is outside the share", name)
	}

	// filepath.Separator rather than both slashes: off Windows a `\` is an
	// ordinary character in a filename, which archives and Windows-authored
	// repositories produce, and `a\b` has to stay removable.
	base := filepath.Base(name)
	if base == "." || base == ".." || strings.ContainsRune(base, filepath.Separator) {
		return "", fmt.Errorf("nfsserve: %q does not name an entry in a directory", name)
	}

	dir, err := securejoin.SecureJoin(root, filepath.Dir(name))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, base), nil
}
