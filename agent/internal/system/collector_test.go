package system

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// fakeHost makes a Host root with every data source, like a Debian server.
func fakeHost(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeProc(t, root, "stat", "cpu  100 0 100 700 100 0 0 0 0 0\nbtime 1790000000\n")
	writeProc(t, root, "meminfo", meminfo)
	writeProc(t, root, "loadavg", "0.50 0.40 0.30 1/100 1000\n")
	writeProc(t, root, "uptime", "60.00 10.00\n")
	writeProc(t, root, "self/mountinfo", "22 1 259:2 / / rw - ext4 /dev/sda1 rw\n26 22 0:25 / /run rw - tmpfs tmpfs rw\n")
	writeProc(t, root, "sys/kernel/hostname", "debian-test\n")
	writeProc(t, root, "sys/kernel/osrelease", "6.12.48+deb13-arm64\n")
	writeFile(t, filepath.Join(root, "etc", "os-release"), "ID=debian\nNAME=\"Debian GNU/Linux\"\nVERSION_ID=\"13\"\n")
	writeFile(t, filepath.Join(root, "usr", "bin", "apt-get"), "")
	addInterface(t, root, "eth0", true, 1, 100, 100)
	addHwmon(t, root, "hwmon0", "coretemp", map[string]string{"temp1": "45000"}, map[string]string{"temp1": "Package id 0"})
	return root
}

func detect(t *testing.T, root, virt string, services ServiceSource) *Host {
	t.Helper()
	run := fakeCommands{
		"systemd-detect-virt": {out: virt + "\n"},
		aptList:               {out: "Listing...\nbash/stable 5.2.37-2+b5 arm64 [upgradable from: 5.2.37-2]\n"},
	}.run
	statfs := func(string) (FSSize, error) { return FSSize{Total: 1000, Free: 500, Available: 500}, nil }
	now := func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }
	return Detect(context.Background(), root, run, statfs, services, now)
}

var agentInfo = protocol.AgentInfo{Hostname: "old-name", AgentVersion: "0.1.0", Capabilities: []string{}, EnabledActions: []protocol.Action{}}

func TestBareMetalHasEveryGroup(t *testing.T) {
	host := detect(t, fakeHost(t), "none", newFakeServices("x.service"))
	want := []string{"load", "temperatures", "disks", "network", "failed_services", "available_updates"}
	if !slices.Equal(host.Capabilities, want) {
		t.Errorf("capabilities = %q, want %q", host.Capabilities, want)
	}
	if *host.Environment != "bare_metal" || *host.Kernel != "6.12.48+deb13-arm64" || host.Release["VERSION_ID"] != "13" {
		t.Errorf("environment %v, kernel %v, release %v", *host.Environment, *host.Kernel, host.Release)
	}

	groups := NewCollector(host, DefaultIntervals, agentInfo).Sample(context.Background())
	if groups.Agent.Hostname != "debian-test" || groups.Agent.AgentVersion != "0.1.0" {
		t.Errorf("agent = %+v, want the current hostname", groups.Agent)
	}
	if groups.System.Load1 == nil || *groups.System.Load1 != 0.5 {
		t.Errorf("load = %v", groups.System.Load1)
	}
	if groups.Temperatures == nil || *groups.Temperatures.CPUCelsius != 45 {
		t.Errorf("temperatures = %+v", groups.Temperatures)
	}
	if len(groups.Disks.Mounts) != 1 || len(groups.Network.Interfaces) != 1 {
		t.Errorf("disks %+v, network %+v", groups.Disks, groups.Network)
	}
	if *groups.FailedServices.Count != 1 || *groups.AvailableUpdates.Count != 1 {
		t.Errorf("failed services %+v, updates %+v", groups.FailedServices, groups.AvailableUpdates)
	}
	if groups.Flags.RebootRequired != "unknown" || *groups.Flags.LastBoot != "2026-09-21T14:13:20Z" {
		t.Errorf("flags = %+v", groups.Flags)
	}
}

func TestLXCHidesLoadAndTemperature(t *testing.T) {
	host := detect(t, fakeHost(t), "lxc", nil)
	if slices.Contains(host.Capabilities, "load") || slices.Contains(host.Capabilities, "temperatures") {
		t.Errorf("capabilities = %q, want no load and no temperatures", host.Capabilities)
	}
	if slices.Contains(host.Capabilities, "failed_services") {
		t.Error("failed_services without systemd")
	}
	groups := NewCollector(host, DefaultIntervals, agentInfo).Sample(context.Background())
	if groups.System.Load1 != nil || groups.Temperatures != nil || groups.FailedServices != nil {
		t.Errorf("load %v, temperatures %+v, failed services %+v; want none", groups.System.Load1, groups.Temperatures, groups.FailedServices)
	}
	// The container's own boot: now minus its uptime.
	if *groups.Flags.LastBoot != "2026-09-29T11:59:00Z" {
		t.Errorf("last boot = %s", *groups.Flags.LastBoot)
	}
}

func TestRunPublishesOnIntervals(t *testing.T) {
	root := fakeHost(t)
	host := detect(t, root, "none", nil)
	intervals := Intervals{
		System: 10 * time.Millisecond, Network: 10 * time.Millisecond, Disks: 10 * time.Millisecond,
		Temperatures: 10 * time.Millisecond, RebootRequired: 10 * time.Millisecond, AvailableUpdates: 10 * time.Millisecond,
		ServicesGap: time.Millisecond, ServicesFull: time.Hour,
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	NewCollector(host, intervals, agentInfo).Run(ctx, func(groups protocol.Groups) {
		mu.Lock()
		defer mu.Unlock()
		for name, present := range map[string]bool{
			"system": groups.System != nil, "network": groups.Network != nil, "disks": groups.Disks != nil,
			"temperatures": groups.Temperatures != nil, "flags": groups.Flags != nil, "available_updates": groups.AvailableUpdates != nil,
		} {
			seen[name] = seen[name] || present
		}
	})
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return !slices.Contains(slices.Collect(maps.Values(seen)), false)
	})
}
