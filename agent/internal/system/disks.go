package system

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// localFileSystems are the file system types of real, local disks. Every
// other type is skipped: virtual ones (tmpfs, overlay, proc, squashfs, …)
// and network ones, whose size check can hang when the server is gone.
var localFileSystems = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"bcachefs": true, "f2fs": true, "jfs": true, "reiserfs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "fuseblk": true,
}

// FSSize is the size of one file system in bytes. Available is the free
// space a normal user can use.
type FSSize struct {
	Total, Free, Available uint64
}

// DiskSampler reads the disks group: space per real mount.
type DiskSampler struct {
	root   string
	statfs func(path string) (FSSize, error)
}

// NewDiskSampler reads the mounts from /proc/self/mountinfo under root and
// their sizes with statfs.
func NewDiskSampler(root string, statfs func(path string) (FSSize, error)) *DiskSampler {
	return &DiskSampler{root: root, statfs: statfs}
}

// Sample reads the disks group. Each device is shown once, at its first
// directory mount, so bind mounts are skipped.
func (d *DiskSampler) Sample() protocol.Disks {
	disks := protocol.Disks{Mounts: []protocol.Mount{}}
	for _, mount := range d.readMounts() {
		entry := protocol.Mount{Mount: mount}
		size, err := d.statfs(filepath.Join(d.root, mount))
		if err == nil && size.Free <= size.Total {
			if used := size.Total - size.Free; used+size.Available > 0 {
				entry.UsedPercent = percent(float64(used) / float64(used+size.Available))
			}
			entry.FreeBytes = roundedBytes(size.Available)
			entry.TotalBytes = roundedBytes(size.Total)
		}
		disks.Mounts = append(disks.Mounts, entry)
	}
	return disks
}

// readMounts returns the mount points of real, local file systems.
func (d *DiskSampler) readMounts() []string {
	file, err := os.Open(filepath.Join(d.root, "proc", "self", "mountinfo"))
	if err != nil {
		return nil
	}
	defer file.Close()
	var mounts []string
	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		// ID parent major:minor root mount-point options [tags] - type source options
		before, after, ok := strings.Cut(scanner.Text(), " - ")
		fields, typeFields := strings.Fields(before), strings.Fields(after)
		if !ok || len(fields) < 5 || len(typeFields) < 1 || !localFileSystems[typeFields[0]] {
			continue
		}
		device, mount := fields[2], unescapeMount(fields[4])
		if seen[device] {
			continue
		}
		// A file bind mount (Docker's /etc/hostname) is not a disk.
		if info, err := os.Stat(filepath.Join(d.root, mount)); err == nil && !info.IsDir() {
			continue
		}
		seen[device] = true
		mounts = append(mounts, mount)
	}
	return mounts
}

// unescapeMount decodes the octal escapes (\040 for a space) in mountinfo.
func unescapeMount(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+4 <= len(text) {
			if value, err := strconv.ParseUint(text[i+1:i+4], 8, 8); err == nil {
				out.WriteByte(byte(value))
				i += 3
				continue
			}
		}
		out.WriteByte(text[i])
	}
	return out.String()
}

func roundedBytes(value uint64) *int64 {
	rounded := int64(math.Round(significant(float64(value), 3)))
	return &rounded
}
