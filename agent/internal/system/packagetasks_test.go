package system

import (
	"context"
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

	writeStamp(t, root, "2026-09-28T03:04:05Z\n")
	updates := collector.Sample(context.Background()).AvailableUpdates
	if updates.LastRefresh == nil || *updates.LastRefresh != "2026-09-28T03:04:05Z" {
		t.Errorf("last refresh %v", updates.LastRefresh)
	}

	writeStamp(t, root, "not a time\n")
	if updates := collector.Sample(context.Background()).AvailableUpdates; updates.LastRefresh != nil {
		t.Errorf("unreadable stamp: last refresh %q, want null", *updates.LastRefresh)
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

	writeStamp(t, root, "2026-09-29T12:00:00Z\n")
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
	writeStamp(t, root, "2026-09-29T12:00:00Z\n")
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
