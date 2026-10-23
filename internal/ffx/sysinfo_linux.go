//go:build linux

package ffx

import "golang.org/x/sys/unix"

func MemTotalMB() uint64 {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return 0
	}
	return uint64(si.Totalram) * uint64(si.Unit) / (1 << 20)
}

func DiskFreeMB(path string) uint64 {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0
	}
	return uint64(st.Bavail) * uint64(st.Bsize) / (1 << 20)
}
