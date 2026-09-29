package helper

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

var updateRunEnabled = config.Config{EnabledActions: []protocol.Action{protocol.ActionUpdateRun}}

// --- The helper's side: the run slot and starting the unit ---

type fakeUnit struct {
	active   bool
	startErr error
	started  []string
}

func (u *fakeUnit) Active(context.Context) (bool, error) { return u.active, nil }

func (u *fakeUnit) Start(_ context.Context, actionID string) error {
	u.started = append(u.started, actionID)
	if u.startErr != nil {
		return u.startErr
	}
	u.active = true
	return nil
}

func newUpdateRunner(t *testing.T, dir string) (*testRunner, *fakeUnit) {
	t.Helper()
	r := newRunner(t, dir)
	unit := &fakeUnit{}
	r.PackageManager, r.UpdateRuns = "apt", unit
	return r, unit
}

func updateRunRequest(id string) ActionRequest {
	request := rebootRequest(id)
	request.Action = protocol.ActionUpdateRun
	return request
}

func TestAcceptedUpdateRunStartsTheUnitAndIsLogged(t *testing.T) {
	dir := t.TempDir()
	r, unit := newUpdateRunner(t, dir)

	ack, run, err := r.Request(updateRunEnabled, updateRunRequest(firstID))
	if err != nil || ack.Status != "accepted" || run != nil {
		t.Fatalf("Request = %+v, %v, %v; want accepted, and no result from the helper", ack, run != nil, err)
	}
	if !slices.Equal(unit.started, []string{firstID}) {
		t.Errorf("started %q", unit.started)
	}
	if entry := logEntries(t, dir)[0]; entry["status"] != "accepted" || entry["action"] != "update_run" {
		t.Errorf("log entry = %v", entry)
	}
}

func TestSecondUpdateRunIsRefusedWhileOneRuns(t *testing.T) {
	r, unit := newUpdateRunner(t, t.TempDir())
	r.Request(updateRunEnabled, updateRunRequest(firstID))

	ack, run, err := r.Request(updateRunEnabled, updateRunRequest(secondID))
	wantRefused(t, ack, run, err, protocol.ReasonUpdateRunRunning)
	if len(unit.started) != 1 {
		t.Errorf("started %q", unit.started)
	}
}

func TestUpdateRunIsRefusedWhenOffOrNotSupported(t *testing.T) {
	r, unit := newUpdateRunner(t, t.TempDir())
	ack, run, err := r.Request(rebootEnabled, updateRunRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonDisabled)

	r.PackageManager = ""
	ack, run, err = r.Request(updateRunEnabled, updateRunRequest(secondID))
	wantRefused(t, ack, run, err, protocol.ReasonDisabled)
	if len(unit.started) != 0 {
		t.Errorf("started %q", unit.started)
	}
}

func holdPackageTaskLock(t *testing.T, path string) (release func()) {
	t.Helper()
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close() })
	return func() { holder.Close() }
}

func TestUpdateRunIsRefusedBusyWhileAnotherPackageTaskRuns(t *testing.T) {
	r, unit := newUpdateRunner(t, t.TempDir())
	holdPackageTaskLock(t, r.PackageTaskLock)

	ack, run, err := r.Request(updateRunEnabled, updateRunRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonBusy)
	if len(unit.started) != 0 {
		t.Errorf("started %q", unit.started)
	}
}

func TestUpdateRunIsRefusedBusyWhenTheUnitCannotTakeTheLock(t *testing.T) {
	r, unit := newUpdateRunner(t, t.TempDir())
	unit.startErr = errors.New("Job for hostbeacon-update-run.service failed")

	ack, run, err := r.Request(updateRunEnabled, updateRunRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonBusy)
}

func TestRebootIsRefusedWhileAnUpdateRunHoldsThePackageTaskLock(t *testing.T) {
	r, _ := newUpdateRunner(t, t.TempDir())
	holdPackageTaskLock(t, r.PackageTaskLock)

	ack, run, err := r.Request(config.Config{EnabledActions: []protocol.Action{protocol.ActionReboot}}, rebootRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonBusy)
}

func TestRepeatedUpdateRunIDIsRefusedAsDuplicate(t *testing.T) {
	r, unit := newUpdateRunner(t, t.TempDir())
	r.Request(updateRunEnabled, updateRunRequest(firstID))
	unit.active = false

	ack, run, err := r.Request(updateRunEnabled, updateRunRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonDuplicate)
	if len(unit.started) != 1 {
		t.Errorf("started %q", unit.started)
	}
}

func TestNextLogWriteCatchesUpAnUnloggedRunResult(t *testing.T) {
	dir := t.TempDir()
	r, _ := newUpdateRunner(t, dir)
	r.UpdateRunRecord = filepath.Join(t.TempDir(), "update-run.json")
	writeUpdateRun(r.UpdateRunRecord, &UpdateRunRecord{ActionID: secondID, RunID: secondID, State: RunFinished, StartedAt: "2026-09-29T11:00:00Z",
		FinishedAt: ptr("2026-09-29T11:30:00Z"), Result: ptr(ResultOK), NeedsManualUpdate: []string{}})

	r.Request(rebootEnabled, rebootRequest(firstID))
	entries := logEntries(t, dir)
	if last := entries[len(entries)-1]; last["entry"] != "result" || last["action_id"] != secondID || last["result"] != "ok" {
		t.Fatalf("log %v, want the run result after the request", entries)
	}
	if record, _ := ReadUpdateRun(r.UpdateRunRecord); !record.Logged {
		t.Error("the record is not marked logged")
	}
}

func TestCopyResetsTheUpdateRunRecordUnlessARunGoesOn(t *testing.T) {
	r, unit := newUpdateRunner(t, t.TempDir())
	r.UpdateRunRecord = filepath.Join(t.TempDir(), "update-run.json")
	writeUpdateRun(r.UpdateRunRecord, &UpdateRunRecord{ActionID: secondID, RunID: secondID, State: RunFinished, StartedAt: "2026-09-29T11:00:00Z", NeedsManualUpdate: []string{}})

	unit.active = true
	if err := r.ResetUpdateRun(); err != nil {
		t.Fatal(err)
	}
	if record, _ := ReadUpdateRun(r.UpdateRunRecord); record == nil {
		t.Fatal("removed the record of a run that goes on")
	}
	unit.active = false
	if err := r.ResetUpdateRun(); err != nil {
		t.Fatal(err)
	}
	if record, _ := ReadUpdateRun(r.UpdateRunRecord); record != nil {
		t.Fatalf("record %+v, want no run yet", record)
	}
}

// --- The unit's side: the steps of the run ---

type fakeHost struct {
	commands []string
	// outputs maps the start of a command line to its output.
	outputs map[string]string
	// fail maps the start of a command line to its error.
	fail map[string]error
	// answer, when set, answers first; ok false leaves it to the maps.
	answer      func(line string) (out string, err error, ok bool)
	streamLines []string
}

func (h *fakeHost) run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	h.commands = append(h.commands, line)
	if h.answer != nil {
		if out, err, ok := h.answer(line); ok {
			return []byte(out), err
		}
	}
	for prefix, err := range h.fail {
		if strings.HasPrefix(line, prefix) {
			return nil, err
		}
	}
	for prefix, out := range h.outputs {
		if strings.HasPrefix(line, prefix) {
			return []byte(out), nil
		}
	}
	return nil, nil
}

func (h *fakeHost) stream(ctx context.Context, line func(string), name string, args ...string) error {
	_, err := h.run(ctx, name, args...)
	for _, text := range h.streamLines {
		line(text)
	}
	return err
}

// ran says whether a command starting with prefix ran.
func (h *fakeHost) ran(prefix string) bool {
	return slices.ContainsFunc(h.commands, func(line string) bool { return strings.HasPrefix(line, prefix) })
}

type testUpdateRun struct {
	*UpdateRun
	host   *fakeHost
	ready  int
	logDir string
	// records holds every state the record was saved in.
	records []UpdateRunRecord
}

var runNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// newUpdateRun makes an Update run on temporary files for a Debian trixie
// Host, with an accepted request for firstID in the Action log.
func newUpdateRun(t *testing.T, manager string) *testUpdateRun {
	t.Helper()
	root, state, locks := t.TempDir(), t.TempDir(), t.TempDir()
	u := &testUpdateRun{host: &fakeHost{outputs: map[string]string{}, fail: map[string]error{}}, logDir: t.TempDir()}
	u.UpdateRun = &UpdateRun{
		Manager:                manager,
		Run:                    u.host.run,
		Stream:                 u.host.stream,
		Root:                   root,
		Record:                 filepath.Join(state, "update-run.json"),
		Request:                filepath.Join(locks, "update-run-request.json"),
		Stamp:                  filepath.Join(state, "package-list-refreshed"),
		Downloads:              filepath.Join(state, "downloads"),
		PackageTaskLock:        filepath.Join(locks, "package-task.lock"),
		PackageManagerLocks:    []string{filepath.Join(locks, "lock-frontend")},
		PackageManagerPIDLocks: []string{filepath.Join(locks, "metadata_lock.pid")},
		Log:                    ActionLog{Dir: u.logDir, Now: func() time.Time { return runNow }},
		Ready:                  func() error { u.ready++; return nil },
		Now:                    func() time.Time { return runNow },
		LockWait:               300 * time.Millisecond,
		LogWait:                300 * time.Millisecond,
		Poll:                   10 * time.Millisecond,
	}
	writeFile(t, filepath.Join(root, "etc", "os-release"), "ID=debian\nVERSION_CODENAME=trixie\n")
	lists := filepath.Join(root, "var", "lib", "apt", "lists")
	os.MkdirAll(lists, 0o755)
	writeRelease(t, lists, "deb_trixie_InRelease", "Debian", "trixie")
	writeRelease(t, lists, "deb_trixie-security_InRelease", "Debian", "trixie-security")
	u.request(t, firstID)
	if err := u.Log.write(logEntry{Entry: "request", ActionID: firstID, Action: protocol.ActionUpdateRun, Status: "accepted"}); err != nil {
		t.Fatal(err)
	}
	return u
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (u *testUpdateRun) request(t *testing.T, actionID string) {
	t.Helper()
	data, _ := json.Marshal(runRequest{ActionID: actionID})
	writeFile(t, u.Request, string(data))
}

func (u *testUpdateRun) record(t *testing.T) *UpdateRunRecord {
	t.Helper()
	record, err := ReadUpdateRun(u.Record)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// resultLines are the result lines of the Action log.
func (u *testUpdateRun) resultLines(t *testing.T) []map[string]any {
	t.Helper()
	return slices.DeleteFunc(logEntries(t, u.logDir), func(entry map[string]any) bool { return entry["entry"] != "result" })
}

const aptPlan = `Inst libfoo1 [1.0-1] (1.0-2 Debian:13.7/stable [amd64])
Inst foo-tool [2:3.1] (2:3.2 Debian-Security:13/stable-security [all])
Conf libfoo1 (1.0-2 Debian:13.7/stable [amd64])
`

func wantRun(t *testing.T, record *UpdateRunRecord, result string, installed, remaining *int64) {
	t.Helper()
	if record == nil || record.State != RunFinished || record.Result == nil || *record.Result != result {
		t.Fatalf("record %+v, want finished %s", record, result)
	}
	if !equalCount(record.Installed, installed) || !equalCount(record.Remaining, remaining) {
		t.Errorf("installed %v, remaining %v, want %v and %v", show(record.Installed), show(record.Remaining), show(installed), show(remaining))
	}
	if record.FinishedAt == nil || *record.FinishedAt != "2026-09-29T12:00:00Z" || record.StartedAt != "2026-09-29T12:00:00Z" {
		t.Errorf("times %q, %v", record.StartedAt, record.FinishedAt)
	}
	if !uuidPattern.MatchString(record.RunID) || record.ActionID != firstID || !record.Logged {
		t.Errorf("run ID %q, action ID %q, logged %v", record.RunID, record.ActionID, record.Logged)
	}
}

func equalCount(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func show(count *int64) any {
	if count == nil {
		return nil
	}
	return *count
}

func count(n int64) *int64 { return &n }

func TestAptUpdateRunInstallsExactlyTheApprovedPackages(t *testing.T) {
	u := newUpdateRun(t, "apt")
	u.host.outputs["apt-get -s"] = aptPlan
	u.host.outputs["apt list --upgradable"] = "Listing...\nheld/stable 2 amd64 [upgradable from: 1]\n"
	u.host.outputs["apt-mark showauto"] = "libfoo1\nother\n"
	u.host.streamLines = []string{"dlstatus:1:10:Retrieving", "pmstatus:libfoo1:12.5:Unpacking libfoo1", "pmstatus:foo-tool:87.2:Configuring foo-tool"}

	if err := u.Start(context.Background(), updateRunEnabled); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"apt-get update -q -o APT::Update::Error-Mode=any",
		"apt-get -s -q dist-upgrade",
		"apt-mark showauto",
		"apt-get install -q --trivial-only --no-remove -o APT::Status-Fd=1 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold libfoo1:amd64=1.0-2 foo-tool:all=2:3.2",
		// Naming a package makes it manually installed; libfoo1 was not.
		"apt-mark auto libfoo1:amd64",
		"apt list --upgradable -o APT::Cmd::Disable-Script-Warning=true",
	}
	if !slices.Equal(u.host.commands, want) {
		t.Errorf("commands\n%s\nwant\n%s", strings.Join(u.host.commands, "\n"), strings.Join(want, "\n"))
	}
	if u.ready != 1 {
		t.Errorf("ready %d times", u.ready)
	}
	wantRun(t, u.record(t), ResultOK, count(2), count(1))
	if results := u.resultLines(t); len(results) != 1 || results[0]["result"] != "ok" || results[0]["action_id"] != firstID {
		t.Errorf("Action log results %v", results)
	}
	if data, _ := os.ReadFile(u.Stamp); string(data) != `{"format":1,"refreshed_at":"2026-09-29T12:00:00Z"}`+"\n" {
		t.Errorf("stamp %q", data)
	}
	if _, err := os.Stat(u.Request); !errors.Is(err, os.ErrNotExist) {
		t.Error("the request was kept, so it could start a second run")
	}
}

func TestUpdateRunRecordsProgress(t *testing.T) {
	u := newUpdateRun(t, "apt")
	u.host.outputs["apt-get -s"] = aptPlan
	u.host.streamLines = []string{"pmstatus:libfoo1:12.5:Unpacking libfoo1"}
	var percent *float64
	u.host.answer = func(line string) (string, error, bool) {
		if strings.HasPrefix(line, "apt list") {
			percent = u.record(t).Percent
		}
		return "", nil, false
	}
	u.Start(context.Background(), updateRunEnabled)
	if percent == nil || *percent != 13 {
		t.Fatalf("percent during the run %v, want 13", percent)
	}
	if u.record(t).Percent != nil {
		t.Error("a finished run has a percent")
	}
}

func TestUpdateRunStopsWhenAptWouldRemoveAPackage(t *testing.T) {
	u := newUpdateRun(t, "apt")
	u.host.outputs["apt-get -s"] = aptPlan + "Remv oldlib1 [0.9]\nRemv oldlib2 [0.9]\n"

	u.Start(context.Background(), updateRunEnabled)
	if u.host.ran("apt-get install") {
		t.Fatal("installed")
	}
	record := u.record(t)
	wantRun(t, record, ResultNeedsManualUpdate, nil, nil)
	if !slices.Equal(record.NeedsManualUpdate, []string{"oldlib1", "oldlib2"}) || record.Error == nil {
		t.Errorf("needs manual update %q, error %v", record.NeedsManualUpdate, record.Error)
	}
	group := UpdateRunGroup(record)
	if *group.NeedsManualUpdate.Count != 2 || *group.Result != "needs_manual_update" {
		t.Errorf("group %+v", group)
	}
	if results := u.resultLines(t); results[0]["result"] != "failed" || !strings.HasPrefix(results[0]["error"].(string), "needs manual update: ") {
		t.Errorf("Action log results %v", results)
	}
}

func TestUpdateRunStopsWhenAPackageIndexIsForTheNextRelease(t *testing.T) {
	u := newUpdateRun(t, "apt")
	u.host.outputs["apt-get -s"] = aptPlan
	writeRelease(t, filepath.Join(u.Root, "var", "lib", "apt", "lists"), "deb_stable_InRelease", "Debian", "forky")

	u.Start(context.Background(), updateRunEnabled)
	if u.host.ran("apt-get install") {
		t.Fatal("installed")
	}
	record := u.record(t)
	wantRun(t, record, ResultNeedsManualUpdate, nil, nil)
	if !slices.Equal(record.NeedsManualUpdate, []string{"libfoo1", "foo-tool"}) {
		t.Errorf("needs manual update %q", record.NeedsManualUpdate)
	}
}

func TestNeedsManualUpdateStaysUntilARunPassesItsChecks(t *testing.T) {
	u := newUpdateRun(t, "apt")
	writeUpdateRun(u.Record, &UpdateRunRecord{ActionID: secondID, RunID: secondID, State: RunFinished, StartedAt: "2026-09-28T12:00:00Z",
		Result: ptr(ResultNeedsManualUpdate), NeedsManualUpdate: []string{"oldlib1"}, Logged: true})
	u.host.fail["apt-get update"] = errors.New("exit status 100")

	// Failed before its checks: the names stay.
	u.Start(context.Background(), updateRunEnabled)
	if record := u.record(t); *record.Result != ResultFailed || !slices.Equal(record.NeedsManualUpdate, []string{"oldlib1"}) {
		t.Fatalf("after a failed refresh: result %s, names %q", *record.Result, record.NeedsManualUpdate)
	}

	// Passed its checks: the names go, even though the install fails.
	delete(u.host.fail, "apt-get update")
	u.host.outputs["apt-get -s"] = aptPlan
	u.host.fail["apt-get install"] = errors.New("exit status 100")
	u.request(t, secondID)
	u.Log.write(logEntry{Entry: "request", ActionID: secondID, Action: protocol.ActionUpdateRun, Status: "accepted"})
	u.Start(context.Background(), updateRunEnabled)
	if record := u.record(t); *record.Result != ResultFailed || len(record.NeedsManualUpdate) != 0 {
		t.Fatalf("after passing the checks: result %s, names %q", *record.Result, record.NeedsManualUpdate)
	}
}

func TestUpdateRunStopsWhenTheRefreshFails(t *testing.T) {
	u := newUpdateRun(t, "apt")
	u.host.fail["apt-get update"] = errors.New("exit status 100")

	u.Start(context.Background(), updateRunEnabled)
	if u.host.ran("apt-get -s") || u.host.ran("apt-get install") {
		t.Fatalf("went on after the failed refresh: %q", u.host.commands)
	}
	record := u.record(t)
	wantRun(t, record, ResultFailed, nil, nil)
	if record.Error == nil || !strings.Contains(*record.Error, "Cannot refresh the package list") {
		t.Errorf("error %v", record.Error)
	}
	if _, err := os.Stat(u.Stamp); err == nil {
		t.Error("stored a refresh time for a failed refresh")
	}
}

func TestFailedInstallIsAFailedRun(t *testing.T) {
	u := newUpdateRun(t, "apt")
	u.host.outputs["apt-get -s"] = aptPlan
	u.host.fail["apt-get install"] = errors.New("E: Version '1.0-2' for 'libfoo1' was not found")

	u.Start(context.Background(), updateRunEnabled)
	record := u.record(t)
	wantRun(t, record, ResultFailed, nil, count(0))
	if record.Error == nil || !strings.Contains(*record.Error, "was not found") {
		t.Errorf("error %v", record.Error)
	}
	if results := u.resultLines(t); results[0]["result"] != "failed" {
		t.Errorf("Action log results %v", results)
	}
}

func TestUpdateRunWithNothingToInstallIsOK(t *testing.T) {
	u := newUpdateRun(t, "apt")
	u.Start(context.Background(), updateRunEnabled)
	if u.host.ran("apt-get install") {
		t.Error("ran the install with nothing to install")
	}
	wantRun(t, u.record(t), ResultOK, count(0), count(0))
}

func TestUpdateRunIsBusyWhileAnotherPackageTaskHoldsTheLock(t *testing.T) {
	u := newUpdateRun(t, "apt")
	holdPackageTaskLock(t, u.PackageTaskLock)

	if err := u.Start(context.Background(), updateRunEnabled); !errors.Is(err, ErrBusy) {
		t.Fatalf("error %v, want ErrBusy", err)
	}
	if u.ready != 0 || len(u.host.commands) != 0 || u.record(t) != nil {
		t.Errorf("ready %d, commands %q, record %+v", u.ready, u.host.commands, u.record(t))
	}
}

func TestUpdateRunHoldsThePackageTaskLockUntilItEnds(t *testing.T) {
	u := newUpdateRun(t, "apt")
	u.host.outputs["apt-get -s"] = aptPlan
	var free bool
	u.host.answer = func(line string) (string, error, bool) {
		if strings.HasPrefix(line, "apt-get install") {
			release, err := lockPackageTask(u.PackageTaskLock)
			if free = err == nil; free {
				release()
			}
		}
		return "", nil, false
	}
	u.Start(context.Background(), updateRunEnabled)
	if free {
		t.Fatal("the package-task lock was free during the install")
	}
	release, err := lockPackageTask(u.PackageTaskLock)
	if err != nil {
		t.Fatalf("the lock is still held after the run: %v", err)
	}
	release()
}

func TestUpdateRunWaitsForThePackageManagerLock(t *testing.T) {
	u := newUpdateRun(t, "apt")
	u.LockWait = 5 * time.Second
	holder := exec.Command(os.Args[0], "-test.run=^$")
	holder.Env = append(os.Environ(), "HOSTBEACON_TEST_HOLD_LOCK="+u.PackageManagerLocks[0])
	stdin, _ := holder.StdinPipe()
	stdout, _ := holder.StdoutPipe()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer holder.Wait()
	buffer := make([]byte, len("locked\n"))
	if _, err := stdout.Read(buffer); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan string, 1)
	go func() {
		// Free the lock once the run is waiting for it.
		for {
			if record, _ := ReadUpdateRun(u.Record); record != nil {
				waiting <- record.State
				stdin.Close()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	u.Start(context.Background(), updateRunEnabled)
	if state := <-waiting; state != RunWaitingForLock {
		t.Errorf("first record state %q, want waiting_for_lock", state)
	}
	wantRun(t, u.record(t), ResultOK, count(0), count(0))
}

func TestUpdateRunGivesUpAfterWaitingForThePackageManager(t *testing.T) {
	u := newUpdateRun(t, "apt")
	holdPackageManagerLock(t, u.PackageManagerLocks[0])

	u.Start(context.Background(), updateRunEnabled)
	if len(u.host.commands) != 0 {
		t.Fatalf("ran %q while the package manager was busy", u.host.commands)
	}
	record := u.record(t)
	wantRun(t, record, ResultFailed, nil, nil)
	if record.Error == nil || !strings.Contains(*record.Error, "busy") {
		t.Errorf("error %v", record.Error)
	}
}

func TestUpdateRunDoesNothingUntilTheRequestIsAcceptedInTheLog(t *testing.T) {
	u := newUpdateRun(t, "apt")
	u.request(t, secondID)

	if err := u.Start(context.Background(), updateRunEnabled); err == nil {
		t.Fatal("ran a request that is not in the Action log")
	}
	if len(u.host.commands) != 0 || u.record(t) != nil {
		t.Errorf("commands %q, record %+v", u.host.commands, u.record(t))
	}
}

func TestUpdateRunUnitRechecksTheHostConfig(t *testing.T) {
	u := newUpdateRun(t, "apt")
	if err := u.Start(context.Background(), rebootEnabled); err == nil {
		t.Fatal("ran with Update run turned off")
	}
	u.request(t, firstID)
	u.Manager = ""
	if err := u.Start(context.Background(), updateRunEnabled); err == nil {
		t.Fatal("ran on a distro without full support")
	}
	if u.ready != 0 || len(u.host.commands) != 0 {
		t.Errorf("ready %d, commands %q", u.ready, u.host.commands)
	}
}

func TestUpdateRunWithoutARequestDoesNothing(t *testing.T) {
	u := newUpdateRun(t, "apt")
	os.Remove(u.Request)
	if err := u.Start(context.Background(), updateRunEnabled); err == nil || u.ready != 0 {
		t.Fatalf("error %v, ready %d", err, u.ready)
	}
}

// dnf

// newDnfRun is a Fedora 44 Host with two packages to update. The fake dnf
// downloads them.
func newDnfRun(t *testing.T) *testUpdateRun {
	t.Helper()
	u := newUpdateRun(t, "dnf")
	os.Remove(filepath.Join(u.Root, "etc", "os-release"))
	writeFile(t, filepath.Join(u.Root, "usr", "lib", "os-release"), "ID=fedora\nVERSION_ID=44\n")
	if err := os.Symlink("../usr/lib/os-release", filepath.Join(u.Root, "etc", "os-release")); err != nil {
		t.Fatal(err)
	}
	headers := "python3-urllib3 noarch 0:2.8.0-1.fc44\nkernel-core x86_64 0:6.17.1-300.fc44\n"
	u.host.answer = func(line string) (string, error, bool) {
		switch {
		case strings.HasPrefix(line, "dnf upgrade"):
			for _, name := range []string{"kernel-core.rpm", "python3-urllib3.rpm"} {
				writeFile(t, filepath.Join(u.Downloads, name), "rpm")
			}
		case strings.HasPrefix(line, "rpm -qp --queryformat %{NAME}:"):
			return "kernel-core:\npython3-urllib3:\n", nil, true
		case strings.HasPrefix(line, "rpm -qp"):
			return headers, nil, true
		case strings.HasPrefix(line, "rpm -q --queryformat"):
			// kernel-core has another version installed; python3-urllib3 is new.
			return "kernel-core\npackage python3-urllib3 is not installed\n", exec.Command("false").Run(), true
		case line == "dnf --version":
			return "dnf5 version 5.4.6.0\n", nil, true
		case strings.HasPrefix(line, "rpm -qf"):
			return "fedora-release-identity-cloud 44\n", nil, true
		case strings.HasPrefix(line, "rpm -K"):
			return u.Downloads + "/kernel-core.rpm: digests signatures OK\n" + u.Downloads + "/python3-urllib3.rpm: digests signatures OK\n", nil, true
		case strings.HasPrefix(line, "dnf --cacheonly"):
			return "held noarch\n", nil, true
		}
		return "", nil, false
	}
	return u
}

func TestDnfUpdateRunInstallsOnlyTheDownloadedPackages(t *testing.T) {
	u := newDnfRun(t)
	if err := u.Start(context.Background(), updateRunEnabled); err != nil {
		t.Fatal(err)
	}
	files := u.Downloads + "/kernel-core.rpm " + u.Downloads + "/python3-urllib3.rpm"
	want := []string{
		"dnf makecache --refresh -q",
		"dnf upgrade -q -y --downloadonly --destdir=" + u.Downloads,
		"rpm -qp --queryformat " + rpmHeaderFormat + " " + files,
		"rpm -qf --queryformat %{NAME} %{VERSION}\n " + filepath.Join(u.Root, "usr", "lib", "os-release"),
		"rpm -qp --queryformat " + rpmHeaderFormat + " " + files,
		"rpm -K " + files,
		"rpm -q --queryformat %{NAME}\n python3-urllib3 kernel-core",
		"rpm -qp --queryformat %{NAME}:[%{OBSOLETENAME} ]\n " + files,
		"dnf install -y -q --disablerepo=* " + files,
		"dnf --version",
		// Named on the command line, dnf would keep the new package forever.
		"dnf mark dependency python3-urllib3",
		"dnf --cacheonly -q repoquery --upgrades --latest-limit=1 --queryformat %{name} %{arch}\n",
	}
	// The root in the test may be under a symlink (macOS /var).
	for i := range u.host.commands {
		u.host.commands[i] = strings.ReplaceAll(u.host.commands[i], mustEval(t, u.Root), u.Root)
	}
	if !slices.Equal(u.host.commands, want) {
		t.Errorf("commands\n%s\nwant\n%s", strings.Join(u.host.commands, "\n"), strings.Join(want, "\n"))
	}
	wantRun(t, u.record(t), ResultOK, count(2), count(1))
	if _, err := os.Stat(u.Downloads); !errors.Is(err, os.ErrNotExist) {
		t.Error("the downloaded packages were kept")
	}
}

func mustEval(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestDnfUpdateRunStopsWhenTheReleaseWouldChange(t *testing.T) {
	for name, owner := range map[string]struct {
		out string
		err error
	}{
		"owner moves to 45": {out: "fedora-release-identity-cloud 44\n"},
		"no owner found":    {out: "file /usr/lib/os-release is not owned by any package\n", err: errors.New("exit status 1")},
	} {
		t.Run(name, func(t *testing.T) {
			u := newDnfRun(t)
			answer := u.host.answer
			u.host.answer = func(line string) (string, error, bool) {
				switch {
				case strings.HasPrefix(line, "rpm -qf"):
					return owner.out, owner.err, true
				case strings.HasPrefix(line, "rpm -qp"):
					return "fedora-release-identity-cloud noarch 0:45-1\n", nil, true
				}
				return answer(line)
			}
			u.Start(context.Background(), updateRunEnabled)
			if u.host.ran("dnf install") {
				t.Fatal("installed")
			}
			wantRun(t, u.record(t), ResultNeedsManualUpdate, nil, nil)
		})
	}
}

func TestDnfUpdateRunStopsWhenThePackagesChangedAfterTheCheck(t *testing.T) {
	u := newDnfRun(t)
	answer, checks := u.host.answer, 0
	u.host.answer = func(line string) (string, error, bool) {
		if strings.HasPrefix(line, "rpm -qp") {
			if checks++; checks == 2 {
				return "python3-urllib3 noarch 0:2.9.0-1.fc44\nkernel-core x86_64 0:6.17.1-300.fc44\n", nil, true
			}
		}
		return answer(line)
	}
	u.Start(context.Background(), updateRunEnabled)
	if u.host.ran("dnf install") {
		t.Fatal("installed a changed plan")
	}
	record := u.record(t)
	wantRun(t, record, ResultFailed, nil, count(1))
	if record.Error == nil || !strings.Contains(*record.Error, "changed") {
		t.Errorf("error %v", record.Error)
	}
}

func TestDnfUpdateRunRefusesUnsignedPackages(t *testing.T) {
	u := newDnfRun(t)
	answer := u.host.answer
	u.host.answer = func(line string) (string, error, bool) {
		if strings.HasPrefix(line, "rpm -K") {
			return u.Downloads + "/kernel-core.rpm: digests signatures OK\n" + u.Downloads + "/python3-urllib3.rpm: digests OK\n", nil, true
		}
		return answer(line)
	}
	u.Start(context.Background(), updateRunEnabled)
	if u.host.ran("dnf install") {
		t.Fatal("installed an unsigned package")
	}
	wantRun(t, u.record(t), ResultFailed, nil, count(1))
}

// --- The end of the unit ---

func TestEndUpdateRunMarksARunThatStoppedWithoutAResultUnknown(t *testing.T) {
	u := newUpdateRun(t, "apt")
	record := &UpdateRunRecord{ActionID: firstID, RunID: secondID, State: RunRunning, StartedAt: "2026-09-29T11:00:00Z", Percent: ptr(40.0), NeedsManualUpdate: []string{}}
	if err := writeUpdateRun(u.Record, record); err != nil {
		t.Fatal(err)
	}

	if err := EndUpdateRun(u.Record, u.Log); err != nil {
		t.Fatal(err)
	}
	record = u.record(t)
	if record.State != RunResultUnknown || record.Percent != nil || !record.Logged || record.Result != nil {
		t.Errorf("record %+v", record)
	}
	if group := UpdateRunGroup(record); group.State != "result_unknown" || *group.RunID != secondID {
		t.Errorf("group %+v", group)
	}
	if results := u.resultLines(t); len(results) != 1 || results[0]["result"] != "failed" || !strings.HasPrefix(results[0]["error"].(string), "result unknown") {
		t.Errorf("Action log results %v", results)
	}
	// A second end changes nothing.
	EndUpdateRun(u.Record, u.Log)
	if len(u.resultLines(t)) != 1 {
		t.Error("logged the result twice")
	}
}

func TestEndUpdateRunLogsAResultTheUnitCouldNotLog(t *testing.T) {
	u := newUpdateRun(t, "apt")
	record := &UpdateRunRecord{ActionID: firstID, RunID: secondID, State: RunFinished, StartedAt: "2026-09-29T11:00:00Z", FinishedAt: ptr("2026-09-29T11:30:00Z"), Result: ptr(ResultOK), NeedsManualUpdate: []string{}}
	writeUpdateRun(u.Record, record)

	if err := EndUpdateRun(u.Record, u.Log); err != nil {
		t.Fatal(err)
	}
	if !u.record(t).Logged || u.resultLines(t)[0]["result"] != "ok" {
		t.Errorf("record %+v, results %v", u.record(t), u.resultLines(t))
	}
}

func TestNoRecordIsAnIdleGroup(t *testing.T) {
	record, err := ReadUpdateRun(filepath.Join(t.TempDir(), "update-run.json"))
	if err != nil || record != nil {
		t.Fatal(record, err)
	}
	group := UpdateRunGroup(nil)
	if group.State != "idle" || group.RunID != nil || group.NeedsManualUpdate.Names == nil {
		t.Errorf("group %+v", group)
	}
	if _, err := protocol.Encode(&protocol.Snapshot{ID: "s", Groups: protocol.Groups{
		Agent: &protocol.AgentInfo{Capabilities: []string{}, EnabledActions: []protocol.Action{}}, System: &protocol.System{},
		UpdateRun: &group, Flags: &protocol.Flags{RebootRequired: "unknown"},
	}}); err != nil {
		t.Errorf("the idle group does not follow the schema: %v", err)
	}
}

func TestAptProgress(t *testing.T) {
	for line, want := range map[string]float64{"pmstatus:libc6:43.4783:Installing libc6": 43, "pmstatus:x:99.6:Done": 100} {
		if got, ok := aptProgress(line); !ok || got != want {
			t.Errorf("%q: %v %v", line, got, ok)
		}
	}
	for _, line := range []string{"dlstatus:1:10:Retrieving", "Setting up libc6", "pmstatus:x:abc:y"} {
		if _, ok := aptProgress(line); ok {
			t.Errorf("%q gave a percent", line)
		}
	}
}
