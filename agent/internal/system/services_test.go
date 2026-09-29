package system

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// fakeServices is systemd with a list of failed services.
type fakeServices struct {
	mu      sync.Mutex
	failed  []string
	err     error
	reads   []time.Time
	changes chan struct{}
}

func newFakeServices(failed ...string) *fakeServices {
	return &fakeServices{failed: failed, changes: make(chan struct{}, 1)}
}

func (f *fakeServices) Failed(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, time.Now())
	return append([]string(nil), f.failed...), f.err
}

func (f *fakeServices) Changes() <-chan struct{} { return f.changes }

func (f *fakeServices) signal() {
	select {
	case f.changes <- struct{}{}:
	default:
	}
}

func (f *fakeServices) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

// collect runs the watcher and returns what it published.
func collect(t *testing.T, source ServiceSource, minGap, fullEvery time.Duration) (func() []protocol.NameList, context.CancelFunc) {
	t.Helper()
	var mu sync.Mutex
	var published []protocol.NameList
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		WatchFailedServices(ctx, source, minGap, fullEvery, func(list protocol.NameList) {
			mu.Lock()
			defer mu.Unlock()
			published = append(published, list)
		})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return func() []protocol.NameList {
		mu.Lock()
		defer mu.Unlock()
		return append([]protocol.NameList(nil), published...)
	}, cancel
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFailedServicesAtStartAndOnChange(t *testing.T) {
	source := newFakeServices("b.service", "a.service")
	published, _ := collect(t, source, 10*time.Millisecond, time.Hour)
	waitFor(t, func() bool { return len(published()) == 1 })
	first := published()[0]
	if first.Count == nil || *first.Count != 2 || first.Names[0] != "a.service" || first.Names[1] != "b.service" {
		t.Errorf("first = %+v, want 2 names, sorted", first)
	}

	source.mu.Lock()
	source.failed = nil
	source.mu.Unlock()
	source.signal()
	waitFor(t, func() bool { return len(published()) == 2 })
	if second := published()[1]; *second.Count != 0 || second.Names == nil {
		t.Errorf("second = %+v, want count 0 and an empty list", second)
	}
}

func TestFailedServicesAtMostOncePerGap(t *testing.T) {
	source := newFakeServices("a.service")
	const gap = 200 * time.Millisecond
	_, _ = collect(t, source, gap, time.Hour)
	waitFor(t, func() bool { return source.readCount() == 1 })

	// A burst of signals: one read after the gap, not one per signal.
	for range 20 {
		source.signal()
		time.Sleep(5 * time.Millisecond)
	}
	waitFor(t, func() bool { return source.readCount() == 2 })
	time.Sleep(gap + 100*time.Millisecond)
	if got := source.readCount(); got != 2 {
		t.Errorf("reads = %d, want 2", got)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if between := source.reads[1].Sub(source.reads[0]); between < gap {
		t.Errorf("reads %v apart, want at least %v", between, gap)
	}
}

func TestFailedServicesFullCheckWithoutSignals(t *testing.T) {
	source := newFakeServices()
	_, _ = collect(t, source, time.Millisecond, 30*time.Millisecond)
	waitFor(t, func() bool { return source.readCount() >= 3 })
}

func TestFailedServicesCapAndError(t *testing.T) {
	var many []string
	for i := range 120 {
		many = append(many, fmt.Sprintf("s%03d.service", i))
	}
	source := newFakeServices(many...)
	published, _ := collect(t, source, time.Millisecond, time.Hour)
	waitFor(t, func() bool { return len(published()) == 1 })
	if list := published()[0]; *list.Count != 120 || len(list.Names) != 100 {
		t.Errorf("count %d with %d names, want 120 with 100", *list.Count, len(list.Names))
	}

	source.mu.Lock()
	source.err = errors.New("D-Bus is gone")
	source.mu.Unlock()
	source.signal()
	waitFor(t, func() bool { return len(published()) == 2 })
	if list := published()[1]; list.Count != nil || len(list.Names) != 0 {
		t.Errorf("after an error = %+v, want count null and no names", list)
	}
}
