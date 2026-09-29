package system

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/helper"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// fakeHelper is the root helper with fixed answers.
type fakeHelper struct {
	mu            sync.Mutex
	disks         []helper.SmartDisk
	smartErr      error
	containers    helper.Containers
	containersErr error
	containerRead int
	// events feeds WatchContainers; closing a watch makes it fail.
	events  chan struct{}
	watches int
	failNow chan struct{}
}

func newFakeHelper() *fakeHelper {
	return &fakeHelper{
		containers: helper.Containers{Engines: []string{"docker"}, Items: []helper.Container{}},
		events:     make(chan struct{}),
		failNow:    make(chan struct{}),
	}
}

func (f *fakeHelper) ReadSmart(context.Context) ([]helper.SmartDisk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.disks), f.smartErr
}

func (f *fakeHelper) ReadContainers(context.Context) (helper.Containers, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerRead++
	return f.containers, f.containersErr
}

func (f *fakeHelper) WatchContainers(ctx context.Context, changed func()) error {
	f.mu.Lock()
	f.watches++
	f.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.failNow:
			return errors.New("the event stream ended")
		case <-f.events:
			changed()
		}
	}
}

func (f *fakeHelper) set(update func(*fakeHelper)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	update(f)
}

func (f *fakeHelper) reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containerRead
}

func detectWithHelper(t *testing.T, virt string, rootHelper RootHelper) *Host {
	t.Helper()
	run := fakeCommands{"systemd-detect-virt": {out: virt + "\n"}}.run
	statfs := func(string) (FSSize, error) { return FSSize{}, nil }
	return Detect(context.Background(), fakeHost(t), run, statfs, nil, rootHelper, time.Now)
}

func smartDisk(device string, health string, temperature, wear float64) helper.SmartDisk {
	return helper.SmartDisk{Device: device, Health: &health, TemperatureCelsius: &temperature, WearPercent: &wear}
}

func TestSmartAndContainersOnlyWithTheHelper(t *testing.T) {
	withDisks := newFakeHelper()
	withDisks.disks = []helper.SmartDisk{smartDisk("sda", "ok", 30, 1)}
	dockerStopped := newFakeHelper()
	dockerStopped.containers.Error = "docker cannot list its containers"
	noEngine := newFakeHelper()
	noEngine.containers.Engines = []string{}
	broken := newFakeHelper()
	broken.smartErr = errors.New("helper is down")
	broken.containersErr = errors.New("helper is down")

	for _, test := range []struct {
		name       string
		virt       string
		rootHelper RootHelper
		smart      bool
		containers bool
	}{
		{"bare metal with disks and Docker", "none", withDisks, true, true},
		{"a VM without real disks", "kvm", newFakeHelper(), false, true},
		{"LXC hides SMART", "lxc", withDisks, false, true},
		{"no container engine", "none", noEngine, false, false},
		{"Docker installed but stopped", "none", dockerStopped, false, true},
		{"helper not reachable", "none", broken, false, false},
		{"no helper", "none", nil, false, false},
	} {
		host := detectWithHelper(t, test.virt, test.rootHelper)
		if got := slices.Contains(host.Capabilities, CapabilitySmart); got != test.smart {
			t.Errorf("%s: smart = %v, want %v", test.name, got, test.smart)
		}
		if got := slices.Contains(host.Capabilities, CapabilityContainers); got != test.containers {
			t.Errorf("%s: containers = %v, want %v", test.name, got, test.containers)
		}
		groups := NewCollector(host, DefaultIntervals, agentInfo).Sample(context.Background())
		if (groups.Smart != nil) != test.smart || (groups.Containers != nil) != test.containers {
			t.Errorf("%s: smart group %v, containers group %v", test.name, groups.Smart, groups.Containers)
		}
	}
}

func TestSmartGroupRoundsAndKeepsASleepingDisk(t *testing.T) {
	fake := newFakeHelper()
	fake.disks = []helper.SmartDisk{smartDisk("sdb", "failing", 44.6, 0), {Device: "nvme0n1"}, smartDisk("sda", "ok", 31.2, 7.4)}
	fake.disks[0].WearPercent = nil
	fake.disks[1] = smartDisk("nvme0n1", "ok", 39, 14)
	host := detectWithHelper(t, "none", fake)
	collector := NewCollector(host, DefaultIntervals, agentInfo)

	first := collector.Sample(context.Background()).Smart
	want := "[nvme0n1 ok 39 14] [sda ok 31 7] [sdb failing 45 null]"
	if got := describeSmart(first); got != want {
		t.Errorf("first = %s, want %s (sorted, whole numbers)", got, want)
	}

	if _, err := protocol.Encode(&protocol.Delta{ID: "agent-1", Groups: protocol.Groups{Smart: first}}); err != nil {
		t.Errorf("the group breaks the protocol: %v", err)
	}

	// sda falls asleep: it keeps its last values instead of being woken.
	// A disk seen asleep from the start has no values yet.
	fake.set(func(f *fakeHelper) {
		f.disks = []helper.SmartDisk{smartDisk("nvme0n1", "ok", 40, 14), {Device: "sda", Standby: true}, {Device: "sdc", Standby: true}}
	})
	next := collector.smart.Sample(context.Background())
	want = "[nvme0n1 ok 40 14] [sda ok 31 7] [sdc null null null]"
	if got := describeSmart(next); got != want {
		t.Errorf("next = %s, want %s", got, want)
	}

	// The helper fails: no group, so the last one stays in Home Assistant.
	fake.set(func(f *fakeHelper) { f.smartErr = errors.New("helper is down") })
	if group := collector.smart.Sample(context.Background()); group != nil {
		t.Errorf("after an error = %s, want no group", describeSmart(group))
	}
}

func describeSmart(smart *protocol.Smart) string {
	if smart == nil {
		return "none"
	}
	text := ""
	show := func(value any) string {
		switch v := value.(type) {
		case *string:
			if v != nil {
				return *v
			}
		case *float64:
			if v != nil {
				return fmt.Sprint(*v)
			}
		}
		return "null"
	}
	for i, disk := range smart.Disks {
		if i > 0 {
			text += " "
		}
		text += fmt.Sprintf("[%s %s %s %s]", disk.Device, show(disk.Health), show(disk.TemperatureCelsius), show(disk.WearPercent))
	}
	return text
}

// watchContainers runs the watcher and returns what it published.
func watchContainers(t *testing.T, rootHelper RootHelper, minGap, fullEvery, retry time.Duration) func() []protocol.Containers {
	t.Helper()
	var mu sync.Mutex
	var published []protocol.Containers
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		WatchContainers(ctx, rootHelper, minGap, fullEvery, retry, func(group protocol.Containers) {
			mu.Lock()
			defer mu.Unlock()
			published = append(published, group)
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	return func() []protocol.Containers {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(published)
	}
}

func TestContainersAtStartAndOnEvents(t *testing.T) {
	fake := newFakeHelper()
	fake.containers.Items = []helper.Container{{Name: "db", State: "unhealthy"}, {Name: "old", State: "stopped"}, {Name: "web", State: "running"}}
	published := watchContainers(t, fake, time.Millisecond, time.Hour, time.Hour)
	waitFor(t, func() bool { return len(published()) == 1 })
	first := published()[0]
	if *first.Count != 3 || *first.Running != 1 || *first.Stopped != 1 || *first.Unhealthy != 1 {
		t.Errorf("first = %+v", first)
	}
	if !slices.Equal(first.Items, []protocol.Container{{Name: "db", State: "unhealthy"}, {Name: "old", State: "stopped"}, {Name: "web", State: "running"}}) {
		t.Errorf("items = %+v", first.Items)
	}

	fake.set(func(f *fakeHelper) { f.containers.Items = f.containers.Items[2:] })
	fake.events <- struct{}{}
	waitFor(t, func() bool { return len(published()) == 2 })
	if second := published()[1]; *second.Count != 1 || *second.Running != 1 || len(second.Items) != 1 {
		t.Errorf("second = %+v", second)
	}
}

func TestContainersAtMostOncePerGap(t *testing.T) {
	fake := newFakeHelper()
	const gap = 200 * time.Millisecond
	watchContainers(t, fake, gap, time.Hour, time.Hour)
	waitFor(t, func() bool { return fake.reads() == 1 })
	for range 20 {
		fake.events <- struct{}{}
	}
	waitFor(t, func() bool { return fake.reads() == 2 })
	time.Sleep(gap + 100*time.Millisecond)
	if got := fake.reads(); got != 2 {
		t.Errorf("reads = %d, want 2", got)
	}
}

func TestContainersFullCheckWithoutEvents(t *testing.T) {
	fake := newFakeHelper()
	watchContainers(t, fake, time.Millisecond, 30*time.Millisecond, time.Hour)
	waitFor(t, func() bool { return fake.reads() >= 3 })
}

func TestContainersWatchStartsAgainAfterItFails(t *testing.T) {
	fake := newFakeHelper()
	watchContainers(t, fake, time.Millisecond, time.Hour, 10*time.Millisecond)
	waitFor(t, func() bool { return fake.reads() == 1 })
	fake.failNow <- struct{}{}
	// Events may be lost while the watch is down, so it reads again.
	waitFor(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.watches == 2 && fake.containerRead == 2
	})
}

func TestContainersCapAndError(t *testing.T) {
	fake := newFakeHelper()
	for i := range 120 {
		fake.containers.Items = append(fake.containers.Items, helper.Container{Name: fmt.Sprintf("app-%03d", i), State: "running"})
	}
	published := watchContainers(t, fake, time.Millisecond, time.Hour, time.Hour)
	waitFor(t, func() bool { return len(published()) == 1 })
	group := published()[0]
	if *group.Count != 120 || *group.Running != 120 || len(group.Items) != 100 {
		t.Errorf("count %d, running %d, %d items; want 120, 120, 100", *group.Count, *group.Running, len(group.Items))
	}
	if _, err := protocol.Encode(&protocol.Delta{ID: "agent-1", Groups: protocol.Groups{Containers: &group}}); err != nil {
		t.Errorf("the group breaks the protocol: %v", err)
	}

	fake.set(func(f *fakeHelper) { f.containersErr = errors.New("the helper is down") })
	fake.events <- struct{}{}
	waitFor(t, func() bool { return len(published()) == 2 })
	fake.set(func(f *fakeHelper) { f.containersErr = nil; f.containers.Error = "docker is not running" })
	fake.events <- struct{}{}
	waitFor(t, func() bool { return len(published()) == 3 })
	if group := published()[2]; group.Count != nil || len(group.Items) != 0 {
		t.Errorf("with an engine error = %+v, want null counts", group)
	}
	if group := published()[1]; group.Count != nil || group.Running != nil || group.Stopped != nil || group.Unhealthy != nil || group.Items == nil || len(group.Items) != 0 {
		t.Errorf("after an error = %+v, want null counts and an empty list", group)
	}
}
