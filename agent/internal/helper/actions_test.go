package helper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

var rebootEnabled = config.Config{EnabledActions: []protocol.Action{protocol.ActionReboot}}

type testRunner struct {
	*ActionRunner
	reboots int
	journal *bytes.Buffer
}

// newRunner makes an ActionRunner on a temporary log directory, with a Host
// that booted an hour ago and no package task running.
func newRunner(t *testing.T, dir string) *testRunner {
	t.Helper()
	locks := t.TempDir()
	r := &testRunner{journal: &bytes.Buffer{}}
	r.ActionRunner = &ActionRunner{
		Log:                 ActionLog{Dir: dir, Now: func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }},
		Journal:             slog.New(slog.NewTextHandler(r.journal, nil)),
		Uptime:              func() (time.Duration, error) { return time.Hour, nil },
		PackageTaskLock:     filepath.Join(locks, "package-task.lock"),
		PackageManagerLocks: []string{filepath.Join(locks, "lock-frontend"), filepath.Join(locks, ".rpm.lock")},
		Reboot: func(context.Context) error {
			r.reboots++
			return nil
		},
	}
	return r
}

func rebootRequest(id string) ActionRequest {
	user := "admin"
	return ActionRequest{ActionID: id, Action: protocol.ActionReboot, PairingID: "p1", PairingName: "Home", User: &user}
}

const (
	firstID  = "d4c3b2a1-9f8e-4d7c-b6a5-493827160a5b"
	secondID = "0a5b4c3d-2e1f-4a0b-9c8d-7e6f5a4b3c2d"
)

// logEntries reads every line of the Action log files in dir.
func logEntries(t *testing.T, dir string) []map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "actions-*.log"))
	var entries []map[string]any
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.Lines(string(data)) {
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			entries = append(entries, entry)
		}
	}
	return entries
}

func wantRefused(t *testing.T, ack Ack, run func(context.Context) protocol.ActionOutcome, err error, reason protocol.RefusalReason) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if ack.Status != "refused" || ack.Reason == nil || *ack.Reason != reason || run != nil {
		t.Fatalf("ack = %+v (run %v), want refused %s", ack, run != nil, reason)
	}
}

func TestAcceptedRebootRunsAndIsLoggedWithItsResult(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir)

	ack, run, err := r.Request(rebootEnabled, rebootRequest(firstID))
	if err != nil || ack.Status != "accepted" || ack.Reason != nil || run == nil {
		t.Fatalf("Request = %+v, %v, %v", ack, run != nil, err)
	}
	if r.reboots != 0 {
		t.Fatal("rebooted before run")
	}
	outcome := run(context.Background())
	if r.reboots != 1 || outcome.Result != "ok" || outcome.Error != nil {
		t.Fatalf("run = %+v after %d reboots", outcome, r.reboots)
	}

	entries := logEntries(t, dir)
	if len(entries) != 2 {
		t.Fatalf("log = %v, want the request and its result", entries)
	}
	request, result := entries[0], entries[1]
	for key, want := range map[string]any{
		"format": 1.0, "entry": "request", "time": "2026-09-29T12:00:00Z", "action_id": firstID, "action": "reboot",
		"pairing_id": "p1", "pairing": "Home", "user": "admin", "status": "accepted",
	} {
		if request[key] != want {
			t.Errorf("request %s = %v, want %v", key, request[key], want)
		}
	}
	if result["entry"] != "result" || result["action_id"] != firstID || result["result"] != "ok" {
		t.Errorf("result entry = %v", result)
	}
	if !strings.Contains(r.journal.String(), firstID) || !strings.Contains(r.journal.String(), "status=accepted") {
		t.Errorf("journal = %q, want the request", r.journal.String())
	}
}

func TestRequestWithoutUserIsLoggedAsNoHAUser(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir)
	request := rebootRequest(firstID)
	request.User = nil

	if _, _, err := r.Request(rebootEnabled, request); err != nil {
		t.Fatal(err)
	}
	if user := logEntries(t, dir)[0]["user"]; user != "no HA user" {
		t.Errorf("user = %v, want no HA user", user)
	}
}

func TestFailedRebootIsLoggedAsFailed(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir)
	r.Reboot = func(context.Context) error { return errors.New("systemctl reboot exited with status 1") }

	_, run, err := r.Request(rebootEnabled, rebootRequest(firstID))
	if err != nil || run == nil {
		t.Fatal(err)
	}
	outcome := run(context.Background())
	if outcome.Result != "failed" || outcome.Error == nil || !strings.Contains(*outcome.Error, "status 1") {
		t.Errorf("outcome = %+v", outcome)
	}
	if result := logEntries(t, dir)[1]; result["result"] != "failed" || !strings.Contains(result["error"].(string), "status 1") {
		t.Errorf("result entry = %v", result)
	}
}

func TestRebootIsRefusedWhenNotEnabled(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir)

	for _, cfg := range []config.Config{{}, {EnabledActions: []protocol.Action{protocol.ActionUpdateRun}}} {
		ack, run, err := r.Request(cfg, rebootRequest(identityFor(len(cfg.EnabledActions))))
		wantRefused(t, ack, run, err, protocol.ReasonDisabled)
	}
	if r.reboots != 0 {
		t.Error("rebooted")
	}
	if entry := logEntries(t, dir)[0]; entry["status"] != "refused" || entry["reason"] != "disabled" {
		t.Errorf("log entry = %v, want the refusal", entry)
	}
}

// identityFor returns a different Action ID for each n.
func identityFor(n int) string {
	return []string{firstID, secondID}[n]
}

func TestRebootIsRefusedWithinTenMinutesOfBoot(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir)
	r.Uptime = func() (time.Duration, error) { return 9*time.Minute + 59*time.Second, nil }

	ack, run, err := r.Request(rebootEnabled, rebootRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonTooSoonAfterBoot)

	r.Uptime = func() (time.Duration, error) { return 10 * time.Minute, nil }
	if ack, _, _ := r.Request(rebootEnabled, rebootRequest(secondID)); ack.Status != "accepted" {
		t.Errorf("after 10 minutes: %+v", ack)
	}
}

func TestRebootIsRefusedWhileThePackageTaskLockIsHeld(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir)
	holder, err := os.OpenFile(r.PackageTaskLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	ack, run, err := r.Request(rebootEnabled, rebootRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonBusy)

	syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	if ack, _, _ := r.Request(rebootEnabled, rebootRequest(secondID)); ack.Status != "accepted" {
		t.Errorf("after the lock was released: %+v", ack)
	}
}

func TestPackageTaskLockIsHeldUntilTheRebootStarted(t *testing.T) {
	r := newRunner(t, t.TempDir())
	_, run, err := r.Request(rebootEnabled, rebootRequest(firstID))
	if err != nil || run == nil {
		t.Fatal(err)
	}
	// Another package task cannot start in between.
	if ack, _, _ := r.Request(rebootEnabled, rebootRequest(secondID)); ack.Reason == nil || *ack.Reason != protocol.ReasonBusy {
		t.Errorf("second request while the first holds the lock: %+v", ack)
	}
	run(context.Background())
}

// The package managers lock with fcntl, which a process never sees for its own
// locks, so another process holds the lock: this test binary, run again.
func TestMain(m *testing.M) {
	if path := os.Getenv("HOSTBEACON_TEST_HOLD_LOCK"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			os.Exit(1)
		}
		lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0}
		if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock); err != nil {
			os.Exit(1)
		}
		os.Stdout.WriteString("locked\n")
		bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func holdPackageManagerLock(t *testing.T, path string) {
	t.Helper()
	holder := exec.Command(os.Args[0], "-test.run=^$")
	holder.Env = append(os.Environ(), "HOSTBEACON_TEST_HOLD_LOCK="+path)
	stdin, _ := holder.StdinPipe()
	stdout, _ := holder.StdoutPipe()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		holder.Wait()
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("the lock holder did not start: %q, %v", line, err)
	}
}

func TestRebootIsRefusedWhileThePackageManagerLockIsHeld(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir)
	holdPackageManagerLock(t, r.PackageManagerLocks[1])

	ack, run, err := r.Request(rebootEnabled, rebootRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonBusy)
	if r.reboots != 0 {
		t.Error("rebooted")
	}
}

func TestRepeatedActionIDIsRefusedAsDuplicateWithTheFirstResult(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir)
	_, run, _ := r.Request(rebootEnabled, rebootRequest(firstID))

	// Before the first result is known.
	ack, again, err := r.Request(rebootEnabled, rebootRequest(firstID))
	wantRefused(t, ack, again, err, protocol.ReasonDuplicate)
	if ack.FirstResult != nil {
		t.Errorf("first result = %+v before it is known", ack.FirstResult)
	}
	run(context.Background())

	ack, again, err = r.Request(rebootEnabled, rebootRequest(firstID))
	wantRefused(t, ack, again, err, protocol.ReasonDuplicate)
	if ack.FirstResult == nil || ack.FirstResult.Result != "ok" {
		t.Errorf("first result = %+v, want ok", ack.FirstResult)
	}
	if r.reboots != 1 {
		t.Errorf("rebooted %d times, want once", r.reboots)
	}
}

func TestRepeatedActionIDIsRefusedAfterARestart(t *testing.T) {
	dir := t.TempDir()
	first := newRunner(t, dir)
	_, run, _ := first.Request(rebootEnabled, rebootRequest(firstID))
	run(context.Background())

	restarted := newRunner(t, dir)
	ack, again, err := restarted.Request(rebootEnabled, rebootRequest(firstID))
	wantRefused(t, ack, again, err, protocol.ReasonDuplicate)
	if ack.FirstResult == nil || ack.FirstResult.Result != "ok" {
		t.Errorf("first result = %+v, want ok", ack.FirstResult)
	}
	if restarted.reboots != 0 {
		t.Error("rebooted again after a restart")
	}
}

func TestRepeatOfARefusedRequestGivesTheRefusalAsItsResult(t *testing.T) {
	r := newRunner(t, t.TempDir())
	r.Request(config.Config{}, rebootRequest(firstID))

	ack, run, err := r.Request(rebootEnabled, rebootRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonDuplicate)
	if ack.FirstResult == nil || ack.FirstResult.Result != "failed" || ack.FirstResult.Error == nil || *ack.FirstResult.Error != "refused: disabled" {
		t.Errorf("first result = %+v, want the refusal", ack.FirstResult)
	}
}

func TestRepeatFromTheMonthBeforeIsFound(t *testing.T) {
	dir := t.TempDir()
	first := newRunner(t, dir)
	first.Log.Now = func() time.Time { return time.Date(2026, 8, 31, 23, 59, 0, 0, time.UTC) }
	first.Request(config.Config{}, rebootRequest(firstID))

	next := newRunner(t, dir) // September
	ack, run, err := next.Request(rebootEnabled, rebootRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonDuplicate)
}

func TestRequestIsRefusedWhenTheLogCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	// A file where the log directory should be.
	blocked := filepath.Join(dir, "log")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRunner(t, blocked)

	ack, run, err := r.Request(rebootEnabled, rebootRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonCannotLog)
	if r.reboots != 0 {
		t.Error("rebooted without a log entry")
	}
	if !strings.Contains(r.journal.String(), "cannot_log") {
		t.Errorf("journal = %q, want the refusal", r.journal.String())
	}
	// The package-task lock is free again.
	r.Log.Dir = filepath.Join(dir, "ok")
	if ack, _, _ := r.Request(rebootEnabled, rebootRequest(secondID)); ack.Status != "accepted" {
		t.Errorf("after the log works again: %+v", ack)
	}
}

func TestLogFilesOlderThanAYearAreRemoved(t *testing.T) {
	dir := t.TempDir()
	for _, month := range []string{"2025-08", "2025-09", "2026-08"} {
		if err := os.WriteFile(filepath.Join(dir, "actions-"+month+".log"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := newRunner(t, dir) // now is 2026-09
	r.Request(rebootEnabled, rebootRequest(firstID))

	files, _ := filepath.Glob(filepath.Join(dir, "actions-*.log"))
	for i := range files {
		files[i] = filepath.Base(files[i])
	}
	want := []string{"actions-2025-09.log", "actions-2026-08.log", "actions-2026-09.log"}
	if strings.Join(files, " ") != strings.Join(want, " ") {
		t.Errorf("files = %v, want %v", files, want)
	}
}

func TestInvalidRequestIsAnError(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir)
	for _, request := range []ActionRequest{
		{ActionID: "not-a-uuid", Action: protocol.ActionReboot, PairingID: "p1", PairingName: "Home"},
		{ActionID: firstID, Action: "shutdown", PairingID: "p1", PairingName: "Home"},
		{ActionID: firstID, Action: protocol.ActionReboot, PairingID: "", PairingName: "Home"},
	} {
		if _, run, err := r.Request(rebootEnabled, request); err == nil || run != nil {
			t.Errorf("%+v: no error", request)
		}
	}
	if r.reboots != 0 || len(logEntries(t, dir)) != 0 {
		t.Error("an invalid request ran or was logged")
	}
}

func TestAnyUserNameIsLoggedAndCut(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir)
	for i, user := range []string{"", strings.Repeat("é", 300)} {
		request := rebootRequest(identityFor(i))
		request.User = &user
		if ack, _, err := r.Request(config.Config{}, request); err != nil || ack.Reason == nil || *ack.Reason != protocol.ReasonDisabled {
			t.Fatalf("user %q: %+v, %v", user, ack, err)
		}
	}
	entries := logEntries(t, dir)
	if entries[0]["user"] != "" || entries[1]["user"] != strings.Repeat("é", 256) {
		t.Errorf("users = %q, %q", entries[0]["user"], entries[1]["user"])
	}
}
