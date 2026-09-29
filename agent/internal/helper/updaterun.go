package helper

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/command"
	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
	"github.com/iwaneo/hostbeacon/agent/internal/statefile"
)

// The Update run (v1 spec §8) runs in its own root systemd unit, so it goes
// on when the helper, the network part, or Home Assistant restarts.
const (
	UpdateRunUnit = "hostbeacon-update-run.service"
	// DefaultUpdateRunRecord is the Update run record. Root writes it; the
	// network part reads it and sends it in every snapshot.
	DefaultUpdateRunRecord = "/var/lib/hostbeacon-helper/update-run.json"
	// DefaultUpdateRunRequest passes the accepted Action ID to the unit.
	DefaultUpdateRunRequest = "/run/hostbeacon-helper/update-run-request.json"
	// DefaultUpdateRunDownloads holds the packages dnf downloads for a run.
	DefaultUpdateRunDownloads = "/var/cache/hostbeacon-helper/update-run"
)

// Run states and results in the record, as in the update_run group.
const (
	RunWaitingForLock = "waiting_for_lock"
	RunRunning        = "running"
	RunFinished       = "finished"
	RunResultUnknown  = "result_unknown"

	ResultOK                = "ok"
	ResultFailed            = "failed"
	ResultNeedsManualUpdate = "needs_manual_update"
)

const (
	updateRunFormat = 1
	maxRunError     = 300
	maxRecordNames  = 100
)

// ErrBusy: another package task holds the package-task lock.
var ErrBusy = errors.New("another package task holds the package-task lock")

// UpdateRunRecord is the record of the last Update run. Later releases may
// add fields; this one ignores them.
type UpdateRunRecord struct {
	Format     int      `json:"format"`
	ActionID   string   `json:"action_id"`
	RunID      string   `json:"run_id"`
	State      string   `json:"state"`
	Percent    *float64 `json:"percent"`
	StartedAt  string   `json:"started_at"`
	FinishedAt *string  `json:"finished_at"`
	Result     *string  `json:"result"`
	Installed  *int64   `json:"installed"`
	Remaining  *int64   `json:"remaining"`
	Error      *string  `json:"error"`
	// NeedsManualUpdate names the packages of the last run that stopped at
	// its checks. It stays until a later run passes them.
	NeedsManualUpdate []string `json:"needs_manual_update"`
	// Logged: the result is in the Action log.
	Logged bool `json:"logged"`
}

func (r *UpdateRunRecord) active() bool {
	return r.State == RunWaitingForLock || r.State == RunRunning
}

// ReadUpdateRun reads the record. It is nil when no run has been made yet.
func ReadUpdateRun(path string) (*UpdateRunRecord, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record UpdateRunRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if record.Format != updateRunFormat {
		return nil, fmt.Errorf("%s: format must be %d", path, updateRunFormat)
	}
	return &record, nil
}

func writeUpdateRun(path string, record *UpdateRunRecord) error {
	record.Format = updateRunFormat
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return statefile.Write(path, append(data, '\n'), 0o644)
}

// UpdateRunGroup is the update_run group for a record; idle without one.
func UpdateRunGroup(record *UpdateRunRecord) protocol.UpdateRun {
	if record == nil {
		return protocol.UpdateRun{State: "idle", NeedsManualUpdate: protocol.NameList{Names: []string{}}}
	}
	count := int64(len(record.NeedsManualUpdate))
	return protocol.UpdateRun{
		RunID:      &record.RunID,
		State:      record.State,
		Percent:    record.Percent,
		StartedAt:  &record.StartedAt,
		FinishedAt: record.FinishedAt,
		Result:     record.Result,
		Installed:  record.Installed,
		Remaining:  record.Remaining,
		Error:      record.Error,
		NeedsManualUpdate: protocol.NameList{
			Count: &count,
			Names: append([]string{}, record.NeedsManualUpdate[:min(len(record.NeedsManualUpdate), maxRecordNames)]...),
		},
	}
}

// UpdateRunStarter starts the Update run unit for the helper.
type UpdateRunStarter interface {
	// Active says whether the unit runs now: the run slot is taken.
	Active(ctx context.Context) (bool, error)
	// Start starts the unit for actionID and returns once the unit holds
	// the package-task lock.
	Start(ctx context.Context, actionID string) error
}

// SystemdUpdateRun starts the Update run unit with systemctl.
type SystemdUpdateRun struct {
	Run     command.Command
	Request string
}

// Active says whether the unit is starting, running, or stopping.
func (s SystemdUpdateRun) Active(ctx context.Context) (bool, error) {
	// is-active exits 3 for an inactive unit, and still prints its state.
	out, err := s.Run(ctx, "systemctl", "is-active", UpdateRunUnit)
	state := strings.TrimSpace(string(out))
	if state == "" {
		return false, withStderr(err)
	}
	return slices.Contains([]string{"active", "activating", "deactivating", "reloading", "refreshing"}, state), nil
}

// Start passes actionID to the unit and starts it. The unit tells systemd it
// is ready only once it holds the package-task lock, so systemctl returns
// then; it fails when the unit could not take the lock.
func (s SystemdUpdateRun) Start(ctx context.Context, actionID string) error {
	data, _ := json.Marshal(runRequest{ActionID: actionID})
	if err := os.MkdirAll(filepath.Dir(s.Request), 0o755); err != nil {
		return err
	}
	if err := statefile.Write(s.Request, data, 0o600); err != nil {
		return err
	}
	_, err := s.Run(ctx, "systemctl", "start", UpdateRunUnit)
	return withStderr(err)
}

type runRequest struct {
	ActionID string `json:"action_id"`
}

// UpdateRun is the Update run unit's work (v1 spec §8).
type UpdateRun struct {
	Manager string // apt or dnf
	// Run runs the refresh, the simulation, and the checks; Stream runs the
	// install and reads its progress.
	Run       command.Command
	Stream    command.Stream
	Root      string
	Record    string
	Request   string
	Stamp     string
	Downloads string
	// The package-task lock and the package manager's locks, as for the
	// package list refresh.
	PackageTaskLock        string
	PackageManagerLocks    []string
	PackageManagerPIDLocks []string
	Log                    ActionLog
	// Ready tells systemd that the unit holds the package-task lock.
	Ready func() error
	Now   func() time.Time
	// LockWait is how long to wait for the package manager's lock (5 minutes);
	// LogWait, for the helper to log the accepted request. Poll is how often
	// both are checked.
	LockWait, LogWait, Poll time.Duration
}

// Start takes the package-task lock, tells systemd it is ready, and runs the
// Update run the helper accepted. It returns ErrBusy when another package
// task holds the lock, and an error when nothing ran. A run that ran has its
// result in the record and the Action log.
func (u *UpdateRun) Start(ctx context.Context, cfg config.Config) error {
	data, err := os.ReadFile(u.Request)
	if err != nil {
		return fmt.Errorf("no Update run request: %w", err)
	}
	// A request starts one run only.
	os.Remove(u.Request)
	var request runRequest
	if err := json.Unmarshal(data, &request); err != nil || !uuidPattern.MatchString(request.ActionID) {
		return errors.New("the Update run request is not valid")
	}
	// The unit re-checks the Host config itself (fail closed).
	if !slices.Contains(cfg.EnabledActions, protocol.ActionUpdateRun) {
		return errors.New("Update run is turned off in the Host config")
	}
	if u.Manager != "apt" && u.Manager != "dnf" {
		return errors.New("Update run is not supported on this distro")
	}
	if err := os.MkdirAll(filepath.Dir(u.PackageTaskLock), 0o755); err != nil {
		return err
	}
	release, err := lockPackageTask(u.PackageTaskLock)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrBusy
	}
	if err != nil {
		return fmt.Errorf("cannot take the package-task lock: %w", err)
	}
	defer release()
	if err := u.Ready(); err != nil {
		return err
	}
	// The helper logs the request as accepted after the unit is ready. An
	// Action that cannot be logged does not run.
	if !u.accepted(ctx, request.ActionID) {
		return errors.New("the Update run request is not accepted in the Action log, so nothing runs")
	}
	record := &UpdateRunRecord{
		ActionID:          request.ActionID,
		RunID:             newRunID(),
		State:             RunWaitingForLock,
		StartedAt:         u.Now().UTC().Format(time.RFC3339),
		NeedsManualUpdate: []string{},
	}
	// "Needs manual update" stays until a run passes its checks (v1 spec
	// §7.7), also when this run fails before them.
	if previous, err := ReadUpdateRun(u.Record); err == nil && previous != nil {
		record.NeedsManualUpdate = previous.NeedsManualUpdate
	}
	if err := writeUpdateRun(u.Record, record); err != nil {
		return err
	}
	u.run(ctx, record)
	return nil
}

func (u *UpdateRun) run(ctx context.Context, record *UpdateRunRecord) {
	if !u.waitForPackageManager(ctx) {
		u.finish(record, ResultFailed, "The package manager was busy for 5 minutes, so nothing was installed.", nil)
		return
	}
	record.State = RunRunning
	u.save(record)
	var manager packageManager = aptRun{u}
	if u.Manager == "dnf" {
		manager = dnfRun{u}
		defer os.RemoveAll(u.Downloads)
	}

	// 1. Refresh the package list.
	if err := manager.refresh(ctx); err != nil {
		u.finish(record, ResultFailed, "Cannot refresh the package list: "+err.Error(), nil)
		return
	}
	if err := writeStamp(u.Stamp, u.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "cannot store the package list refresh time:", err)
	}
	// 2. Simulate the whole run.
	plan, err := manager.plan(ctx)
	if err != nil {
		u.finish(record, ResultFailed, "Cannot simulate the Update run: "+err.Error(), nil)
		return
	}
	// 3. Stop on a distro release change or any removal.
	changes, err := manager.releaseChanges(ctx, plan)
	if err != nil {
		u.finish(record, ResultFailed, "Cannot check the distro release: "+err.Error(), nil)
		return
	}
	if changes {
		u.finish(record, ResultNeedsManualUpdate, "The Update run could move the Host to another distro release, so nothing was installed.", names(plan.Installs))
		return
	}
	if len(plan.Removes) > 0 {
		u.finish(record, ResultNeedsManualUpdate, "The Update run would remove packages, so nothing was installed.", plan.Removes)
		return
	}
	record.NeedsManualUpdate = []string{}
	// 4. Install exactly the approved transaction.
	installed := int64(len(plan.Installs))
	var installErr error
	if installed > 0 {
		installErr = manager.install(ctx, plan, func(percent float64) {
			if record.Percent == nil || *record.Percent != percent {
				record.Percent = &percent
				u.save(record)
			}
		})
	}
	// 5. Re-read what is left. The network part re-reads Available updates
	// and Reboot required when it sees the run finish.
	if remaining, err := manager.remaining(ctx); err == nil {
		record.Remaining = &remaining
	}
	if installErr != nil {
		u.finish(record, ResultFailed, "The install failed: "+installErr.Error(), nil)
		return
	}
	record.Installed = &installed
	u.finish(record, ResultOK, "", nil)
}

// accepted waits until the Action log holds actionID as an accepted request.
func (u *UpdateRun) accepted(ctx context.Context, actionID string) bool {
	return u.waitFor(ctx, u.LogWait, func() bool {
		found, first, err := u.Log.find(actionID)
		return err == nil && found && first == nil
	})
}

// waitForPackageManager waits until no package manager holds its lock.
func (u *UpdateRun) waitForPackageManager(ctx context.Context) bool {
	return u.waitFor(ctx, u.LockWait, func() bool {
		held, err := anyLockHeld(u.PackageManagerLocks)
		return err == nil && !held && !anyPIDLockHeld(u.PackageManagerPIDLocks)
	})
}

func (u *UpdateRun) waitFor(ctx context.Context, limit time.Duration, done func() bool) bool {
	deadline := time.After(limit)
	for {
		if done() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case <-time.After(u.Poll):
		}
	}
}

// finish stores the result in the record and the Action log. A run that
// needs a manual update names its packages; any other keeps the names the
// record has.
func (u *UpdateRun) finish(record *UpdateRunRecord, result, message string, needsManualUpdate []string) {
	record.State = RunFinished
	record.FinishedAt = ptr(u.Now().UTC().Format(time.RFC3339))
	record.Result = &result
	record.Percent = nil
	if result == ResultNeedsManualUpdate {
		record.NeedsManualUpdate = append([]string{}, needsManualUpdate...)
	}
	if message != "" {
		record.Error = ptr(shorten(message))
	}
	u.save(record)
	if logResult(u.Log, record) == nil {
		record.Logged = true
		u.save(record)
	}
}

// save writes the record. When it cannot, the run goes on: the unit's end
// marks the result unknown.
func (u *UpdateRun) save(record *UpdateRunRecord) {
	if err := writeUpdateRun(u.Record, record); err != nil {
		fmt.Fprintln(os.Stderr, "cannot write the Update run record:", err)
	}
}

// EndUpdateRun completes the record after the unit ended. A run that
// stopped without a result (the Host restarted, or the unit was killed) is
// result_unknown, and a result not yet in the Action log is written there
// (v1 spec §11). Call it only when the unit is not running.
func EndUpdateRun(path string, log ActionLog) error {
	record, err := ReadUpdateRun(path)
	if err != nil || record == nil {
		return err
	}
	if record.active() {
		record.State = RunResultUnknown
		record.Percent = nil
		record.Error = ptr("The Update run stopped before it reported a result.")
		if err := writeUpdateRun(path, record); err != nil {
			return err
		}
	}
	if record.Logged {
		return nil
	}
	if err := logResult(log, record); err != nil {
		return err
	}
	record.Logged = true
	return writeUpdateRun(path, record)
}

// logResult writes the run's result to the Action log.
func logResult(log ActionLog, record *UpdateRunRecord) error {
	entry := logEntry{Entry: "result", ActionID: record.ActionID, Action: protocol.ActionUpdateRun, Result: "failed", Error: record.Error}
	switch {
	case record.State == RunResultUnknown:
		entry.Error = ptr("result unknown: " + deref(record.Error))
	case record.Result != nil && *record.Result == ResultOK:
		entry.Result = "ok"
	case record.Result != nil && *record.Result == ResultNeedsManualUpdate:
		entry.Error = ptr("needs manual update: " + deref(record.Error))
	}
	return log.write(entry)
}

func deref(text *string) string {
	if text == nil {
		return ""
	}
	return *text
}

// shorten keeps an error short: the record is sent to Home Assistant.
func shorten(text string) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= maxRunError {
		return string(runes)
	}
	return string(runes[:maxRunError-1]) + "…"
}

func names(packages []planned) []string {
	list := make([]string, 0, len(packages))
	for _, item := range packages {
		list = append(list, item.Name)
	}
	return list
}

func newRunID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// packageManager does the steps that differ between apt and dnf.
type packageManager interface {
	refresh(ctx context.Context) error
	plan(ctx context.Context) (transaction, error)
	releaseChanges(ctx context.Context, plan transaction) (bool, error)
	install(ctx context.Context, plan transaction, progress func(percent float64)) error
	remaining(ctx context.Context) (int64, error)
}

type aptRun struct{ *UpdateRun }

func (a aptRun) refresh(ctx context.Context) error {
	_, err := a.Run(ctx, "apt-get", "update", "-q", "-o", "APT::Update::Error-Mode=any")
	return withStderr(err)
}

func (a aptRun) plan(ctx context.Context) (transaction, error) {
	out, err := a.Run(ctx, "apt-get", "-s", "-q", "dist-upgrade")
	return parseAptSimulation(string(out)), withStderr(err)
}

func (a aptRun) releaseChanges(ctx context.Context, plan transaction) (bool, error) {
	data, err := os.ReadFile(filepath.Join(a.Root, "etc", "os-release"))
	if err != nil {
		return false, err
	}
	return aptReleaseChanges(filepath.Join(a.Root, "var", "lib", "apt", "lists"), parseOSRelease(string(data))["VERSION_CODENAME"])
}

// install names every approved package with its exact version. With
// --trivial-only, apt refuses to install anything more than those, and
// --no-remove refuses any removal: a changed plan stops the run.
func (a aptRun) install(ctx context.Context, plan transaction, progress func(float64)) error {
	auto, err := a.Run(ctx, "apt-mark", "showauto")
	if err != nil {
		return fmt.Errorf("cannot read which packages are automatically installed: %w", withStderr(err))
	}
	args := append([]string{
		"install", "-q", "--trivial-only", "--no-remove",
		"-o", "APT::Status-Fd=1",
		// Keep the Host's current config files.
		"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold",
	}, aptInstallArgs(plan.Installs)...)
	if err := a.Stream(ctx, func(line string) {
		if percent, ok := aptProgress(line); ok {
			progress(percent)
		}
	}, "apt-get", args...); err != nil {
		return withStderr(err)
	}
	if marks := aptAutoMarks(plan.Installs, string(auto)); len(marks) > 0 {
		// The packages are installed; a failed mark only keeps them from autoremove.
		if _, err := a.Run(ctx, "apt-mark", append([]string{"auto"}, marks...)...); err != nil {
			fmt.Fprintln(os.Stderr, "cannot mark packages as automatically installed:", withStderr(err))
		}
	}
	return nil
}

// aptProgress reads a status line like "pmstatus:tzdata:43.4783:Installing tzdata".
func aptProgress(line string) (float64, bool) {
	fields := strings.SplitN(line, ":", 4)
	if len(fields) < 3 || fields[0] != "pmstatus" {
		return 0, false
	}
	percent, err := strconv.ParseFloat(fields[2], 64)
	if err != nil || percent < 0 || percent > 100 {
		return 0, false
	}
	return math.Round(percent), true
}

func (a aptRun) remaining(ctx context.Context) (int64, error) {
	out, err := a.Run(ctx, "apt", "list", "--upgradable", "-o", "APT::Cmd::Disable-Script-Warning=true")
	return countAptUpgradable(string(out)), err
}

type dnfRun struct{ *UpdateRun }

func (d dnfRun) refresh(ctx context.Context) error {
	_, err := d.Run(ctx, "dnf", "makecache", "--refresh", "-q")
	return withStderr(err)
}

// plan downloads the packages of the whole run. The downloaded files are
// the approved transaction: the install uses only them.
func (d dnfRun) plan(ctx context.Context) (transaction, error) {
	if err := os.RemoveAll(d.Downloads); err != nil {
		return transaction{}, err
	}
	if err := os.MkdirAll(d.Downloads, 0o700); err != nil {
		return transaction{}, err
	}
	if _, err := d.Run(ctx, "dnf", "upgrade", "-q", "-y", "--downloadonly", "--destdir="+d.Downloads); err != nil {
		return transaction{}, withStderr(err)
	}
	files, err := filepath.Glob(filepath.Join(d.Downloads, "*.rpm"))
	if err != nil || len(files) == 0 {
		return transaction{}, err
	}
	installs, err := d.headers(ctx, files)
	return transaction{Installs: installs, Files: files}, err
}

func (d dnfRun) headers(ctx context.Context, files []string) ([]planned, error) {
	out, err := d.Run(ctx, "rpm", append([]string{"-qp", "--queryformat", rpmHeaderFormat}, files...)...)
	return parseRPMHeaders(string(out)), withStderr(err)
}

// releaseChanges finds the packages that own /etc/os-release. rpm exits 1
// when no package owns it; that counts as a change (fail closed).
func (d dnfRun) releaseChanges(ctx context.Context, plan transaction) (bool, error) {
	path, err := filepath.EvalSymlinks(filepath.Join(d.Root, "etc", "os-release"))
	if err != nil {
		return false, err
	}
	var owners []planned
	if out, err := d.Run(ctx, "rpm", "-qf", "--queryformat", "%{NAME} %{VERSION}\n", path); err == nil {
		for line := range strings.Lines(string(out)) {
			if fields := strings.Fields(line); len(fields) == 2 {
				owners = append(owners, planned{Name: fields[0], Version: fields[1]})
			}
		}
	}
	return dnfReleaseChanges(owners, plan.Installs), nil
}

// install first checks that the downloaded files are still exactly the
// approved packages and signed with a key rpm trusts, then installs only
// those files, with every repository off. dnf removes nothing but the
// packages they obsolete.
func (d dnfRun) install(ctx context.Context, plan transaction, _ func(float64)) error {
	if now, err := d.headers(ctx, plan.Files); err != nil || !slices.Equal(now, plan.Installs) {
		return errors.New("the packages changed after they were checked")
	}
	out, err := d.Run(ctx, "rpm", append([]string{"-K"}, plan.Files...)...)
	if err != nil {
		return fmt.Errorf("a package is not signed with a key rpm trusts: %w", withStderr(err))
	}
	for line := range strings.Lines(string(out)) {
		if !strings.HasSuffix(strings.TrimSpace(line), "signatures OK") {
			return errors.New("a package is not signed: " + strings.TrimSpace(line))
		}
	}
	args := append([]string{"install", "-y", "-q", "--disablerepo=*"}, plan.Files...)
	return withStderr(d.Stream(ctx, func(string) {}, "dnf", args...))
}

func (d dnfRun) remaining(ctx context.Context) (int64, error) {
	out, err := d.Run(ctx, "dnf", "--cacheonly", "-q", "repoquery", "--upgrades", "--latest-limit=1", "--queryformat", "%{name} %{arch}\n")
	var count int64
	for line := range strings.Lines(string(out)) {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, err
}
