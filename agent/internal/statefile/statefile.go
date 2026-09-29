// Package statefile writes files in the Agent's state directory.
package statefile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Write replaces path with data in one step, so a reader never sees half a
// file. The new file gets perm and the owner of its directory: root runs some
// owner commands, and the network part must still read what they write. A
// symlink at path is replaced, never followed.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read the owner of " + dir)
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(perm); err != nil {
		temp.Close()
		return err
	}
	if os.Geteuid() == 0 {
		if err := temp.Chown(int(owner.Uid), int(owner.Gid)); err != nil {
			temp.Close()
			return err
		}
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
