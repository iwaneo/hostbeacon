package helper

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// fakeJobs records which jobs ran.
type fakeJobs struct {
	mu      sync.Mutex
	ran     []string
	changes chan struct{}
	stopped chan struct{}
}

func newFakeJobs() *fakeJobs {
	return &fakeJobs{changes: make(chan struct{}), stopped: make(chan struct{})}
}

func (f *fakeJobs) record(job string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, job)
}

func (f *fakeJobs) jobs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran...)
}

func (f *fakeJobs) ReadSmart(context.Context) ([]SmartDisk, error) {
	f.record(JobReadSmart)
	health := "ok"
	return []SmartDisk{{Device: "sda", Health: &health}}, nil
}

func (f *fakeJobs) ReadContainers(context.Context) (Containers, error) {
	f.record(JobReadContainers)
	return Containers{Engines: []string{"docker"}, Items: []Container{{Name: "web", State: StateRunning}}}, nil
}

func (f *fakeJobs) WatchContainers(ctx context.Context, changed func()) error {
	f.record(JobWatchContainers)
	defer close(f.stopped)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.changes:
			changed()
		}
	}
}

func (f *fakeJobs) ReadSMBIOSUUID(context.Context) (*string, error) {
	f.record(JobReadSMBIOSUUID)
	uuid := "4c4c4544-0042-3510-8053-b4c04f4e3632"
	return &uuid, nil
}

type setup struct {
	uid        int
	loadConfig func() (config.Config, error)
	actions    *ActionRunner
}

// start runs a helper on a real socket and returns a client for it. By
// default the caller (this test) is the allowed user.
func start(t *testing.T, jobs Jobs, options ...func(*setup)) Client {
	t.Helper()
	s := setup{uid: os.Getuid(), loadConfig: func() (config.Config, error) { return config.Config{}, nil }}
	for _, option := range options {
		option(&s)
	}
	// A short path: socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "helper.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	server := &Server{AllowedUID: s.uid, LoadConfig: s.loadConfig, Jobs: jobs, Actions: s.actions}
	go func() {
		defer close(done)
		server.Serve(ctx, listener)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return Client{Socket: path}
}

func TestAllowedCallerGetsEachReadJob(t *testing.T) {
	jobs := newFakeJobs()
	client := start(t, jobs)
	ctx := context.Background()

	disks, err := client.ReadSmart(ctx)
	if err != nil || len(disks) != 1 || disks[0].Device != "sda" || *disks[0].Health != "ok" {
		t.Errorf("ReadSmart = %+v, %v", disks, err)
	}
	containers, err := client.ReadContainers(ctx)
	if err != nil || len(containers.Items) != 1 || containers.Items[0].State != StateRunning {
		t.Errorf("ReadContainers = %+v, %v", containers, err)
	}
	uuid, err := client.ReadSMBIOSUUID(ctx)
	if err != nil || uuid == nil || *uuid != "4c4c4544-0042-3510-8053-b4c04f4e3632" {
		t.Errorf("ReadSMBIOSUUID = %v, %v", uuid, err)
	}
}

func TestCallerThatIsNotTheAgentUserIsRefused(t *testing.T) {
	jobs := newFakeJobs()
	client := start(t, jobs, func(s *setup) { s.uid = os.Getuid() + 1 })

	_, err := client.ReadSmart(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error = %v, want a refusal", err)
	}
	if ran := jobs.jobs(); len(ran) != 0 {
		t.Errorf("jobs ran for a refused caller: %v", ran)
	}
}

func TestUnknownJobIsRefused(t *testing.T) {
	jobs := newFakeJobs()
	client := start(t, jobs)

	for _, job := range []string{"reboot_now", "", "read_smart\nread_containers", "../read_smart"} {
		err := client.call(context.Background(), request{Job: job}, new(any))
		if err == nil || !strings.Contains(err.Error(), "unknown job") {
			t.Errorf("job %q: error = %v, want unknown job", job, err)
		}
	}
	if ran := jobs.jobs(); len(ran) != 0 {
		t.Errorf("jobs ran: %v", ran)
	}
}

func TestUnreadableConfigRefusesEveryJob(t *testing.T) {
	jobs := newFakeJobs()
	client := start(t, jobs, func(s *setup) {
		s.loadConfig = func() (config.Config, error) { return config.Config{}, errors.New("permission denied") }
	})
	ctx := context.Background()

	if _, err := client.ReadSmart(ctx); err == nil || !strings.Contains(err.Error(), "Host config") {
		t.Errorf("ReadSmart error = %v, want a config refusal", err)
	}
	if _, err := client.ReadContainers(ctx); err == nil {
		t.Error("ReadContainers: no error")
	}
	if _, err := client.ReadSMBIOSUUID(ctx); err == nil {
		t.Error("ReadSMBIOSUUID: no error")
	}
	if err := client.WatchContainers(ctx, func() {}); err == nil {
		t.Error("WatchContainers: no error")
	}
	if ran := jobs.jobs(); len(ran) != 0 {
		t.Errorf("jobs ran without a config: %v", ran)
	}
}

func TestWatchSendsChangesUntilTheCallerLeaves(t *testing.T) {
	jobs := newFakeJobs()
	client := start(t, jobs)
	ctx, cancel := context.WithCancel(context.Background())
	changed := make(chan struct{})
	watched := make(chan error)
	go func() { watched <- client.WatchContainers(ctx, func() { changed <- struct{}{} }) }()

	for range 2 {
		select {
		case jobs.changes <- struct{}{}:
		case <-time.After(5 * time.Second):
			t.Fatal("the watch job did not start")
		}
		select {
		case <-changed:
		case <-time.After(5 * time.Second):
			t.Fatal("no change reached the caller")
		}
	}
	cancel()
	if err := <-watched; !errors.Is(err, context.Canceled) {
		t.Errorf("WatchContainers = %v, want canceled", err)
	}
	select {
	case <-jobs.stopped:
	case <-time.After(5 * time.Second):
		t.Error("the watch job kept running after the caller left")
	}
}

func TestActionJobAnswersTheAckThenTheResult(t *testing.T) {
	r := newRunner(t, t.TempDir())
	client := start(t, newFakeJobs(), func(s *setup) {
		s.actions = r.ActionRunner
		s.loadConfig = func() (config.Config, error) { return rebootEnabled, nil }
	})

	ack, result, err := client.Act(context.Background(), rebootRequest(firstID))
	if err != nil || ack.Status != "accepted" {
		t.Fatalf("Act = %+v, %v", ack, err)
	}
	select {
	case outcome, ok := <-result:
		if !ok || outcome.Result != "ok" {
			t.Errorf("result = %+v, %v", outcome, ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}
	if r.reboots != 1 {
		t.Errorf("reboots = %d", r.reboots)
	}

	ack, result, err = client.Act(context.Background(), rebootRequest(firstID))
	if err != nil || ack.Status != "refused" || *ack.Reason != protocol.ReasonDuplicate || ack.FirstResult.Result != "ok" || result != nil {
		t.Errorf("repeat: %+v, %v, %v", ack, result, err)
	}
}

func TestActionJobUsesTheConfigOfEachRequest(t *testing.T) {
	r := newRunner(t, t.TempDir())
	client := start(t, newFakeJobs(), func(s *setup) {
		s.actions = r.ActionRunner
		s.loadConfig = func() (config.Config, error) { return config.Config{}, nil }
	})

	ack, result, err := client.Act(context.Background(), rebootRequest(firstID))
	if err != nil || ack.Status != "refused" || *ack.Reason != protocol.ReasonDisabled || result != nil {
		t.Errorf("Act = %+v, %v, %v", ack, result, err)
	}
}

func TestActionJobIsRefusedWithoutAConfig(t *testing.T) {
	r := newRunner(t, t.TempDir())
	client := start(t, newFakeJobs(), func(s *setup) {
		s.actions = r.ActionRunner
		s.loadConfig = func() (config.Config, error) { return config.Config{}, errors.New("permission denied") }
	})

	if _, _, err := client.Act(context.Background(), rebootRequest(firstID)); err == nil {
		t.Error("no error")
	}
	if r.reboots != 0 {
		t.Error("rebooted")
	}
}

func TestIdentityCopyIsWrittenToTheActionLog(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir)
	client := start(t, newFakeJobs(), func(s *setup) { s.actions = r.ActionRunner })

	if err := client.LogIdentityCopy(context.Background(), IdentityCopy{InstanceID: secondID, CopiedFrom: []string{firstID}}); err != nil {
		t.Fatal(err)
	}
	entries := logEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	entry := entries[0]
	copiedFrom, _ := entry["copied_from"].([]any)
	if entry["entry"] != "identity_copy" || entry["instance_id"] != secondID || len(copiedFrom) != 1 || copiedFrom[0] != firstID {
		t.Errorf("entry = %v", entry)
	}
	if _, found := entry["action"]; found {
		t.Errorf("an identity copy entry has an action: %v", entry)
	}

	for _, bad := range []IdentityCopy{
		{InstanceID: "not-a-uuid", CopiedFrom: []string{firstID}},
		{InstanceID: secondID, CopiedFrom: []string{"x\ny"}},
	} {
		if err := client.LogIdentityCopy(context.Background(), bad); err == nil {
			t.Errorf("%+v: no error", bad)
		}
	}
	if len(logEntries(t, dir)) != 1 {
		t.Error("an invalid identity copy was logged")
	}
}
