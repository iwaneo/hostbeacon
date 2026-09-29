package system

import "syscall"

// Statfs reads the size of the file system at path.
func Statfs(path string) (FSSize, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return FSSize{}, err
	}
	size := uint64(stat.Bsize)
	return FSSize{Total: stat.Blocks * size, Free: stat.Bfree * size, Available: stat.Bavail * size}, nil
}
