package system

import (
	"context"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/helper"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// fakeTasks is systemd with the package list refresh unit.
type fakeTasks struct {
	mu      sync.Mutex
	running bool
	changes chan struct{}
}

func newFakeTasks() *fakeTasks { return &fakeTasks{changes: make(chan struct{}, 1)} }

func (f *fakeTasks) PackageTaskRunning(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running, nil
}

func (f *fakeTasks) PackageTaskChanges() <-chan struct{} { return f.changes }

func (f *fakeTasks) set(running bool) {
	f.mu.Lock()
	f.running = running
	f.mu.Unlock()
	f.changes <- struct{}{}
}

func writeStamp(t *testing.T, root, text string) {
	t.Helper()
	writeFile(t, filepath.Join(root, helper.DefaultPackageListStamp), text)
}

func TestAvailableUpdatesCarryTheLastRefreshTime(t *testing.T) {
	root := fakeHost(t)
	host := detect(t, root, "none", nil)
	collector := NewCollector(host, DefaultIntervals, agentInfo)
	if updates := collector.Sample(context.Background()).AvailableUpdates; updates.LastRefresh != nil {
		t.Errorf("never refreshed: last refresh %q, want null", *updates.LastRefresh)
	}

	writeStamp(t, root, `{"format":1,"refreshed_at":"2026-09-28T03:04:05Z"}`)
	updates := collector.Sample(context.Background()).AvailableUpdates
	if updates.LastRefresh == nil || *updates.LastRefresh != "2026-09-28T03:04:05Z" {
		t.Errorf("last refresh %v", updates.LastRefresh)
	}

	writeStamp(t, root, "not a time\n")
	if updates := collector.Sample(context.Background()).AvailableUpdates; updates.LastRefresh != nil {
		t.Errorf("unreadable stamp: last refresh %q, want null", *updates.LastRefresh)
	}
}

func TestAvailableUpdatesCarryTheRefreshSchedule(t *testing.T) {
	host := detect(t, fakeHost(t), "none", nil)
	collector := NewCollector(host, DefaultIntervals, agentInfo)
	if updates := collector.Sample(context.Background()).AvailableUpdates; updates.RefreshSchedule != nil {
		t.Errorf("unknown schedule: %q, want it left out", *updates.RefreshSchedule)
	}
	host.RefreshSchedule = "03:00"
	if updates := collector.Sample(context.Background()).AvailableUpdates; updates.RefreshSchedule == nil || *updates.RefreshSchedule != "03:00" {
		t.Errorf("refresh schedule %v, want 03:00", updates.RefreshSchedule)
	}
}

func TestPackageTaskRunningFlag(t *testing.T) {
	host := detect(t, fakeHost(t), "none", nil)
	collector := NewCollector(host, DefaultIntervals, agentInfo)
	if collector.Sample(context.Background()).Flags.PackageTaskRunning {
		t.Error("running without systemd")
	}
	tasks := newFakeTasks()
	host.Tasks = tasks
	tasks.running = true
	if !collector.Sample(context.Background()).Flags.PackageTaskRunning {
		t.Error("not running while the refresh unit runs")
	}
}

func TestEndOfAPackageTaskRereadsAvailableUpdates(t *testing.T) {
	root := fakeHost(t)
	host := detect(t, root, "none", nil)
	tasks := newFakeTasks()
	host.Tasks = tasks
	long := Intervals{
		System: time.Hour, Network: time.Hour, Disks: time.Hour, Temperatures: time.Hour,
		RebootRequired: time.Hour, AvailableUpdates: time.Hour, ServicesGap: time.Hour, ServicesFull: time.Hour,
	}
	var mu sync.Mutex
	var published []protocol.Groups
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	NewCollector(host, long, agentInfo).Run(ctx, func(groups protocol.Groups) {
		mu.Lock()
		defer mu.Unlock()
		published = append(published, groups)
	})
	last := func(check func(protocol.Groups) bool) func() bool {
		return func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(published) > 0 && check(published[len(published)-1])
		}
	}

	tasks.set(true)
	waitFor(t, last(func(g protocol.Groups) bool { return g.Flags != nil && g.Flags.PackageTaskRunning }))

	writeStamp(t, root, `{"format":1,"refreshed_at":"2026-09-29T12:00:00Z"}`)
	tasks.set(false)
	waitFor(t, last(func(g protocol.Groups) bool {
		return g.AvailableUpdates != nil && g.AvailableUpdates.LastRefresh != nil && *g.AvailableUpdates.LastRefresh == "2026-09-29T12:00:00Z"
	}))
	mu.Lock()
	defer mu.Unlock()
	if flags := published[len(published)-2].Flags; flags == nil || flags.PackageTaskRunning {
		t.Errorf("flags before the re-read: %+v, want not running", flags)
	}
}

func TestPackageTaskUnitPath(t *testing.T) {
	if got := unitPath("hostbeacon-package-list-refresh.service"); got != "/org/freedesktop/systemd1/unit/hostbeacon_2dpackage_2dlist_2drefresh_2eservice" {
		t.Errorf("path %q", got)
	}
}

func TestTurnWithoutARefreshDoesNotRereadAvailableUpdates(t *testing.T) {
	root := fakeHost(t)
	writeStamp(t, root, `{"format":1,"refreshed_at":"2026-09-29T12:00:00Z"}`)
	host := detect(t, root, "none", nil)
	tasks := newFakeTasks()
	host.Tasks = tasks
	long := Intervals{
		System: time.Hour, Network: time.Hour, Disks: time.Hour, Temperatures: time.Hour,
		RebootRequired: time.Hour, AvailableUpdates: time.Hour, ServicesGap: time.Hour, ServicesFull: time.Hour,
	}
	var mu sync.Mutex
	var flags, updates int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collector := NewCollector(host, long, agentInfo)
	collector.Sample(ctx)
	collector.Run(ctx, func(groups protocol.Groups) {
		mu.Lock()
		defer mu.Unlock()
		if groups.Flags != nil {
			flags++
		}
		if groups.AvailableUpdates != nil {
			updates++
		}
	})
	// The hourly turn found the list fresh: the stamp did not change.
	tasks.set(true)
	tasks.set(false)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return flags == 2
	})
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if updates != 0 {
		t.Errorf("Available updates re-read %d times, want none", updates)
	}
}

// exitError is the error of a program that exited with a failure.
func exitError(t *testing.T) error {
	t.Helper()
	err := exec.Command("false").Run()
	if err == nil {
		t.Fatal("false succeeded")
	}
	return err
}

func TestPackageSystemBroken(t *testing.T) {
	audit, check, dnfCheck := "dpkg --audit", "apt-get check -qq -o Debug::NoLocking=1", "dnf -q check"
	for _, test := range []struct {
		name     string
		manager  string
		commands fakeCommands
		broken   bool
		fix      string
		ok       bool
	}{
		{"apt fine", "apt", fakeCommands{audit: {}, check: {}}, false, "", true},
		{"half installed", "apt", fakeCommands{audit: {out: "The following packages are only half configured:\n libfoo1\n"}, check: {}}, true, "sudo dpkg --configure -a", true},
		{"audit fails", "apt", fakeCommands{audit: {err: exitError(t)}, check: {}}, true, "sudo dpkg --configure -a", true},
		{"unmet dependencies", "apt", fakeCommands{audit: {}, check: {err: exitError(t)}}, true, "sudo apt-get --fix-broken install", true},
		{"cannot tell", "apt", fakeCommands{}, false, "", false},
		{"dnf fine", "dnf", fakeCommands{dnfCheck: {}}, false, "", true},
		{"dnf problems", "dnf", fakeCommands{dnfCheck: {err: exitError(t)}}, true, "sudo dnf check", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			broken, fix, ok := PackageSystemBroken(context.Background(), test.commands.run, test.manager)
			if broken != test.broken || ok != test.ok || (fix == nil) != (test.fix == "") || fix != nil && *fix != test.fix {
				t.Errorf("broken %v, fix %v, ok %v", broken, fix, ok)
			}
		})
	}
}

func TestBrokenPackageSystemIsAFlagAndIsNotCheckedDuringAPackageTask(t *testing.T) {
	root := fakeHost(t)
	host := detect(t, root, "none", nil)
	commands := fakeCommands{"dpkg --audit": {out: "half configured: libfoo1\n"}}
	host.Run = commands.run
	collector := NewCollector(host, DefaultIntervals, agentInfo)
	flags := collector.Sample(context.Background()).Flags
	if !flags.PackageSystemBroken || flags.PackageSystemFixCommand == nil || *flags.PackageSystemFixCommand != "sudo dpkg --configure -a" {
		t.Fatalf("flags %+v", flags)
	}

	// During an Update run, packages are half installed on purpose.
	tasks := newFakeTasks()
	tasks.running = true
	host.Tasks = tasks
	commands["dpkg --audit"] = fakeResult{}
	if flags := collector.Sample(context.Background()).Flags; !flags.PackageSystemBroken {
		t.Error("checked during a package task")
	}
	tasks.running = false
	commands["apt-get check -qq -o Debug::NoLocking=1"] = fakeResult{}
	if flags := collector.Sample(context.Background()).Flags; flags.PackageSystemBroken || flags.PackageSystemFixCommand != nil {
		t.Errorf("flags after the fix %+v", flags)
	}
}

func writeRunRecord(t *testing.T, root, text string) {
	t.Helper()
	writeFile(t, filepath.Join(root, helper.DefaultUpdateRunRecord), text)
}

func TestUpdateRunGroupComesFromTheRecord(t *testing.T) {
	root := fakeHost(t)
	collector := NewCollector(detect(t, root, "none", nil), DefaultIntervals, agentInfo)
	if run := collector.Sample(context.Background()).UpdateRun; run == nil || run.State != "idle" || run.RunID != nil {
		t.Fatalf("no run yet: %+v", run)
	}
	writeRunRecord(t, root, `{"format":1,"action_id":"d4c3b2a1-9f8e-4d7c-b6a5-493827160a5b","run_id":"0a5b4c3d-2e1f-4a0b-9c8d-7e6f5a4b3c2d","state":"running","percent":40,"started_at":"2026-09-29T12:00:00Z","needs_manual_update":[],"logged":false,"added_later":1}`)
	run := collector.Sample(context.Background()).UpdateRun
	if run == nil || run.State != "running" || *run.Percent != 40 || *run.RunID != "0a5b4c3d-2e1f-4a0b-9c8d-7e6f5a4b3c2d" || *run.StartedAt != "2026-09-29T12:00:00Z" {
		t.Errorf("running: %+v", run)
	}
	writeRunRecord(t, root, "not json")
	if run := collector.Sample(context.Background()).UpdateRun; run != nil {
		t.Errorf("unreadable record: %+v, want no group", run)
	}
}

func TestFinishedUpdateRunRereadsAvailableUpdatesAndFlags(t *testing.T) {
	root := fakeHost(t)
	host := detect(t, root, "none", nil)
	intervals := Intervals{
		System: time.Hour, Network: time.Hour, Disks: time.Hour, Temperatures: time.Hour,
		RebootRequired: time.Hour, AvailableUpdates: time.Hour, ServicesGap: time.Hour, ServicesFull: time.Hour,
		UpdateRun: 10 * time.Millisecond,
	}
	collector := NewCollector(host, intervals, agentInfo)
	collector.Sample(context.Background())
	var mu sync.Mutex
	var published []protocol.Groups
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collector.Run(ctx, func(groups protocol.Groups) {
		mu.Lock()
		defer mu.Unlock()
		published = append(published, groups)
	})
	count := func(has func(protocol.Groups) bool) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, groups := range published {
			if has(groups) {
				n++
			}
		}
		return n
	}
	isUpdates := func(g protocol.Groups) bool { return g.AvailableUpdates != nil }
	isFlags := func(g protocol.Groups) bool { return g.Flags != nil }

	writeRunRecord(t, root, `{"format":1,"action_id":"d4c3b2a1-9f8e-4d7c-b6a5-493827160a5b","run_id":"0a5b4c3d-2e1f-4a0b-9c8d-7e6f5a4b3c2d","state":"running","started_at":"2026-09-29T12:00:00Z","needs_manual_update":[]}`)
	waitFor(t, func() bool {
		return count(func(g protocol.Groups) bool { return g.UpdateRun != nil && g.UpdateRun.State == "running" }) > 0
	})
	if count(isUpdates) != 0 {
		t.Error("re-read Available updates while the run runs")
	}
	writeRunRecord(t, root, `{"format":1,"action_id":"d4c3b2a1-9f8e-4d7c-b6a5-493827160a5b","run_id":"0a5b4c3d-2e1f-4a0b-9c8d-7e6f5a4b3c2d","state":"finished","started_at":"2026-09-29T12:00:00Z","finished_at":"2026-09-29T12:05:00Z","result":"ok","installed":1,"remaining":0,"needs_manual_update":[]}`)
	waitFor(t, func() bool { return count(isUpdates) == 1 && count(isFlags) == 1 })
	time.Sleep(50 * time.Millisecond)
	if count(isUpdates) != 1 {
		t.Error("re-read Available updates more than once for one finished run")
	}
}
