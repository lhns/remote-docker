package unions

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"testing"
)

// The account is root in its own daemon and chooses every directory in it, so
// a path the daemon reports must not be resolved through a link it planted.
// An ABSOLUTE link in the middle of the path would otherwise resolve against
// the agent's root. These tests stand a temporary directory in for
// /proc/<pid>/root, and a "secret" outside it for what must stay out of reach.

const volumeDir = "/var/lib/docker/volumes/rd-cache/_data"

// daemonFS is a daemon root holding a cache volume, and a directory outside it.
type daemonFS struct {
	root, outside, data string
}

func newDaemonFS(t *testing.T) daemonFS {
	t.Helper()
	d := daemonFS{root: t.TempDir(), outside: t.TempDir()}
	d.data = filepath.Join(d.root, filepath.FromSlash(volumeDir))
	for _, sub := range []string{"upper", "merged"} {
		if err := os.MkdirAll(filepath.Join(d.data, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(d.outside, "upper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.outside, "upper", "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// replace swaps a path inside the daemon for an absolute symlink to target.
func (d daemonFS) replace(t *testing.T, inDaemon, target string) {
	t.Helper()
	p := filepath.Join(d.root, filepath.FromSlash(inDaemon))
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
}

// Each directory the account can swap for a link, at each depth.
func TestOpenInRefusesALinkAnywhereInTheDaemonsPath(t *testing.T) {
	for _, link := range []string{
		volumeDir + "/upper", // the leaf
		volumeDir,            // _data
		path.Dir(volumeDir),  // the volume's directory, an intermediate component
		"/var/lib/docker",    // further up
	} {
		t.Run(link, func(t *testing.T) {
			d := newDaemonFS(t)
			d.replace(t, link, d.outside)

			r, err := openAt(d.root, volumeDir, volumeDir+"/upper")
			if err == nil {
				_ = r.Close()
				t.Fatalf("a path through an absolute link at %s was opened", link)
			}
		})
	}
}

// An ordinary tree opens, so the refusals above are not bought by refusing
// everything.
func TestOpenInOpensAnOrdinaryDaemonPath(t *testing.T) {
	d := newDaemonFS(t)
	for _, p := range []string{volumeDir + "/upper", volumeDir + "/merged", volumeDir} {
		r, err := openAt(d.root, volumeDir, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		_ = r.Close()
	}
}

// Shared daemon: the path down to the cache volume is the operator's and is
// followed as it always was, a link in the data root included. What is under
// the volume is the account's and is contained.
func TestOpenInSharedModeFollowsTheDataRootButContainsTheVolume(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	real := filepath.Join(base, "data", "docker", "volumes", "rd-cache", "_data")
	if err := os.MkdirAll(filepath.Join(real, "upper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The data root reached through a link, as /var/lib/docker -> /data/docker.
	if err := os.Symlink(filepath.Join(base, "data", "docker"), filepath.Join(base, "docker")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	cache := filepath.ToSlash(filepath.Join(base, "docker", "volumes", "rd-cache", "_data"))

	for _, shared := range []string{"", "/"} {
		r, err := openAt(shared, cache, cache+"/upper")
		if err != nil {
			t.Fatalf("root %q: a data root reached through a link was refused: %v", shared, err)
		}
		_ = r.Close()
	}

	if err := os.RemoveAll(filepath.Join(real, "upper")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(real, "upper")); err != nil {
		t.Fatal(err)
	}
	if r, err := openAt("/", cache, cache+"/upper"); err == nil {
		_ = r.Close()
		t.Error("a link under the cache volume was followed out of it")
	}
}

func TestChownRefusesALinkAndTouchesNothingOutside(t *testing.T) {
	d := newDaemonFS(t)
	d.replace(t, volumeDir+"/upper", filepath.Join(d.outside, "upper"))

	if err := chownAt(d.root, volumeDir, volumeDir+"/upper", os.Getuid(), os.Getgid()); err == nil {
		t.Error("a chown went through a link out of the daemon's filesystem")
	}
}

// A directory inside the cache layer replaced by a link is not entered: the
// listing neither fails nor reports what is behind it.
func TestChangesDoNotListThroughALinkInTheCacheLayer(t *testing.T) {
	d := newDaemonFS(t)
	upper := filepath.Join(d.data, "upper")
	if err := os.WriteFile(filepath.Join(upper, "made"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(d.outside, "upper"), filepath.Join(upper, "evil")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	r := rootAt(t, upper)
	changes, err := changesIn(&live{}, r, "/m/aaaa", true, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ch := range changes {
		got = append(got, ch.Path)
		if ch.Path == "/evil/secret" {
			t.Errorf("a change was listed through a link: %+v", ch)
		}
	}
	if !slices.Contains(got, "/made") {
		t.Errorf("changes = %v, want the ordinary file", got)
	}
}
