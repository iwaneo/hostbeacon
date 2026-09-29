package system

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Command runs a program without a shell and returns its standard output.
type Command func(ctx context.Context, name string, args ...string) ([]byte, error)

// containerTypes are the systemd-detect-virt answers for a container.
var containerTypes = []string{"lxc", "lxc-libvirt", "systemd-nspawn", "docker", "podman", "rkt", "wsl", "proot", "pouch", "openvz"}

// DetectEnvironment asks systemd-detect-virt. It returns bare_metal, vm,
// lxc, or null, and whether the Host is any kind of container.
func DetectEnvironment(ctx context.Context, run Command) (environment *string, container bool) {
	out, _ := run(ctx, "systemd-detect-virt") // exits 1 when it prints "none"
	kind := strings.TrimSpace(string(out))
	var value string
	switch {
	case kind == "":
		return nil, false
	case kind == "none":
		value = "bare_metal"
	case kind == "lxc" || kind == "lxc-libvirt":
		value = "lxc"
	case slices.Contains(containerTypes, kind):
		return nil, true
	default:
		value = "vm"
	}
	return &value, value == "lxc"
}

// ReadOSRelease reads /etc/os-release, or /usr/lib/os-release without it.
func ReadOSRelease(root string) map[string]string {
	values := map[string]string{}
	file, err := os.Open(filepath.Join(root, "etc", "os-release"))
	if err != nil {
		if file, err = os.Open(filepath.Join(root, "usr", "lib", "os-release")); err != nil {
			return values
		}
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if name, value, ok := strings.Cut(scanner.Text(), "="); ok && !strings.HasPrefix(name, "#") {
			values[name] = strings.Trim(value, `"'`)
		}
	}
	return values
}

// Kernel is the running kernel release.
func Kernel(root string) *string {
	if kernel := readTrimmed(filepath.Join(root, "proc", "sys", "kernel", "osrelease")); kernel != "" {
		return &kernel
	}
	return nil
}

// Hostname is the Host's current hostname.
func Hostname(root string) string {
	return readTrimmed(filepath.Join(root, "proc", "sys", "kernel", "hostname"))
}

// LastBoot is the boot time in UTC. In a container, the kernel's boot time
// is the hypervisor's, so it is now minus the container's uptime.
func LastBoot(root string, container bool, now time.Time) *string {
	var boot time.Time
	if container {
		fields := strings.Fields(readTrimmed(filepath.Join(root, "proc", "uptime")))
		if len(fields) == 0 {
			return nil
		}
		seconds, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || seconds < 0 {
			return nil
		}
		boot = now.Add(-time.Duration(seconds * float64(time.Second))).Truncate(time.Second)
	} else {
		data, _ := os.ReadFile(filepath.Join(root, "proc", "stat"))
		for line := range strings.Lines(string(data)) {
			if value, ok := strings.CutPrefix(line, "btime "); ok {
				if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
					boot = time.Unix(seconds, 0)
				}
			}
		}
		if boot.IsZero() {
			return nil
		}
	}
	text := boot.UTC().Format(time.RFC3339)
	return &text
}

// RebootRequired is yes, no, or unknown. Yes when the Debian and Ubuntu
// marker file exists, or when a newer kernel than the running one is
// installed. A container runs the hypervisor's kernel, so only the marker
// file counts there. Without the marker file and without kernels to compare,
// it is unknown: not every Host writes the marker file (Proxmox does not).
func RebootRequired(root string, container bool) string {
	if exists(filepath.Join(root, "run", "reboot-required")) {
		return "yes"
	}
	if running := Kernel(root); !container && running != nil {
		if installed := installedKernels(root); len(installed) > 0 {
			newest := slices.MaxFunc(installed, compareVersions)
			if compareVersions(newest, *running) > 0 {
				return "yes"
			}
			return "no"
		}
	}
	return "unknown"
}

// installedKernels lists kernel releases from /boot/vmlinuz-<release> and
// /lib/modules/<release>/vmlinuz. Rescue images are skipped.
func installedKernels(root string) []string {
	var releases []string
	images, _ := filepath.Glob(filepath.Join(root, "boot", "vmlinuz-*"))
	for _, image := range images {
		releases = append(releases, strings.TrimPrefix(filepath.Base(image), "vmlinuz-"))
	}
	for _, dir := range []string{"lib", "usr/lib"} {
		images, _ := filepath.Glob(filepath.Join(root, dir, "modules", "*", "vmlinuz"))
		for _, image := range images {
			releases = append(releases, filepath.Base(filepath.Dir(image)))
		}
	}
	return slices.DeleteFunc(releases, func(release string) bool { return strings.Contains(release, "rescue") })
}

// compareVersions compares kernel releases part by part: runs of digits as
// numbers, other text as text. So 6.8.12-10-pve is newer than 6.8.12-4-pve.
func compareVersions(a, b string) int {
	for a != "" && b != "" {
		partA, restA := nextPart(a)
		partB, restB := nextPart(b)
		numberA, errA := strconv.ParseUint(partA, 10, 64)
		numberB, errB := strconv.ParseUint(partB, 10, 64)
		switch {
		case errA == nil && errB == nil && numberA != numberB:
			if numberA < numberB {
				return -1
			}
			return 1
		case (errA != nil || errB != nil) && partA != partB:
			return strings.Compare(partA, partB)
		}
		a, b = restA, restB
	}
	return strings.Compare(a, b)
}

func nextPart(text string) (part, rest string) {
	digit := unicode.IsDigit(rune(text[0]))
	end := strings.IndexFunc(text, func(r rune) bool { return unicode.IsDigit(r) != digit })
	if end < 0 {
		return text, ""
	}
	return text[:end], text[end:]
}
