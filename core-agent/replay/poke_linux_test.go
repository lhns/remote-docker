//go:build linux

package replay

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A FIFO in a share is somebody's ordinary file, and opening one for writing
// blocks until a reader appears. The notify channel replays one event at a
// time, so a poke that waited there wedged every event after it.
func TestPokeDoesNotWaitOnAFifo(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Skipf("cannot make a fifo here: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- SyscallPoker{}.Poke(dir, "pipe", false) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poking a fifo blocked waiting for a reader")
	}
}

// watcher is an inotify instance on one directory, the way a container's
// watcher sees what a poke does.
type watcher struct {
	t  *testing.T
	fd int
}

func watch(t *testing.T, dir string, mask uint32) *watcher {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Skipf("no inotify here: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, dir, mask); err != nil {
		t.Skipf("cannot watch %s: %v", dir, err)
	}
	return &watcher{t: t, fd: fd}
}

// masks is every event queued so far.
func (w *watcher) masks() []uint32 {
	w.t.Helper()
	buf := make([]byte, 4096)
	n, err := unix.Read(w.fd, buf)
	if err != nil {
		return nil
	}
	var out []uint32
	for off := 0; off+16 <= n; {
		out = append(out, binary.NativeEndian.Uint32(buf[off+4:]))
		off += 16 + int(binary.NativeEndian.Uint32(buf[off+12:]))
	}
	return out
}

func has(masks []uint32, bit uint32) bool {
	for _, m := range masks {
		if m&bit != 0 {
			return true
		}
	}
	return false
}

const pokeEvents = unix.IN_MODIFY | unix.IN_OPEN | unix.IN_ATTRIB | unix.IN_CLOSE_WRITE | unix.IN_CLOSE_NOWRITE

// An ordinary file gets the two events the feature rests on and its mtime is
// unchanged, through a trusted directory that is itself a link.
func TestPokeProducesTheMeasuredEvents(t *testing.T) {
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(tree, "src", "a.go")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}

	trusted := filepath.Join(t.TempDir(), "mp")
	if err := os.Symlink(tree, trusted); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	w := watch(t, filepath.Join(tree, "src"), pokeEvents)
	if err := (SyscallPoker{}).Poke(trusted, "/src/a.go", false); err != nil {
		t.Fatalf("poke: %v", err)
	}
	got := w.masks()
	t.Logf("events: %#x", got)
	// Exactly what the measured matrix says, so a descriptor opened for
	// reading, or a second open, shows up as a different list.
	if len(got) != 3 || !has(got, unix.IN_MODIFY) || !has(got, unix.IN_OPEN) || !has(got, unix.IN_CLOSE_WRITE) {
		t.Errorf("events %#x, want exactly IN_MODIFY, IN_OPEN and IN_CLOSE_WRITE", got)
	}
	if has(got, unix.IN_ATTRIB) || has(got, unix.IN_CLOSE_NOWRITE) {
		t.Errorf("events %#x include IN_ATTRIB or IN_CLOSE_NOWRITE", got)
	}
	if st, err := os.Stat(file); err != nil || !st.ModTime().Equal(old) {
		t.Errorf("mtime = %v, %v; want %v unchanged", st.ModTime(), err, old)
	}

	// A directory is poked without being opened.
	if err := (SyscallPoker{}).Poke(trusted, "/src", true); err != nil {
		t.Errorf("poking a directory: %v", err)
	}
}

// outsideFile is a file the poke must never reach, with a watcher on it.
func outsideFile(t *testing.T) (dir string, old time.Time, w *watcher) {
	t.Helper()
	dir = t.TempDir()
	file := filepath.Join(dir, "secret")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old = time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	return dir, old, watch(t, dir, pokeEvents)
}

// A link in any component of the path, intermediate or last, absolute or
// relative, is refused: nothing outside is touched, opened or reported.
func TestPokeFollowsNoLinkInThePath(t *testing.T) {
	trusted := t.TempDir()
	outside, old, w := outsideFile(t)

	if err := os.MkdirAll(filepath.Join(trusted, "var", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Join(trusted, "var", "lib"), outside)
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"absolute":     outside,
		"relative":     rel,
		"intermediate": outside,
	}
	for name, target := range links {
		t.Run(name, func(t *testing.T) {
			var link, path string
			switch name {
			case "intermediate":
				link, path = filepath.Join(trusted, "var", "lib", "share"), "/var/lib/share/secret"
			default:
				link, path = filepath.Join(trusted, "var", "lib", "secret"), "/var/lib/secret"
				// The last component is a link to the file itself.
				target = filepath.Join(target, "secret")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("cannot create a symlink here: %v", err)
			}
			defer func() { _ = os.Remove(link) }()

			for _, isDir := range []bool{false, true} {
				if err := (SyscallPoker{}).Poke(trusted, path, isDir); err == nil {
					t.Errorf("poke through %s succeeded", link)
				}
			}
			if got := w.masks(); len(got) != 0 {
				t.Errorf("the file outside saw events %#x", got)
			}
			if st, err := os.Stat(filepath.Join(outside, "secret")); err != nil || !st.ModTime().Equal(old) {
				t.Errorf("outside mtime = %v, %v; want %v unchanged", st.ModTime(), err, old)
			}
		})
	}
}

// ".." in the path stays below the trusted directory.
func TestPokeDoesNotClimbOutOfTheTrustedDirectory(t *testing.T) {
	base := t.TempDir()
	trusted := filepath.Join(base, "root")
	if err := os.Mkdir(trusted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := watch(t, base, pokeEvents)

	err := (SyscallPoker{}).Poke(trusted, "/../secret", false)
	if err == nil {
		t.Error("a path climbing out of the trusted directory was poked")
	}
	if got := w.masks(); len(got) != 0 {
		t.Errorf("the file outside saw events %#x", got)
	}
}
