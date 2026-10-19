package main

import "syscall"

// statfs measures the filesystem holding dir.
func statfs(dir string) (diskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return diskUsage{}, err
	}
	bs := uint64(st.Bsize)
	return diskUsage{Used: (st.Blocks - st.Bfree) * bs, Avail: st.Bavail * bs}, nil
}
