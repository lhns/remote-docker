//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// groups is the transcript's order. Step names are ids the runner's diff keys
// on; keep them stable, and unique within a run: a step printing several
// lines carries a `.<sub>` suffix per line (`case.create`, `case.readdir`),
// which fsprobe_test.go asserts.
// owner and noIno are what a group prints differently; see step.format in
// stat.go for why they are a group's property and not a rule about its name.
var groups = []struct {
	name  string
	owner bool // print mode, uid and gid on every stat: the attrs group is about them
	noIno bool // print no inode label: the names group's steps share no inode
	run   func(g *group)
}{
	{name: "create", run: groupCreate},
	{name: "names", noIno: true, run: groupNames},
	{name: "rename", run: groupRename},
	{name: "remove", run: groupRemove},
	{name: "links", run: groupLinks},
	{name: "attrs", owner: true, run: groupAttrs},
	{name: "dirs", run: groupDirs},
	{name: "mmap", run: groupMmap},
	{name: "locks", run: groupLocks},
	{name: "concurrency", run: groupConcurrency},
	{name: "workload", run: groupWorkload},
}

const (
	gib   = 1 << 30
	large = 4*gib + 1
)

// pattern is 1 MiB of bytes that a torn or shifted read cannot reproduce.
func pattern() []byte {
	b := make([]byte, 1<<20)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// match answers ok:match / ok:mismatch for a read-back.
func match(got, want []byte) string {
	if bytes.Equal(got, want) {
		return "match"
	}
	return "mismatch"
}

// band reports n as ~want when it is within tol of it, and exactly otherwise.
// A count that is inherently approximate is reported approximately, so that two
// runs of the same probe against the same filesystem cannot differ on it while
// a filesystem that is out by thousands still shows up.
func band(n, want, tol int) string {
	if n >= want-tol && n <= want+tol {
		return fmt.Sprintf("~%d", want)
	}
	return fmt.Sprint(n)
}

// exactly reports whether n is the number the step asked for, without printing
// a number that a partial failure would make vary between runs.
func exactly(n, want int) string {
	if n == want {
		return fmt.Sprint(want)
	}
	return fmt.Sprintf("not-%d", want)
}

// some buckets a count of anomalies to none/some: how MANY torn lines or
// duplicated names a filesystem produces is timing, whether it produces any is
// the property.
func some(n int) string {
	if n == 0 {
		return "none"
	}
	return "some"
}

// bumped answers whether name's mtime is later than before. Only meaningful
// where the caller has waited longer than the filesystem's timestamp
// granularity; see probe.format in stat.go.
func bumped(g *group, name string, before unix.Timespec) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(g.path(name), &st); err != nil {
		return "", err
	}
	if st.Mtim.Sec > before.Sec || (st.Mtim.Sec == before.Sec && st.Mtim.Nsec > before.Nsec) {
		return "bumped", nil
	}
	return "not-bumped", nil
}

// createExcl is open(O_CREAT|O_EXCL), closed again.
func createExcl(path string) error {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

// countLines splits a file into lines and reports how many match ok, for
// the append steps: every line is fixed width, so a torn one is visible.
func countLines(data []byte, ok func(string) bool) (n, torn int) {
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		n++
		if !ok(line) {
			torn++
		}
	}
	return n, torn
}

func groupCreate(g *group) {
	// runOwner, not run: this is the one line outside the attrs group that
	// shows what a freshly created file's mode and ownership are.
	g.runOwner("create", "open(O_CREAT|O_EXCL) f", func(s *step) (string, error) {
		err := createExcl(g.path("f"))
		s.Stat("f")
		return "", err
	})
	g.run("excl-again", "open(O_CREAT|O_EXCL) f", func(*step) (string, error) {
		return "", createExcl(g.path("f"))
	})
	g.run("write-read", "write 1 MiB, read back", func(*step) (string, error) {
		want := pattern()
		if err := os.WriteFile(g.path("f"), want, 0o644); err != nil {
			return "", err
		}
		got, err := os.ReadFile(g.path("f"))
		if err != nil {
			return "", err
		}
		return match(got, want), nil
	})
	g.run("append-two-fds", "2 x O_APPEND fds, 100 writes each", func(s *step) (string, error) {
		fds := [2]int{}
		for i := range fds {
			fd, err := unix.Open(g.path("app"), unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND, 0o644)
			if err != nil {
				return "", err
			}
			defer closeFd(fd)
			fds[i] = fd
		}
		for i := range 100 {
			for j, fd := range fds {
				if _, err := unix.Write(fd, []byte(fmt.Sprintf("%c%04d\n", 'a'+j, i))); err != nil {
					return "", err
				}
			}
		}
		data, err := os.ReadFile(g.path("app"))
		if err != nil {
			return "", err
		}
		s.Stat("app")
		n, torn := countLines(data, func(l string) bool { return len(l) == 5 && (l[0] == 'a' || l[0] == 'b') })
		if torn > 0 || n != 200 {
			return "torn", nil
		}
		return "200 lines", nil
	})
	g.run("truncate-down-path", "truncate f to 100", func(s *step) (string, error) {
		err := unix.Truncate(g.path("f"), 100)
		s.Stat("f")
		return "", err
	})
	g.run("truncate-up-path", "truncate f to 8192", func(s *step) (string, error) {
		err := unix.Truncate(g.path("f"), 8192)
		s.Stat("f")
		return "", err
	})
	g.run("ftruncate-fd", "ftruncate f to 1000 via fd", func(s *step) (string, error) {
		fd, err := unix.Open(g.path("f"), unix.O_RDWR, 0)
		if err != nil {
			return "", err
		}
		defer closeFd(fd)
		err = unix.Ftruncate(fd, 1000)
		s.Fstat(fd, "f")
		return "", err
	})
	g.run("sparse", "pwrite 1 byte at 1 GiB, fsync (blocks bucketed to 0 or some, never counted)", func(*step) (string, error) {
		fd, err := unix.Open(g.path("sparse"), unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0o644)
		if err != nil {
			return "", err
		}
		defer closeFd(fd)
		if _, err := unix.Pwrite(fd, []byte{1}, gib); err != nil {
			return "", err
		}
		// Without this the answer is a race rather than a property of the
		// filesystem: under delayed allocation st_blocks stays 0 until
		// writeback, so the same run twice reports 0 and then some.
		if err := unix.Fsync(fd); err != nil {
			return "", err
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return "", err
		}
		blocks := "0"
		if st.Blocks > 0 {
			blocks = "some"
		}
		return fmt.Sprintf("size=%d blocks=%s", st.Size, blocks), nil
	})
	if g.p.large {
		g.run("large", "ftruncate to 4 GiB+1, write and read the last byte", func(s *step) (string, error) {
			fd, err := unix.Open(g.path("large"), unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0o644)
			if err != nil {
				return "", err
			}
			defer closeFd(fd)
			if err := unix.Ftruncate(fd, large); err != nil {
				return "", err
			}
			if _, err := unix.Pwrite(fd, []byte{7}, large-1); err != nil {
				return "", err
			}
			buf := make([]byte, 1)
			if _, err := unix.Pread(fd, buf, large-1); err != nil {
				return "", err
			}
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil {
				return "", err
			}
			s.Fstat(fd, "large")
			return fmt.Sprintf("%s size=%d", match(buf, []byte{7}), st.Size), nil
		})
	}
}

func groupNames(g *group) {
	// This group prints no inode label (noIno in the groups table): every name
	// is created, listed and unlinked on its own, so a label relates nothing to
	// anything, and a host that refuses one name renumbers every name after it.
	//
	// listed says whether one ReadDir of the group directory shows name.
	listed := func(dir, name string) (string, error) {
		entries, err := os.ReadDir(g.path(dir))
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if e.Name() == name {
				return "listed", nil
			}
		}
		return "missing", nil
	}
	// one runs create, stat, readdir and unlink for a single name.
	one := func(id, name string) {
		quoted := fmt.Sprintf("%q", name)
		g.run(id+".create", "create "+quoted, func(s *step) (string, error) {
			err := createExcl(g.path(name))
			s.Lstat(name)
			return "", err
		})
		g.run(id+".readdir", "readdir", func(*step) (string, error) { return listed(".", name) })
		g.run(id+".unlink", "unlink "+quoted, func(*step) (string, error) { return "", unix.Unlink(g.path(name)) })
	}

	one("plain", "plain")
	one("space", "with space")
	one("nfc", "nfc-\u00e9")
	one("nfd", "nfd-e\u0301")

	g.run("case.create", "create a and A", func(*step) (string, error) {
		if err := createExcl(g.path("a")); err != nil {
			return "", err
		}
		return "", createExcl(g.path("A"))
	})
	g.run("case.readdir", "readdir count of a/A", func(*step) (string, error) {
		entries, err := os.ReadDir(g.dir)
		if err != nil {
			return "", err
		}
		n := 0
		for _, e := range entries {
			if e.Name() == "a" || e.Name() == "A" {
				n++
			}
		}
		if n == 1 {
			return "1 entry", nil
		}
		return fmt.Sprintf("%d entries", n), nil
	})
	g.run("case.write", "write a, write A, read a", func(*step) (string, error) {
		if err := os.WriteFile(g.path("a"), []byte("lower"), 0o644); err != nil {
			return "", err
		}
		if err := os.WriteFile(g.path("A"), []byte("upper"), 0o644); err != nil {
			return "", err
		}
		got, err := os.ReadFile(g.path("a"))
		if err != nil {
			return "", err
		}
		if string(got) == "lower" {
			return "distinct", nil
		}
		return "aliased", nil
	})
	g.run("case.unlink", "unlink a and A", func(*step) (string, error) {
		err := unix.Unlink(g.path("a"))
		if err2 := unix.Unlink(g.path("A")); err == nil {
			err = err2
		}
		return "", err
	})

	one("long250", strings.Repeat("n", 250))

	deep := strings.TrimSuffix(strings.Repeat("x/", 300), "/")
	g.run("deep300.mkdir", "mkdir chain of 300 components", func(*step) (string, error) {
		return "", os.MkdirAll(g.path(deep), 0o755)
	})
	g.run("deep300.create", "create leaf file", func(s *step) (string, error) {
		err := createExcl(g.path(deep + "/f"))
		s.Stat(deep + "/f")
		return "", err
	})
	g.run("deep300.readdir", "readdir leaf", func(*step) (string, error) { return listed(deep, "f") })
	g.run("deep300.remove", "remove chain", func(*step) (string, error) { return "", os.RemoveAll(g.path("x")) })

	one("con", "con")
	one("nul", "nul")
	one("aux", "aux")
	one("com1", "com1")
	one("trailing-dot", "trailing.")
	one("trailing-space", "trailing ")
	one("colon", "colon:name")
	one("star", "star*")
	one("question", "question?")
	one("quote", `quote"`)
	one("pipe", "pipe|")
	one("ltgt", "lt<gt>")
}

func groupRename(g *group) {
	g.run("within-dir", "create a, rename a -> b", func(s *step) (string, error) {
		if err := createExcl(g.path("a")); err != nil {
			return "", err
		}
		err := unix.Rename(g.path("a"), g.path("b"))
		s.Stat("b")
		return "", err
	})
	g.run("across-dirs", "create d1/f, rename -> d2/f", func(s *step) (string, error) {
		for _, d := range []string{"d1", "d2"} {
			if err := unix.Mkdir(g.path(d), 0o755); err != nil {
				return "", err
			}
		}
		if err := createExcl(g.path("d1/f")); err != nil {
			return "", err
		}
		err := unix.Rename(g.path("d1/f"), g.path("d2/f"))
		s.Stat("d2/f")
		return "", err
	})
	g.run("over-file", "create x and y, rename x -> y", func(s *step) (string, error) {
		if err := os.WriteFile(g.path("x"), []byte("x"), 0o644); err != nil {
			return "", err
		}
		if err := os.WriteFile(g.path("y"), []byte("yy"), 0o644); err != nil {
			return "", err
		}
		err := unix.Rename(g.path("x"), g.path("y"))
		s.Stat("y")
		return "", err
	})
	g.run("over-empty-dir", "mkdir e1 e2, rename e1 -> e2", func(s *step) (string, error) {
		for _, d := range []string{"e1", "e2"} {
			if err := unix.Mkdir(g.path(d), 0o755); err != nil {
				return "", err
			}
		}
		err := unix.Rename(g.path("e1"), g.path("e2"))
		s.Stat("e2")
		return "", err
	})
	g.run("over-nonempty-dir", "mkdir n1 n2 n2/f, rename n1 -> n2", func(*step) (string, error) {
		for _, d := range []string{"n1", "n2"} {
			if err := unix.Mkdir(g.path(d), 0o755); err != nil {
				return "", err
			}
		}
		if err := createExcl(g.path("n2/f")); err != nil {
			return "", err
		}
		return "", unix.Rename(g.path("n1"), g.path("n2"))
	})
	g.run("open-then-rename", "open o, rename o -> o2, write via fd, read o2", func(s *step) (string, error) {
		fd, err := unix.Open(g.path("o"), unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0o644)
		if err != nil {
			return "", err
		}
		defer closeFd(fd)
		if err := unix.Rename(g.path("o"), g.path("o2")); err != nil {
			return "", err
		}
		if _, err := unix.Write(fd, []byte("hello")); err != nil {
			return "", err
		}
		s.Fstat(fd, "o2")
		got, err := os.ReadFile(g.path("o2"))
		if err != nil {
			return "", err
		}
		return match(got, []byte("hello")), nil
	})
	g.run("dir-with-open-file", "open dd/f, rename dd -> dd2, write via fd, read dd2/f", func(s *step) (string, error) {
		if err := unix.Mkdir(g.path("dd"), 0o755); err != nil {
			return "", err
		}
		fd, err := unix.Open(g.path("dd/f"), unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0o644)
		if err != nil {
			return "", err
		}
		defer closeFd(fd)
		if err := unix.Rename(g.path("dd"), g.path("dd2")); err != nil {
			return "", err
		}
		if _, err := unix.Write(fd, []byte("hello")); err != nil {
			return "", err
		}
		s.Fstat(fd, "dd2/f")
		got, err := os.ReadFile(g.path("dd2/f"))
		if err != nil {
			return "", err
		}
		return match(got, []byte("hello")), nil
	})
	g.run("ino-after-rename", "create i, stat, rename i -> i2, stat", func(s *step) (string, error) {
		if err := createExcl(g.path("i")); err != nil {
			return "", err
		}
		var before unix.Stat_t
		if err := unix.Stat(g.path("i"), &before); err != nil {
			return "", err
		}
		if err := unix.Rename(g.path("i"), g.path("i2")); err != nil {
			return "", err
		}
		var after unix.Stat_t
		if err := unix.Stat(g.path("i2"), &after); err != nil {
			return "", err
		}
		s.Stat("i2")
		if g.p.label(&before) == g.p.label(&after) {
			return "same-ino", nil
		}
		return "different-ino", nil
	})
}

func groupRemove(g *group) {
	// nfsEntries counts .nfs* entries in the group directory: the client's
	// silly rename of a file unlinked while open.
	nfsEntries := func() (string, error) {
		entries, err := os.ReadDir(g.dir)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".nfs") {
				return "sillyrename", nil
			}
		}
		return "no-sillyrename", nil
	}
	var fd int
	g.run("unlink-open.unlink", "open u, unlink u", func(*step) (string, error) {
		var err error
		fd, err = unix.Open(g.path("u"), unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0o644)
		if err != nil {
			return "", err
		}
		return "", unix.Unlink(g.path("u"))
	})
	g.run("unlink-open.write", "write 100 bytes via fd, fstat", func(s *step) (string, error) {
		if _, err := unix.Write(fd, make([]byte, 100)); err != nil {
			return "", err
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return "", err
		}
		s.Fstat(fd, "u")
		return fmt.Sprintf("size=%d", st.Size), nil
	})
	g.run("unlink-open.readdir-open", "readdir for .nfs* while open", func(*step) (string, error) { return nfsEntries() })
	g.run("unlink-open.close", "close", func(*step) (string, error) { return "", unix.Close(fd) })
	g.run("unlink-open.readdir-closed", "readdir for .nfs* after close", func(*step) (string, error) { return nfsEntries() })

	g.run("rmdir-nonempty", "mkdir d d/f, rmdir d", func(*step) (string, error) {
		if err := unix.Mkdir(g.path("d"), 0o755); err != nil {
			return "", err
		}
		if err := createExcl(g.path("d/f")); err != nil {
			return "", err
		}
		return "", unix.Rmdir(g.path("d"))
	})
	g.run("rmdir-empty", "unlink d/f, rmdir d", func(*step) (string, error) {
		if err := unix.Unlink(g.path("d/f")); err != nil {
			return "", err
		}
		return "", unix.Rmdir(g.path("d"))
	})
	g.run("recreate-same-name", "create r, unlink, create again", func(s *step) (string, error) {
		if err := createExcl(g.path("r")); err != nil {
			return "", err
		}
		var first unix.Stat_t
		if err := unix.Stat(g.path("r"), &first); err != nil {
			return "", err
		}
		if err := unix.Unlink(g.path("r")); err != nil {
			return "", err
		}
		if err := createExcl(g.path("r")); err != nil {
			return "", err
		}
		var second unix.Stat_t
		if err := unix.Stat(g.path("r"), &second); err != nil {
			return "", err
		}
		s.Stat("r")
		if g.p.label(&first) == g.p.label(&second) {
			return "same-ino", nil
		}
		return "different-ino", nil
	})
}

func groupLinks(g *group) {
	nlink := func(name string) (string, error) {
		var st unix.Stat_t
		if err := unix.Stat(g.path(name), &st); err != nil {
			return "", err
		}
		return fmt.Sprintf("nlink=%d", uint64(st.Nlink)), nil //nolint:unconvert // Nlink's width differs per arch
	}
	g.run("hardlink.link", "create orig, link orig -> hard", func(s *step) (string, error) {
		if err := os.WriteFile(g.path("orig"), []byte("orig"), 0o644); err != nil {
			return "", err
		}
		err := unix.Link(g.path("orig"), g.path("hard"))
		s.Stat("hard")
		if err != nil {
			return "", err
		}
		return nlink("orig")
	})
	g.run("hardlink.stat", "stat orig and hard", func(*step) (string, error) { return g.sameIno("orig", "hard") })
	g.run("write-via-link", "write hard, read orig", func(*step) (string, error) {
		if err := os.WriteFile(g.path("hard"), []byte("via link"), 0o644); err != nil {
			return "", err
		}
		got, err := os.ReadFile(g.path("orig"))
		if err != nil {
			return "", err
		}
		return match(got, []byte("via link")), nil
	})
	g.run("unlink-one", "unlink orig, stat hard", func(s *step) (string, error) {
		if err := unix.Unlink(g.path("orig")); err != nil {
			return "", err
		}
		s.Stat("hard")
		return nlink("hard")
	})
	g.run("symlink", "create target.txt, symlink target.txt -> link, readlink", func(s *step) (string, error) {
		if err := os.WriteFile(g.path("target.txt"), []byte("target"), 0o644); err != nil {
			return "", err
		}
		if err := unix.Symlink("target.txt", g.path("link")); err != nil {
			return "", err
		}
		s.Lstat("link")
		target, err := os.Readlink(g.path("link"))
		return target, err
	})
	g.run("follow", "read link", func(*step) (string, error) {
		got, err := os.ReadFile(g.path("link"))
		if err != nil {
			return "", err
		}
		return match(got, []byte("target")), nil
	})
	g.run("dangling.symlink", "symlink nothing -> dang, stat", func(s *step) (string, error) {
		if err := unix.Symlink("nothing", g.path("dang")); err != nil {
			return "", err
		}
		var st unix.Stat_t
		err := unix.Stat(g.path("dang"), &st)
		s.Stat("dang")
		return "", err
	})
	g.run("dangling.lstat", "lstat dang", func(s *step) (string, error) {
		var st unix.Stat_t
		err := unix.Lstat(g.path("dang"), &st)
		s.Lstat("dang")
		return "", err
	})
	g.run("symlink-to-dir", "mkdir sd sd/f, symlink sd -> sdl, readdir sdl", func(s *step) (string, error) {
		if err := unix.Mkdir(g.path("sd"), 0o755); err != nil {
			return "", err
		}
		if err := createExcl(g.path("sd/f")); err != nil {
			return "", err
		}
		if err := unix.Symlink("sd", g.path("sdl")); err != nil {
			return "", err
		}
		s.Stat("sdl")
		entries, err := os.ReadDir(g.path("sdl"))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d entries", len(entries)), nil
	})
	g.run("lstat-vs-stat", "lstat link / stat link", func(*step) (string, error) {
		var l, st unix.Stat_t
		if err := unix.Lstat(g.path("link"), &l); err != nil {
			return "", err
		}
		if err := unix.Stat(g.path("link"), &st); err != nil {
			return "", err
		}
		return fileType(l.Mode) + "/" + fileType(st.Mode), nil
	})
}

func groupAttrs(g *group) {
	g.run("create", "create m", func(s *step) (string, error) {
		err := os.WriteFile(g.path("m"), []byte("m"), 0o644)
		s.Stat("m")
		return "", err
	})
	chmod := func(id string, mode uint32) {
		g.run(id+".chmod", fmt.Sprintf("chmod %04o m", mode), func(s *step) (string, error) {
			err := unix.Chmod(g.path("m"), mode)
			s.Stat("m")
			return "", err
		})
	}
	chmod("chmod-644", 0o644)
	chmod("chmod-600", 0o600)
	chmod("chmod-755", 0o755)
	chmod("chmod-000", 0)
	g.run("chmod-000.open", "open(O_RDONLY) m", func(*step) (string, error) {
		fd, err := unix.Open(g.path("m"), unix.O_RDONLY, 0)
		if err != nil {
			return "", err
		}
		return "", unix.Close(fd)
	})
	chmod("chmod-x", 0o111)
	g.run("chown-self", "chown m to own uid:gid", func(s *step) (string, error) {
		err := unix.Chown(g.path("m"), os.Getuid(), os.Getgid())
		s.Stat("m")
		return "", err
	})
	g.run("chown-other", "chown m to 1234:1234", func(s *step) (string, error) {
		err := unix.Chown(g.path("m"), 1234, 1234)
		s.Stat("m")
		if err != nil {
			return "", err
		}
		var st unix.Stat_t
		if err := unix.Stat(g.path("m"), &st); err != nil {
			return "", err
		}
		return fmt.Sprintf("uid=%d", st.Uid), nil
	})

	// applied says whether m's mtime is the fixed past epoch that was set.
	const past = 1000000000
	applied := func() (string, error) {
		var st unix.Stat_t
		if err := unix.Stat(g.path("m"), &st); err != nil {
			return "", err
		}
		if st.Mtim.Sec == past && st.Mtim.Nsec == 0 {
			return "applied", nil
		}
		return "ignored", nil
	}
	g.run("utime-path", "utimensat m to epoch 1000000000", func(s *step) (string, error) {
		ts := []unix.Timespec{{Sec: past}, {Sec: past}}
		err := unix.UtimesNanoAt(unix.AT_FDCWD, g.path("m"), ts, 0)
		s.Stat("m")
		if err != nil {
			return "", err
		}
		return applied()
	})
	g.run("futimens-fd", "touch m to now, futimes fd to epoch 1000000000", func(s *step) (string, error) {
		now := []unix.Timespec{{Sec: 0, Nsec: unix.UTIME_NOW}, {Sec: 0, Nsec: unix.UTIME_NOW}}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, g.path("m"), now, 0); err != nil {
			return "", err
		}
		fd, err := unix.Open(g.path("m"), unix.O_RDONLY, 0)
		if err != nil {
			return "", err
		}
		defer closeFd(fd)
		err = unix.Futimes(fd, []unix.Timeval{{Sec: past}, {Sec: past}})
		s.Fstat(fd, "m")
		if err != nil {
			return "", err
		}
		return applied()
	})
	var beforeWrite unix.Timespec
	g.run("mtime-on-write.stat", "stat m", func(s *step) (string, error) {
		var st unix.Stat_t
		if err := unix.Stat(g.path("m"), &st); err != nil {
			return "", err
		}
		beforeWrite = st.Mtim
		s.Stat("m")
		return "", nil
	})
	g.run("mtime-on-write.write", "sleep 1.1s, write m, stat (reports bumped/not-bumped)", func(s *step) (string, error) {
		// The sleep is longer than any filesystem's timestamp granularity, so
		// bumped/not-bumped is a property of the filesystem rather than of when
		// the two operations happened to fall. The stat suffix cannot answer it:
		// see probe.format in stat.go.
		time.Sleep(1100 * time.Millisecond)
		if err := os.WriteFile(g.path("m"), []byte("mm"), 0o644); err != nil {
			return "", err
		}
		s.Stat("m")
		return bumped(g, "m", beforeWrite)
	})
}

// getdents lists a directory in the kernel's order, in 4 KiB buffers, calling
// between with the names of the first buffer once it is read. Names exclude
// . and ..
func getdents(dir string, between func(first []string)) ([]string, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer closeFd(fd)
	var names []string
	buf := make([]byte, 4096)
	for first := true; ; first = false {
		n, err := unix.Getdents(fd, buf)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return names, nil
		}
		_, _, names = unix.ParseDirent(buf[:n], -1, names)
		if first && between != nil {
			between(append([]string(nil), names...))
		}
	}
}

func groupDirs(g *group) {
	const many = "many"
	g.run("readdir-5000", "create 5000 files, one ReadDir", func(*step) (string, error) {
		if err := unix.Mkdir(g.path(many), 0o755); err != nil {
			return "", err
		}
		for i := range 5000 {
			if err := createExcl(g.path(fmt.Sprintf("%s/f%05d", many, i))); err != nil {
				return "", err
			}
		}
		entries, err := os.ReadDir(g.path(many))
		if err != nil {
			return "", err
		}
		return fmt.Sprint(len(entries)), nil
	})
	g.run("readdir-modified-midscan", "getdents; after 1st buffer create 10, remove 10 listed (count banded, dups bucketed)", func(*step) (string, error) {
		var modifyErr error
		names, err := getdents(g.path(many), func(first []string) {
			for i := range 10 {
				if err := createExcl(g.path(fmt.Sprintf("%s/g%05d", many, i))); err != nil {
					modifyErr = err
					return
				}
			}
			for _, name := range first[:10] {
				if err := unix.Unlink(g.path(many + "/" + name)); err != nil {
					modifyErr = err
					return
				}
			}
		})
		if err != nil {
			return "", err
		}
		if modifyErr != nil {
			return "", modifyErr
		}
		seen := map[string]int{}
		dups := 0
		for _, n := range names {
			seen[n]++
			if seen[n] == 2 {
				dups++
			}
		}
		// How many entries a scan interrupted by a mutation returns is not a
		// constant: it depends on where the created and removed names fall
		// relative to the buffer already read, which is the kernel's business
		// and not the filesystem's. What the two transcripts have to agree on
		// is that the scan finished near 5000 and whether it repeated a name.
		return fmt.Sprintf("entries=%s dups=%s", band(len(names), 5000, 20), some(dups)), nil
	})
	g.run("order-stable", "two getdents listings compared", func(*step) (string, error) {
		a, err := getdents(g.path(many), nil)
		if err != nil {
			return "", err
		}
		b, err := getdents(g.path(many), nil)
		if err != nil {
			return "", err
		}
		if strings.Join(a, "\x00") == strings.Join(b, "\x00") {
			return "ordered-same", nil
		}
		return "ordered-differs", nil
	})
	g.run("mkdir-chain-50", "50 nested dirs, stat leaf", func(s *step) (string, error) {
		chain := strings.TrimSuffix(strings.Repeat("c/", 50), "/")
		err := os.MkdirAll(g.path(chain), 0o755)
		s.Stat(chain)
		return "", err
	})
}

func groupMmap(g *group) {
	g.run("mmap-read-sees-write", "map mm MAP_SHARED, child pwrites, re-read mapping", func(*step) (string, error) {
		if err := os.WriteFile(g.path("mm"), fill('A', 4096), 0o644); err != nil {
			return "", err
		}
		fd, err := unix.Open(g.path("mm"), unix.O_RDONLY, 0)
		if err != nil {
			return "", err
		}
		defer closeFd(fd)
		m, err := unix.Mmap(fd, 0, 4096, unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			return "", err
		}
		defer func() { _ = unix.Munmap(m) }()
		if m[0] != 'A' {
			return "", errors.New("mapping does not show the file's own content")
		}
		res, err := child("write-at-0", g.path("mm"), "B")
		if err != nil {
			return "", err
		}
		if res != "ok" {
			return "child=" + res, nil
		}
		if m[0] == 'B' && m[4095] == 'B' {
			return "sees", nil
		}
		return "stale", nil
	})
	g.run("mmap-write-msync", "child maps mw, writes, msyncs; read(2)", func(*step) (string, error) {
		if err := os.WriteFile(g.path("mw"), make([]byte, 4096), 0o644); err != nil {
			return "", err
		}
		res, err := child("mmap-write", g.path("mw"), "C")
		if err != nil {
			return "", err
		}
		if res != "ok" {
			return "child=" + res, nil
		}
		got, err := os.ReadFile(g.path("mw"))
		if err != nil {
			return "", err
		}
		return match(got, fill('C', 4096)), nil
	})
}

func groupLocks(g *group) {
	g.run("flock-excl", "flock(LOCK_EX) fl; child flock(LOCK_EX|LOCK_NB)", func(*step) (string, error) {
		fd, err := unix.Open(g.path("fl"), unix.O_RDWR|unix.O_CREAT, 0o644)
		if err != nil {
			return "", err
		}
		defer closeFd(fd)
		if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
			return "", err
		}
		res, err := child("flock-nb", g.path("fl"))
		if err != nil {
			return "", err
		}
		return "child=" + res, nil
	})
	g.run("fcntl-range", "F_SETLK write 0-10 on fc; child F_SETLK same range", func(*step) (string, error) {
		fd, err := unix.Open(g.path("fc"), unix.O_RDWR|unix.O_CREAT, 0o644)
		if err != nil {
			return "", err
		}
		defer closeFd(fd)
		if err := unix.FcntlFlock(uintptr(fd), unix.F_SETLK, &unix.Flock_t{Type: unix.F_WRLCK, Start: 0, Len: 10}); err != nil {
			return "", err
		}
		res, err := child("fcntl-setlk", g.path("fc"))
		if err != nil {
			return "", err
		}
		return "child=" + res, nil
	})
	g.run("flock-same-process-twice", "flock(LOCK_EX) on fd1, flock(LOCK_EX|LOCK_NB) on fd2", func(*step) (string, error) {
		fd1, err := unix.Open(g.path("fl"), unix.O_RDWR, 0)
		if err != nil {
			return "", err
		}
		defer closeFd(fd1)
		fd2, err := unix.Open(g.path("fl"), unix.O_RDWR, 0)
		if err != nil {
			return "", err
		}
		defer closeFd(fd2)
		if err := unix.Flock(fd1, unix.LOCK_EX); err != nil {
			return "", err
		}
		return "", unix.Flock(fd2, unix.LOCK_EX|unix.LOCK_NB)
	})
}

// waitAll waits for every started child; the first wait error wins.
func waitAll(cmds []*exec.Cmd) error {
	var first error
	for _, c := range cmds {
		if err := c.Wait(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func groupConcurrency(g *group) {
	g.run("append-2x1000", "2 children append 1000 lines each with O_APPEND (reports whether all 2000 arrived and whether any line is torn)", func(*step) (string, error) {
		if err := createExcl(g.path("app")); err != nil {
			return "", err
		}
		var cmds []*exec.Cmd
		for _, tag := range []string{"x", "y"} {
			cmd, err := startChild("append", g.path("app"), tag, "1000")
			if err != nil {
				return "", err
			}
			cmds = append(cmds, cmd)
		}
		if err := waitAll(cmds); err != nil {
			return "", err
		}
		data, err := os.ReadFile(g.path("app"))
		if err != nil {
			return "", err
		}
		n, torn := countLines(data, func(l string) bool {
			return len(l) == 8 && (l[0] == 'x' || l[0] == 'y') && l[1] == ' '
		})
		return fmt.Sprintf("lines=%s torn=%s", exactly(n, 2000), some(torn)), nil
	})
	g.run("create-8x200", "8 children create 200 files each in one dir (reports whether all 1600 are there)", func(*step) (string, error) {
		if err := unix.Mkdir(g.path("cr"), 0o755); err != nil {
			return "", err
		}
		var cmds []*exec.Cmd
		for i := range 8 {
			cmd, err := startChild("create", g.path("cr"), fmt.Sprintf("c%d", i), "200")
			if err != nil {
				return "", err
			}
			cmds = append(cmds, cmd)
		}
		if err := waitAll(cmds); err != nil {
			return "", err
		}
		entries, err := os.ReadDir(g.path("cr"))
		if err != nil {
			return "", err
		}
		return "entries=" + exactly(len(entries), 1600), nil
	})
}

// sh runs one external command in the group directory. Its error is the
// first line of stderr, so the transcript names the reason rather than the
// exit status.
func (g *group) sh(env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

func groupWorkload(g *group) {
	if _, err := exec.LookPath("tar"); err != nil {
		g.run("tar.lookup", "tar in PATH", func(*step) (string, error) { return "no-tar", nil })
	} else {
		g.run("tar.mkdir", "mkdir src with 20 files", func(*step) (string, error) {
			if err := unix.Mkdir(g.path("src"), 0o755); err != nil {
				return "", err
			}
			for i := range 20 {
				if err := os.WriteFile(g.path(fmt.Sprintf("src/f%02d", i)), fill(byte('a'+i), 100), 0o644); err != nil {
					return "", err
				}
			}
			return "", nil
		})
		g.run("tar.create", "tar -cf src.tar src", func(*step) (string, error) {
			return "", g.sh(nil, "tar", "-cf", "src.tar", "src")
		})
		g.run("tar.rm", "rm -r src", func(*step) (string, error) { return "", os.RemoveAll(g.path("src")) })
		g.run("tar.extract", "tar -xf src.tar", func(*step) (string, error) { return "", g.sh(nil, "tar", "-xf", "src.tar") })
		g.run("tar.verify", "verify 20 files", func(s *step) (string, error) {
			s.Stat("src/f00")
			entries, err := os.ReadDir(g.path("src"))
			if err != nil {
				return "", err
			}
			n := 0
			for _, e := range entries {
				got, err := os.ReadFile(g.path("src/" + e.Name()))
				if err != nil {
					return "", err
				}
				if bytes.Equal(got, fill(byte('a'+n), 100)) {
					n++
				}
			}
			return fmt.Sprintf("%d files", n), nil
		})
		g.run("tar.touch", "sleep 1.1s, touch src/f00 (reports bumped/not-bumped)", func(s *step) (string, error) {
			var st unix.Stat_t
			if err := unix.Stat(g.path("src/f00"), &st); err != nil {
				return "", err
			}
			// As in attrs/mtime-on-write.write: the sleep is what makes the
			// answer the filesystem's rather than the scheduler's.
			time.Sleep(1100 * time.Millisecond)
			if err := g.sh(nil, "touch", "src/f00"); err != nil {
				return "", err
			}
			s.Stat("src/f00")
			return bumped(g, "src/f00", st.Mtim)
		})
		g.run("tar.chmod", "chmod 600 src/f00", func(s *step) (string, error) {
			err := g.sh(nil, "chmod", "600", "src/f00")
			s.Stat("src/f00")
			return "", err
		})
	}

	if _, err := exec.LookPath("git"); err != nil {
		g.run("git.lookup", "git in PATH", func(*step) (string, error) { return "no-git", nil })
		return
	}
	// git is pinned so that what it does here is a function of the filesystem
	// and of nothing else. Every input that varies run to run is fixed: the two
	// commit timestamps, the identity, the locale and the timezone, so 200
	// commits produce byte-identical objects twice; the config files, so the
	// machine's own git config cannot reach in; and auto gc, which otherwise
	// fires from inside `git commit` at a threshold nothing here controls and
	// detaches a second process that outlives the step. HOME is the probe root
	// rather than the repository, so anything git writes to it cannot arrive as
	// an untracked file in `git status`.
	//
	// A repository owned by another uid (root_squash) is refused by git's
	// safe.directory check, which would hide everything after it; the
	// ownership is reported on its own line instead and the check waived.
	const gitDate = "@1000000000 +0000" // git's raw format; the epoch attrs uses
	env := []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=" + g.p.root,
		"XDG_CONFIG_HOME=" + g.p.root,
		"GIT_AUTHOR_DATE=" + gitDate,
		"GIT_COMMITTER_DATE=" + gitDate,
		"GIT_AUTHOR_NAME=fsprobe",
		"GIT_AUTHOR_EMAIL=fsprobe@example",
		"GIT_COMMITTER_NAME=fsprobe",
		"GIT_COMMITTER_EMAIL=fsprobe@example",
		"GIT_TERMINAL_PROMPT=0",
		"TZ=UTC",
		"LC_ALL=C",
		"LANG=C",
	}
	gitArgs := func(args []string) []string {
		return append([]string{
			"-c", "safe.directory=*",
			"-c", "commit.gpgsign=false",
			"-c", "gc.auto=0",
			"-c", "gc.autoDetach=false",
			"-c", "core.fsmonitor=false",
			"-c", "advice.detachedHead=false",
			"-c", "init.defaultBranch=main",
		}, args...)
	}
	git := func(args ...string) error {
		return g.sh(env, "git", gitArgs(args)...)
	}
	g.run("git.owner", "owner of the directory", func(*step) (string, error) {
		var st unix.Stat_t
		if err := unix.Stat(g.dir, &st); err != nil {
			return "", err
		}
		if int(st.Uid) == os.Getuid() {
			return "owned", nil
		}
		return "foreign", nil
	})
	g.run("git.init", "git init, config user", func(*step) (string, error) {
		if err := git("init", "-q"); err != nil {
			return "", err
		}
		if err := git("config", "user.email", "fsprobe@example"); err != nil {
			return "", err
		}
		return "", git("config", "user.name", "fsprobe")
	})
	g.run("git.commits", "200 commits, one file each", func(*step) (string, error) {
		for i := range 200 {
			name := fmt.Sprintf("c%03d", i)
			if err := os.WriteFile(g.path(name), []byte(name+"\n"), 0o644); err != nil {
				return "", err
			}
			if err := git("add", name); err != nil {
				return "", err
			}
			if err := git("commit", "-q", "-m", name); err != nil {
				return "", err
			}
		}
		return "", nil
	})
	g.run("git.status", "git status --porcelain, twice", func(*step) (string, error) {
		dirty := 0
		for range 2 {
			cmd := exec.Command("git", gitArgs([]string{"status", "--porcelain"})...)
			cmd.Dir = g.dir
			cmd.Env = append(os.Environ(), env...)
			out, err := cmd.Output()
			if err != nil {
				return "", err
			}
			if s := strings.TrimSpace(string(out)); s != "" {
				dirty += len(strings.Split(s, "\n"))
			}
		}
		if dirty == 0 {
			return "clean", nil
		}
		return fmt.Sprintf("dirty:%d", dirty), nil
	})
	g.run("git.checkout-back", "git checkout HEAD~100", func(*step) (string, error) { return "", git("checkout", "-q", "HEAD~100") })
	g.run("git.checkout-return", "git checkout -", func(*step) (string, error) { return "", git("checkout", "-q", "-") })
	g.run("git.gc", "git gc", func(*step) (string, error) { return "", git("gc", "-q") })
	g.run("git.fsck", "git fsck", func(*step) (string, error) { return "", git("fsck", "--no-progress") })
}
