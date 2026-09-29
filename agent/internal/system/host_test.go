package system

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// fakeCommands answers commands by their full command line.
type fakeCommands map[string]fakeResult

type fakeResult struct {
	out string
	err error
}

func (f fakeCommands) run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := name
	for _, arg := range args {
		line += " " + arg
	}
	result, ok := f[line]
	if !ok {
		return nil, errors.New("not found: " + line)
	}
	return []byte(result.out), result.err
}

func TestDetectEnvironment(t *testing.T) {
	exit1 := errors.New("exit status 1")
	for _, test := range []struct {
		out       string
		err       error
		want      string
		container bool
	}{
		{"none\n", exit1, "bare_metal", false},
		{"kvm\n", nil, "vm", false},
		{"microsoft\n", nil, "vm", false},
		{"lxc\n", nil, "lxc", true},
		{"lxc-libvirt\n", nil, "lxc", true},
		{"systemd-nspawn\n", nil, "", true},
		{"", errors.New("not found"), "", false},
	} {
		run := fakeCommands{"systemd-detect-virt": {test.out, test.err}}.run
		env, container := DetectEnvironment(context.Background(), run)
		got := ""
		if env != nil {
			got = *env
		}
		if got != test.want || container != test.container {
			t.Errorf("%q: environment %q container %v, want %q %v", test.out, got, container, test.want, test.container)
		}
	}
}

func TestOSReleaseAndKernel(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "usr", "lib", "os-release"), "ID=fedora\nNAME=\"Fedora Linux\"\nVERSION_ID=42\n# comment\n")
	writeProc(t, root, "sys/kernel/osrelease", "6.15.4-200.fc42.aarch64\n")
	release := ReadOSRelease(root)
	if release["ID"] != "fedora" || release["NAME"] != "Fedora Linux" || release["VERSION_ID"] != "42" {
		t.Errorf("os-release = %v", release)
	}
	if kernel := Kernel(root); kernel == nil || *kernel != "6.15.4-200.fc42.aarch64" {
		t.Errorf("kernel = %v", kernel)
	}
}

func TestLastBoot(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "stat", "cpu  1 0 1 1 0 0 0 0 0 0\nbtime 1790000000\n")
	writeProc(t, root, "uptime", "3600.55 100.00\n")
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	if got := LastBoot(root, false, now); got == nil || *got != "2026-09-21T14:13:20Z" {
		t.Errorf("last boot = %v, want the kernel boot time", got)
	}
	// In LXC, btime is the hypervisor's. The container's uptime is its own.
	if got := LastBoot(root, true, now); got == nil || *got != "2026-09-29T10:59:59Z" {
		t.Errorf("last boot in a container = %v, want now minus uptime", got)
	}
	if got := LastBoot(t.TempDir(), false, now); got != nil {
		t.Errorf("last boot = %v, want null", *got)
	}
}

func TestRebootRequired(t *testing.T) {
	kernels := func(t *testing.T, running string, installed ...string) string {
		root := t.TempDir()
		writeProc(t, root, "sys/kernel/osrelease", running+"\n")
		for _, name := range installed {
			writeFile(t, filepath.Join(root, "boot", "vmlinuz-"+name), "")
		}
		return root
	}

	t.Run("the Debian file says yes", func(t *testing.T) {
		root := kernels(t, "6.1.0-21-amd64", "6.1.0-21-amd64")
		writeFile(t, filepath.Join(root, "run", "reboot-required"), "*** System restart required ***\n")
		if got := RebootRequired(root, false); got != "yes" {
			t.Errorf("got %q, want yes", got)
		}
	})
	t.Run("a newer kernel is installed", func(t *testing.T) {
		// Proxmox: no reboot-required file, but a newer kernel.
		root := kernels(t, "6.8.12-4-pve", "6.8.12-4-pve", "6.8.12-10-pve", "6.5.13-6-pve")
		if got := RebootRequired(root, false); got != "yes" {
			t.Errorf("got %q, want yes", got)
		}
	})
	t.Run("running the newest kernel", func(t *testing.T) {
		root := kernels(t, "6.11.10-300.fc41.x86_64", "6.11.4-301.fc41.x86_64", "6.11.10-300.fc41.x86_64", "0-rescue-abc123")
		if got := RebootRequired(root, false); got != "no" {
			t.Errorf("got %q, want no", got)
		}
	})
	t.Run("Fedora keeps kernels in lib/modules", func(t *testing.T) {
		root := kernels(t, "6.15.4-200.fc42.aarch64")
		writeFile(t, filepath.Join(root, "lib", "modules", "6.15.4-200.fc42.aarch64", "vmlinuz"), "")
		writeFile(t, filepath.Join(root, "lib", "modules", "6.15.9-201.fc42.aarch64", "vmlinuz"), "")
		if got := RebootRequired(root, false); got != "yes" {
			t.Errorf("got %q, want yes", got)
		}
	})
	t.Run("a container does not check kernels", func(t *testing.T) {
		root := kernels(t, "6.8.12-4-pve", "6.8.12-10-pve")
		if got := RebootRequired(root, true); got != "unknown" {
			t.Errorf("got %q, want unknown", got)
		}
		writeFile(t, filepath.Join(root, "run", "reboot-required"), "")
		if got := RebootRequired(root, true); got != "yes" {
			t.Errorf("with the marker file: got %q, want yes", got)
		}
	})
	t.Run("no kernels to compare", func(t *testing.T) {
		root := kernels(t, "6.1.0-21-amd64")
		if got := RebootRequired(root, false); got != "unknown" {
			t.Errorf("got %q, want unknown", got)
		}
	})
}
