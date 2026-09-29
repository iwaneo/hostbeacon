package helper

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
	"github.com/iwaneo/hostbeacon/agent/internal/release"
	"github.com/iwaneo/hostbeacon/agent/internal/release/releasetest"
)

var agentUpdateEnabled = config.Config{EnabledActions: []protocol.Action{protocol.ActionAgentUpdate}}

// --- The helper's side: starting the unit ---

func newAgentUpdateRunner(t *testing.T, dir string) (*testRunner, *fakeUnit) {
	t.Helper()
	r := newRunner(t, dir)
	unit := &fakeUnit{}
	r.AgentUpdates = unit
	return r, unit
}

func agentUpdateRequest(id string) ActionRequest {
	request := rebootRequest(id)
	request.Action = protocol.ActionAgentUpdate
	return request
}

func TestAcceptedAgentUpdateStartsTheUnitAndIsLogged(t *testing.T) {
	dir := t.TempDir()
	r, unit := newAgentUpdateRunner(t, dir)

	ack, run, err := r.Request(agentUpdateEnabled, agentUpdateRequest(firstID))
	if err != nil || ack.Status != "accepted" || run != nil {
		t.Fatalf("Request = %+v, %v, %v; want accepted, and no result from the helper", ack, run != nil, err)
	}
	if !slices.Equal(unit.started, []string{firstID}) {
		t.Errorf("started %q", unit.started)
	}
	if entry := logEntries(t, dir)[0]; entry["status"] != "accepted" || entry["action"] != "agent_update" {
		t.Errorf("log entry = %v", entry)
	}
}

func TestAgentUpdateIsRefusedWhenOff(t *testing.T) {
	r, unit := newAgentUpdateRunner(t, t.TempDir())
	ack, run, err := r.Request(updateRunEnabled, agentUpdateRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonDisabled)
	if len(unit.started) != 0 {
		t.Errorf("started %q", unit.started)
	}
}

func TestAgentUpdateIsRefusedBusyWhileAPackageTaskRuns(t *testing.T) {
	r, unit := newAgentUpdateRunner(t, t.TempDir())
	release := holdPackageTaskLock(t, r.PackageTaskLock)
	ack, run, err := r.Request(agentUpdateEnabled, agentUpdateRequest(firstID))
	wantRefused(t, ack, run, err, protocol.ReasonBusy)
	release()

	unit.active = true
	ack, run, err = r.Request(agentUpdateEnabled, agentUpdateRequest(secondID))
	wantRefused(t, ack, run, err, protocol.ReasonBusy)
	if len(unit.started) != 0 {
		t.Errorf("started %q", unit.started)
	}
}

// --- The unit's side: the steps of the Agent update ---

// fakeReleases serves signed releases by URL, like GitHub Releases.
type fakeReleases struct {
	files map[string][]byte
	// downloaded lists every URL asked for.
	downloaded []string
}

func (f *fakeReleases) download(_ context.Context, url string) ([]byte, error) {
	f.downloaded = append(f.downloaded, url)
	data, found := f.files[url]
	if !found {
		return nil, errors.New("404 Not Found")
	}
	return data, nil
}

// publish adds a release signed with key, with a package of each kind for
// amd64, whose content is its file name.
func (f *fakeReleases) publish(key releasetest.Key, version string) map[string][]byte {
	files := map[string][]byte{}
	for _, kind := range []release.Kind{release.Deb, release.RPM} {
		name := release.FileName(kind, version, "amd64")
		files[name] = []byte(name)
	}
	name := release.FileName(release.Tarball, version, "amd64")
	files[name] = tarball(version)
	sums := releasetest.Sums(files)
	files[release.SumsFile] = sums
	files[release.SignatureFile] = key.Sign(sums, "hostbeacon "+version)
	for name, data := range files {
		f.files[release.VersionURL(version, name)] = data
	}
	return files
}

// latest makes version the newest release.
func (f *fakeReleases) latest(version string) {
	for _, name := range []string{release.SumsFile, release.SignatureFile} {
		f.files[release.LatestURL(name)] = f.files[release.VersionURL(version, name)]
	}
}

// tarball makes a release tarball with fake programs.
func tarball(version string) []byte {
	var out bytes.Buffer
	zipped := gzip.NewWriter(&out)
	archive := tar.NewWriter(zipped)
	dir := "hostbeacon_" + version + "_linux_amd64/"
	archive.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir, Mode: 0o755})
	for _, name := range []string{"hostbeacon", "hostbeacon-helper", "LICENSE"} {
		content := []byte(name + " " + version)
		archive.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: dir + name, Mode: 0o755, Size: int64(len(content))})
		archive.Write(content)
	}
	archive.Close()
	zipped.Close()
	return out.Bytes()
}

type testAgentUpdate struct {
	*AgentUpdate
	host     *fakeHost
	releases *fakeReleases
	key      releasetest.Key
	ready    int
	logDir   string
	// unhealthy lists the versions whose health check fails.
	unhealthy []string
	checked   []string
}

// newAgentUpdate makes an Agent update of a .deb install of 1.0.0 on amd64,
// with 1.1.0 as the newest release, and an accepted request for firstID.
func newAgentUpdate(t *testing.T, kind release.Kind) *testAgentUpdate {
	t.Helper()
	state, locks := t.TempDir(), t.TempDir()
	u := &testAgentUpdate{
		host:     &fakeHost{outputs: map[string]string{}, fail: map[string]error{}},
		releases: &fakeReleases{files: map[string][]byte{}},
		key:      releasetest.NewKey(),
		logDir:   t.TempDir(),
	}
	keys, err := release.ParseKeys(u.key.Public)
	if err != nil {
		t.Fatal(err)
	}
	u.AgentUpdate = &AgentUpdate{
		Version:         "1.0.0",
		Kind:            kind,
		Arch:            "amd64",
		Keys:            keys,
		Download:        u.releases.download,
		Run:             u.host.run,
		TarballDir:      filepath.Join(t.TempDir(), "lib"),
		Record:          filepath.Join(state, "agent-update.json"),
		Request:         filepath.Join(locks, "agent-update-request.json"),
		Staging:         filepath.Join(state, "agent-update"),
		PackageTaskLock: filepath.Join(locks, "package-task.lock"),
		Log:             ActionLog{Dir: u.logDir, Now: func() time.Time { return runNow }},
		Ready:           func() error { u.ready++; return nil },
		Healthy: func(_ context.Context, version string) error {
			u.checked = append(u.checked, version)
			if slices.Contains(u.unhealthy, version) {
				return errors.New("hostbeacon.service is not active")
			}
			return nil
		},
		Now:     func() time.Time { return runNow },
		LogWait: 300 * time.Millisecond,
		Poll:    10 * time.Millisecond,
	}
	u.releases.publish(u.key, "1.0.0")
	u.releases.publish(u.key, "1.1.0")
	u.releases.latest("1.1.0")
	// The tarball install of the running version.
	writeFile(t, filepath.Join(u.TarballDir, "1.0.0", "hostbeacon"), "hostbeacon 1.0.0")
	u.request(t, firstID)
	if err := u.Log.write(logEntry{Entry: "request", ActionID: firstID, Action: protocol.ActionAgentUpdate, Status: "accepted"}); err != nil {
		t.Fatal(err)
	}
	return u
}

func (u *testAgentUpdate) request(t *testing.T, actionID string) {
	t.Helper()
	data, _ := json.Marshal(runRequest{ActionID: actionID})
	writeFile(t, u.Request, string(data))
}

func (u *testAgentUpdate) start(t *testing.T) *AgentUpdateRecord {
	t.Helper()
	if err := u.Start(context.Background(), agentUpdateEnabled); err != nil {
		t.Fatalf("Start: %v", err)
	}
	record, err := ReadAgentUpdate(u.Record)
	if err != nil || record == nil {
		t.Fatalf("record %v, %v", record, err)
	}
	return record
}

// staged lists the files in the staging directory.
func (u *testAgentUpdate) staged() []string {
	entries, _ := os.ReadDir(u.Staging)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func (u *testAgentUpdate) installedAnything() bool {
	return u.host.ran("apt-get") || u.host.ran("dnf") || slices.ContainsFunc(u.host.commands, func(line string) bool {
		return strings.HasSuffix(line, "hostbeacon install")
	})
}

func wantAgentUpdate(t *testing.T, record *AgentUpdateRecord, result, errorPart string) {
	t.Helper()
	if record.State != AgentUpdateFinished || record.Result == nil || *record.Result != result {
		t.Fatalf("record %+v, want finished %s", record, result)
	}
	if got := deref(record.Error); !strings.Contains(got, errorPart) || (errorPart == "") != (got == "") {
		t.Errorf("error %q, want it to say %q", got, errorPart)
	}
	if record.ActionID != firstID || record.FromVersion != "1.0.0" || !record.Logged || record.FinishedAt == nil {
		t.Errorf("record %+v", record)
	}
}

func TestAgentUpdateInstallsTheNewestReleaseAndKeepsIt(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	record := u.start(t)

	wantAgentUpdate(t, record, ResultOK, "")
	if deref(record.ToVersion) != "1.1.0" || u.ready != 1 {
		t.Errorf("to %v, ready %d", deref(record.ToVersion), u.ready)
	}
	newDeb := filepath.Join(u.Staging, "hostbeacon_1.1.0_amd64.deb")
	want := []string{
		"apt-get install -y -q -o DPkg::Lock::Timeout=300 " + newDeb,
		"systemctl restart hostbeacon-helper.service hostbeacon.service",
	}
	if !slices.Equal(u.host.commands, want) {
		t.Errorf("commands\n%q\nwant\n%q", u.host.commands, want)
	}
	if !slices.Equal(u.checked, []string{"1.1.0"}) {
		t.Errorf("health checks %q", u.checked)
	}
	// The new package stays for the next update's rollback.
	if staged := u.staged(); !slices.Equal(staged, []string{"hostbeacon_1.1.0_amd64.deb"}) {
		t.Errorf("staging holds %q", staged)
	}
	if lines := resultLines(t, u.logDir); len(lines) != 1 || lines[0]["result"] != "ok" || lines[0]["action_id"] != firstID {
		t.Errorf("Action log results %v", lines)
	}
}

func TestAgentUpdateRefusesABadSignature(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	other := releasetest.NewKey()
	sums := u.releases.files[release.LatestURL(release.SumsFile)]
	u.releases.files[release.LatestURL(release.SignatureFile)] = other.Sign(sums, "hostbeacon 1.1.0")

	wantAgentUpdate(t, u.start(t), ResultFailed, "not signed with a Hostbeacon release key")
	if u.installedAnything() {
		t.Errorf("installed: %q", u.host.commands)
	}
}

func TestAgentUpdateRefusesADowngrade(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	u.releases.publish(u.key, "0.9.0")
	u.releases.latest("0.9.0")

	wantAgentUpdate(t, u.start(t), ResultFailed, "older than the installed 1.0.0")
	if u.installedAnything() {
		t.Errorf("installed: %q", u.host.commands)
	}
}

func TestAgentUpdateWithTheNewestInstalledChangesNothing(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	u.releases.latest("1.0.0")

	record := u.start(t)
	wantAgentUpdate(t, record, ResultOK, "")
	if u.installedAnything() || deref(record.ToVersion) != "1.0.0" {
		t.Errorf("installed: %q, to %v", u.host.commands, deref(record.ToVersion))
	}
}

func TestAgentUpdateRefusesABadHash(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	u.releases.files[release.VersionURL("1.1.0", "hostbeacon_1.1.0_amd64.deb")] = []byte("changed on the way")

	wantAgentUpdate(t, u.start(t), ResultFailed, "hash differs")
	if u.installedAnything() {
		t.Errorf("installed: %q", u.host.commands)
	}
	if slices.Contains(u.staged(), "hostbeacon_1.1.0_amd64.deb") {
		t.Error("kept the bad file")
	}
}

func TestAgentUpdateRollsBackWhenTheHealthCheckFails(t *testing.T) {
	for _, test := range []struct {
		kind            release.Kind
		install, revert string
	}{
		{release.Deb, "apt-get install -y -q -o DPkg::Lock::Timeout=300 %s/hostbeacon_1.1.0_amd64.deb",
			"apt-get install -y -q -o DPkg::Lock::Timeout=300 --allow-downgrades %s/hostbeacon_1.0.0_amd64.deb"},
		{release.RPM, "dnf install -y -q --disablerepo=* %s/hostbeacon-1.1.0-1.x86_64.rpm",
			"dnf downgrade -y -q --disablerepo=* %s/hostbeacon-1.0.0-1.x86_64.rpm"},
	} {
		t.Run(string(test.kind), func(t *testing.T) {
			u := newAgentUpdate(t, test.kind)
			u.unhealthy = []string{"1.1.0"}

			record := u.start(t)
			wantAgentUpdate(t, record, ResultFailed, "went back to 1.0.0")
			restart := "systemctl restart hostbeacon-helper.service hostbeacon.service"
			want := []string{fmt.Sprintf(test.install, u.Staging), restart, fmt.Sprintf(test.revert, u.Staging), restart}
			if !slices.Equal(u.host.commands, want) {
				t.Errorf("commands\n%q\nwant\n%q", u.host.commands, want)
			}
			if !slices.Equal(u.checked, []string{"1.1.0", "1.0.0"}) {
				t.Errorf("health checks %q", u.checked)
			}
			if deref(record.ToVersion) != "1.0.0" {
				t.Errorf("to %v, want the version that runs now", deref(record.ToVersion))
			}
			if staged := u.staged(); !slices.Equal(staged, []string{release.FileName(test.kind, "1.0.0", "amd64")}) {
				t.Errorf("staging holds %q", staged)
			}
		})
	}
}

func TestAgentUpdateSaysWhenTheRollbackFailsToo(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	u.unhealthy = []string{"1.1.0", "1.0.0"}
	wantAgentUpdate(t, u.start(t), ResultFailed, "the rollback to 1.0.0 failed too")
}

func TestAgentUpdateAfterAFailedInstallRollsBackOnlyWhenTheOldVersionDoesNotRun(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	u.host.fail["apt-get install -y -q -o DPkg::Lock::Timeout=300 /"] = errors.New("exit status 100")
	// The install changed nothing: 1.0.0 still runs.
	wantAgentUpdate(t, u.start(t), ResultFailed, "The install failed")
	if u.host.ran("apt-get install -y -q -o DPkg::Lock::Timeout=300 --allow-downgrades") {
		t.Errorf("rolled back: %q", u.host.commands)
	}

	u = newAgentUpdate(t, release.Deb)
	u.host.fail["apt-get install -y -q -o DPkg::Lock::Timeout=300 /"] = errors.New("exit status 100")
	calls := 0
	healthy := u.Healthy
	u.Healthy = func(ctx context.Context, version string) error {
		// The half-done install broke 1.0.0; the rollback repairs it.
		if calls++; calls == 1 {
			return errors.New("hostbeacon.service is failed")
		}
		return healthy(ctx, version)
	}
	wantAgentUpdate(t, u.start(t), ResultFailed, "went back to 1.0.0")
	if !u.host.ran("apt-get install -y -q -o DPkg::Lock::Timeout=300 --allow-downgrades") {
		t.Errorf("did not roll back: %q", u.host.commands)
	}
}

func TestAgentUpdateStopsWhenTheInstalledVersionCannotBeKept(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	delete(u.releases.files, release.VersionURL("1.0.0", "hostbeacon_1.0.0_amd64.deb"))

	wantAgentUpdate(t, u.start(t), ResultFailed, "Cannot keep the installed version 1.0.0")
	if u.installedAnything() {
		t.Errorf("installed: %q", u.host.commands)
	}
}

func TestAgentUpdateUsesTheKeptPackageOfTheInstalledVersion(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	writeFile(t, filepath.Join(u.Staging, "hostbeacon_1.0.0_amd64.deb"), "kept")
	u.start(t)
	if slices.Contains(u.releases.downloaded, release.VersionURL("1.0.0", release.SumsFile)) {
		t.Errorf("downloaded %q", u.releases.downloaded)
	}
}

func TestAgentUpdateOfADevelopmentBuildStops(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	u.Version = "0.0.0-dev"
	record := u.start(t)
	if record.Result == nil || *record.Result != ResultFailed || !strings.Contains(deref(record.Error), "not a release") {
		t.Fatalf("record %+v", record)
	}
	if u.installedAnything() {
		t.Errorf("installed: %q", u.host.commands)
	}
}

func TestAgentUpdateOfATarballSwitchesVersions(t *testing.T) {
	u := newAgentUpdate(t, release.Tarball)
	u.unhealthy = []string{"1.1.0"}

	wantAgentUpdate(t, u.start(t), ResultFailed, "went back to 1.0.0")
	unpacked := filepath.Join(u.Staging, "hostbeacon_1.1.0_linux_amd64", "hostbeacon")
	if got, err := os.ReadFile(unpacked); err == nil || !os.IsNotExist(err) {
		t.Errorf("the unpacked tarball stays: %q", got)
	}
	restart := "systemctl restart hostbeacon-helper.service hostbeacon.service"
	want := []string{unpacked + " install", restart, filepath.Join(u.TarballDir, "1.0.0", "hostbeacon") + " install", restart}
	if !slices.Equal(u.host.commands, want) {
		t.Errorf("commands\n%q\nwant\n%q", u.host.commands, want)
	}
}

func TestAgentUpdateOfATarballNeedsTheInstalledVersion(t *testing.T) {
	u := newAgentUpdate(t, release.Tarball)
	os.RemoveAll(filepath.Join(u.TarballDir, "1.0.0"))
	wantAgentUpdate(t, u.start(t), ResultFailed, "Cannot keep the installed version 1.0.0")
	if u.installedAnything() {
		t.Errorf("installed: %q", u.host.commands)
	}
}

func TestAgentUpdateIsBusyWhileAnotherPackageTaskRuns(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	holdPackageTaskLock(t, u.PackageTaskLock)
	if err := u.Start(context.Background(), agentUpdateEnabled); !errors.Is(err, ErrBusy) {
		t.Fatalf("Start = %v, want busy", err)
	}
	if u.ready != 0 || len(u.releases.downloaded) != 0 {
		t.Errorf("ready %d, downloaded %q", u.ready, u.releases.downloaded)
	}
}

func TestAgentUpdateFromHomeAssistantNeedsTheActionOnButTheOwnersDoesNot(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	if err := u.Start(context.Background(), config.Config{}); err == nil {
		t.Fatal("ran with Agent update off")
	}
	if len(u.releases.downloaded) != 0 {
		t.Errorf("downloaded %q", u.releases.downloaded)
	}

	u.request(t, "")
	if err := u.Start(context.Background(), config.Config{}); err != nil {
		t.Fatal(err)
	}
	record, _ := ReadAgentUpdate(u.Record)
	if record.ActionID != "" || deref(record.Result) != ResultOK {
		t.Errorf("record %+v", record)
	}
	// Only Home Assistant's requests are in the Action log.
	if lines := resultLines(t, u.logDir); len(lines) != 0 {
		t.Errorf("Action log results %v", lines)
	}
}

func TestAgentUpdateNotInTheActionLogDoesNotRun(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	u.request(t, secondID)
	if err := u.Start(context.Background(), agentUpdateEnabled); err == nil {
		t.Fatal("ran a request the helper did not log")
	}
	if len(u.releases.downloaded) != 0 {
		t.Errorf("downloaded %q", u.releases.downloaded)
	}
}

func TestEndAgentUpdateMarksAStoppedUpdateUnknown(t *testing.T) {
	u := newAgentUpdate(t, release.Deb)
	writeAgentUpdate(u.Record, &AgentUpdateRecord{ActionID: firstID, State: AgentUpdateRunning, FromVersion: "1.0.0", StartedAt: "2026-09-29T12:00:00Z"})

	if err := EndAgentUpdate(u.Record, u.Log, u.Now); err != nil {
		t.Fatal(err)
	}
	record, _ := ReadAgentUpdate(u.Record)
	if record.State != AgentUpdateResultUnknown || !record.Logged || record.FinishedAt == nil {
		t.Errorf("record %+v", record)
	}
	if lines := resultLines(t, u.logDir); len(lines) != 1 || lines[0]["result"] != "failed" {
		t.Errorf("Action log results %v", lines)
	}
}

func TestAgentUpdateOutcome(t *testing.T) {
	for _, test := range []struct {
		record AgentUpdateRecord
		want   *protocol.ActionOutcome
	}{
		{AgentUpdateRecord{State: AgentUpdateRunning}, nil},
		{AgentUpdateRecord{State: AgentUpdateFinished, Result: ptr(ResultOK)}, &protocol.ActionOutcome{Result: "ok"}},
		{AgentUpdateRecord{State: AgentUpdateFinished, Result: ptr(ResultFailed), Error: ptr("bad")}, &protocol.ActionOutcome{Result: "failed", Error: ptr("bad")}},
		{AgentUpdateRecord{State: AgentUpdateResultUnknown, Error: ptr("stopped")}, &protocol.ActionOutcome{Result: "failed", Error: ptr("stopped")}},
	} {
		got := test.record.Outcome()
		if (got == nil) != (test.want == nil) || got != nil && (got.Result != test.want.Result || deref(got.Error) != deref(test.want.Error)) {
			t.Errorf("Outcome(%+v) = %+v, want %+v", test.record, got, test.want)
		}
	}
}

func TestAgentUpdateActionResultIsForHomeAssistantsRequestsForAnHour(t *testing.T) {
	finished := AgentUpdateRecord{ActionID: firstID, State: AgentUpdateFinished, Result: ptr(ResultOK), FinishedAt: ptr("2026-09-29T12:00:00Z")}
	if got := finished.ActionResult(runNow.Add(59 * time.Minute)); got == nil || got.ActionID != firstID || got.Result != "ok" || got.Action != protocol.ActionAgentUpdate {
		t.Errorf("ActionResult = %+v", got)
	}
	if got := finished.ActionResult(runNow.Add(61 * time.Minute)); got != nil {
		t.Errorf("an hour later: %+v", got)
	}
	owner := finished
	owner.ActionID = ""
	running := AgentUpdateRecord{ActionID: firstID, State: AgentUpdateRunning}
	for _, record := range []AgentUpdateRecord{owner, running} {
		if got := record.ActionResult(runNow); got != nil {
			t.Errorf("ActionResult(%+v) = %+v", record, got)
		}
	}
}

func resultLines(t *testing.T, dir string) []map[string]any {
	t.Helper()
	return slices.DeleteFunc(logEntries(t, dir), func(entry map[string]any) bool { return entry["entry"] != "result" })
}

// --- The health check ---

func TestSystemdHealthWaitsUntilBothPartsRunTheVersionAndStayUp(t *testing.T) {
	host := &fakeHost{outputs: map[string]string{
		"/usr/bin/hostbeacon version": "hostbeacon 1.1.0\n",
		"systemctl show":              "ActiveState=active\nMainPID=10\n\nActiveState=active\nMainPID=20\n",
	}}
	health := SystemdHealth{Run: host.run, Program: "/usr/bin/hostbeacon", Start: time.Second, Stable: 20 * time.Millisecond, Poll: 5 * time.Millisecond}
	if err := health.Check(context.Background(), "1.1.0"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if err := health.Check(context.Background(), "1.2.0"); err == nil {
		t.Fatal("passed with another version installed")
	}

	// A part that restarts (crashes) within the stable time.
	pids := 0
	host.answer = func(line string) (string, error, bool) {
		if !strings.HasPrefix(line, "systemctl show") {
			return "", nil, false
		}
		pids++
		return fmt.Sprintf("ActiveState=active\nMainPID=10\n\nActiveState=active\nMainPID=%d\n", 20+pids), nil, true
	}
	if err := health.Check(context.Background(), "1.1.0"); err == nil {
		t.Fatal("passed while the network part restarts")
	}

	host.answer = func(line string) (string, error, bool) {
		if !strings.HasPrefix(line, "systemctl show") {
			return "", nil, false
		}
		return "ActiveState=active\nMainPID=10\n\nActiveState=activating\nMainPID=0\n", nil, true
	}
	if err := health.Check(context.Background(), "1.1.0"); err == nil {
		t.Fatal("passed while the network part does not run")
	}

	// A network part that runs but does not listen cannot serve Home Assistant.
	host.answer = nil
	health.Listening = func(context.Context) error { return errors.New("connection refused") }
	if err := health.Check(context.Background(), "1.1.0"); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("Check = %v, want it to fail while nothing listens", err)
	}
}
