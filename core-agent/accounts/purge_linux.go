//go:build linux

package accounts

import (
	"bufio"
	"os"
	"strings"
	"syscall"
)

// ownerOf is the uid owning a file.
func ownerOf(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// mountedUnder names a mount at or below dir, whose contents RemoveAll would
// delete too.
func mountedUnder(dir string) (string, error) {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		m := strings.ReplaceAll(fields[1], `\040`, " ") // how /proc/mounts writes a space
		if m == dir || strings.HasPrefix(m, dir+"/") {
			return m, nil
		}
	}
	return "", sc.Err()
}
