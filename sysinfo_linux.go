//go:build linux

package main

import "syscall"

// statfsUsage asks the kernel about one mounted filesystem. Frsize is the
// fragment size the block counts are expressed in; Bsize is only a fallback
// for filesystems that leave it zero.
var statfsUsage = func(path string) (fsUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return fsUsage{}, err
	}
	bs := int64(st.Frsize)
	if bs <= 0 {
		bs = int64(st.Bsize)
	}
	return fsUsage{
		Total:      int64(st.Blocks) * bs,
		Free:       int64(st.Bfree) * bs,
		Avail:      int64(st.Bavail) * bs,
		Inodes:     int64(st.Files),
		InodesFree: int64(st.Ffree),
	}, nil
}
