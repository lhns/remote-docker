//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func selfCommand(role string, args ...string) *exec.Cmd {
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	cmd := exec.Command(self, append([]string{"--child", role}, args...)...)
	cmd.Stderr = os.Stderr
	return cmd
}

// child runs a role to completion and returns what it printed, trimmed: one
// result token, `ok` or an errno name. An exec failure is the error.
func child(role string, args ...string) (string, error) {
	out, err := selfCommand(role, args...).Output()
	if err != nil {
		return "", fmt.Errorf("child %s: %w", role, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// startChild is child without waiting, for roles that run concurrently. Its
// result token goes to stderr so it cannot land in the transcript.
func startChild(role string, args ...string) (*exec.Cmd, error) {
	cmd := selfCommand(role, args...)
	cmd.Stdout = os.Stderr
	return cmd, cmd.Start()
}

// childMain is the hidden entry point. Every role prints resultOf(err) and
// exits 0, so the parent reads the outcome from stdout rather than from an
// exit status.
func childMain(role string, args []string) int {
	fmt.Println(resultOf(runChild(role, args)))
	return 0
}

func runChild(role string, args []string) error {
	switch role {
	case "write-at-0":
		// write-at-0 <file> <byte>: pwrite 4096 copies of the byte at 0.
		fd, err := unix.Open(args[0], unix.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer closeFd(fd)
		_, err = unix.Pwrite(fd, fill(args[1][0], 4096), 0)
		return err

	case "mmap-write":
		// mmap-write <file> <byte>: map MAP_SHARED, write, msync, unmap.
		fd, err := unix.Open(args[0], unix.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer closeFd(fd)
		m, err := unix.Mmap(fd, 0, 4096, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return err
		}
		copy(m, fill(args[1][0], 4096))
		if err := unix.Msync(m, unix.MS_SYNC); err != nil {
			return err
		}
		return unix.Munmap(m)

	case "flock-nb":
		fd, err := unix.Open(args[0], unix.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer closeFd(fd)
		return unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)

	case "fcntl-setlk":
		fd, err := unix.Open(args[0], unix.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer closeFd(fd)
		return unix.FcntlFlock(uintptr(fd), unix.F_SETLK, &unix.Flock_t{
			Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 10,
		})

	case "append":
		// append <file> <tag> <n>: n lines of fixed width, one write each.
		n, _ := strconv.Atoi(args[2])
		fd, err := unix.Open(args[0], unix.O_WRONLY|unix.O_APPEND, 0)
		if err != nil {
			return err
		}
		defer closeFd(fd)
		for i := range n {
			if _, err := unix.Write(fd, []byte(fmt.Sprintf("%s %06d\n", args[1], i))); err != nil {
				return err
			}
		}
		return nil

	case "create":
		// create <dir> <tag> <n>: n files named <tag>-<i>.
		n, _ := strconv.Atoi(args[2])
		for i := range n {
			name := args[0] + "/" + fmt.Sprintf("%s-%03d", args[1], i)
			fd, err := unix.Open(name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			_ = unix.Close(fd)
		}
		return nil
	}
	return fmt.Errorf("unknown child role %q", role)
}

// closeFd closes a descriptor whose operation has already succeeded or
// failed; its own error carries nothing further.
func closeFd(fd int) { _ = unix.Close(fd) }

func fill(b byte, n int) []byte {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = b
	}
	return buf
}
