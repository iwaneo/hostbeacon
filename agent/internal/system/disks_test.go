package system

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// A mountinfo from a Host with Docker, snaps, a bind mount, and a mount
// point with a space in its name.
const mountinfo = `22 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw
23 22 0:21 / /proc rw,nosuid shared:12 - proc proc rw
24 22 0:22 / /sys rw,nosuid shared:7 - sysfs sysfs rw
25 22 0:5 / /dev rw,nosuid shared:2 - devtmpfs udev rw
26 22 0:25 / /run rw,nosuid shared:5 - tmpfs tmpfs rw
27 22 259:1 / /boot/efi rw,relatime shared:30 - vfat /dev/nvme0n1p1 rw
28 22 8:1 / /srv/big\040disk rw,relatime shared:31 - xfs /dev/sda1 rw
29 22 0:30 / /var/lib/docker/overlay2/abc/merged rw shared:40 - overlay overlay rw
30 22 7:0 / /snap/core/1 ro shared:41 - squashfs /dev/loop0 ro
31 22 259:2 /srv/data /mnt/data rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw
32 26 0:40 / /run/user/1000 rw shared:50 - tmpfs tmpfs rw
33 22 0:41 / /mnt/nas rw shared:51 - nfs4 nas:/export rw
34 22 0:42 / /home rw,relatime shared:52 - btrfs /dev/sdb1 rw
35 22 254:1 /docker/containers/abc/hostname /etc/hostname rw - ext4 /dev/vda1 rw
`

func TestDisksSkipVirtualAndRepeatedMounts(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "self/mountinfo", mountinfo)
	// A file bind mount, as Docker makes for /etc/hostname, is not a disk.
	writeFile(t, filepath.Join(root, "etc", "hostname"), "host\n")
	sizes := map[string]FSSize{
		root:                                {Total: 100_000_000_000, Free: 30_000_000_000, Available: 25_000_000_000},
		filepath.Join(root, "boot/efi"):     {Total: 536_000_000, Free: 500_000_000, Available: 500_000_000},
		filepath.Join(root, "srv/big disk"): {Total: 4_000_000_000_000, Free: 1_234_567_890_123, Available: 1_234_567_890_123},
		filepath.Join(root, "home"):         {},
	}
	sampler := NewDiskSampler(root, func(path string) (FSSize, error) {
		size, ok := sizes[path]
		if !ok {
			return FSSize{}, errors.New("unexpected statfs of " + path)
		}
		return size, nil
	})
	disks := sampler.Sample()

	var mounts []string
	for _, mount := range disks.Mounts {
		mounts = append(mounts, mount.Mount)
	}
	want := []string{"/", "/boot/efi", "/srv/big disk", "/home"}
	if len(mounts) != len(want) {
		t.Fatalf("mounts = %q, want %q", mounts, want)
	}
	for i := range want {
		if mounts[i] != want[i] {
			t.Fatalf("mounts = %q, want %q", mounts, want)
		}
	}

	rootMount := disks.Mounts[0]
	// Used 70 of 95 GB that a user can use: 73.7 %, as df shows it.
	if rootMount.UsedPercent == nil || *rootMount.UsedPercent != 74 {
		t.Errorf("/ used = %v, want 74", rootMount.UsedPercent)
	}
	if rootMount.FreeBytes == nil || *rootMount.FreeBytes != 25_000_000_000 || *rootMount.TotalBytes != 100_000_000_000 {
		t.Errorf("/ free and total = %v %v", rootMount.FreeBytes, rootMount.TotalBytes)
	}
	if big := disks.Mounts[2]; *big.FreeBytes != 1_230_000_000_000 {
		t.Errorf("free = %d, want 1230000000000 (3 significant digits)", *big.FreeBytes)
	}
	if home := disks.Mounts[3]; home.UsedPercent != nil || home.FreeBytes == nil || *home.FreeBytes != 0 {
		t.Errorf("empty file system = %+v, want used null and free 0", home)
	}
}

func TestDiskThatCannotBeReadIsNull(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "self/mountinfo", "22 1 259:2 / / rw - ext4 /dev/sda1 rw\n")
	disks := NewDiskSampler(root, func(string) (FSSize, error) { return FSSize{}, errors.New("gone") }).Sample()
	want := protocol.Mount{Mount: "/"}
	if len(disks.Mounts) != 1 || disks.Mounts[0] != want {
		t.Errorf("mounts = %+v, want / with null values", disks.Mounts)
	}
}

func TestNoMountinfoGivesNoMounts(t *testing.T) {
	disks := NewDiskSampler(t.TempDir(), nil).Sample()
	if disks.Mounts == nil || len(disks.Mounts) != 0 {
		t.Errorf("mounts = %#v, want an empty list", disks.Mounts)
	}
}
