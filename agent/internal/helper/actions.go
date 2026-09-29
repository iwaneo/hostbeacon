package helper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// DefaultPackageTaskLock is the Hostbeacon package-task lock (v1 spec §8),
// taken with flock by the Update run, the Agent update, and the package list
// refresh.
const DefaultPackageTaskLock = "/run/hostbeacon-helper/package-task.lock"

// DefaultPackageManagerLocks are the locks apt, dpkg, rpm, and dnf5 hold
// while they change the Host or refresh the package list. They lock with
// fcntl. A missing file is not held.
var DefaultPackageManagerLocks = []string{
	"/var/lib/dpkg/lock-frontend",
	"/var/lib/dpkg/lock",
	"/var/lib/apt/lists/lock",
	"/var/cache/apt/archives/lock",
	"/usr/lib/sysimage/rpm/.rpm.lock",
	"/var/lib/rpm/.rpm.lock",
	"/run/dnf/rpmtransaction.lock",
}

// DefaultPackageManagerPIDLocks are the locks of dnf 4 (AlmaLinux, Rocky,
// RHEL 9). dnf 4 writes its process ID into the file and deletes the file
// when it is done.
var DefaultPackageManagerPIDLocks = []string{
	"/var/cache/dnf/metadata_lock.pid",
	"/var/cache/dnf/download_lock.pid",
	"/var/lib/dnf/rpmdb_lock.pid",
}

// minUptime: no Reboot within 10 minutes of boot (v1 spec §9).
const minUptime = 10 * time.Minute

// updateRunStartTimeout limits how long the helper waits for the Update run
// unit to take the package-task lock. Home Assistant waits 30 seconds for
// the answer.
const updateRunStartTimeout = 25 * time.Second

const maxUser = 256

// ActionRequest is an Action request as the network part passes it on. The
// Pairing and the user are the network part's and Home Assistant's claims.
type ActionRequest struct {
	ActionID    string          `json:"action_id"`
	Action      protocol.Action `json:"action"`
	PairingID   string          `json:"pairing_id"`
	PairingName string          `json:"pairing_name"`
	User        *string         `json:"user"` // nil: no HA user
}

// Ack is the helper's answer to an Action request.
type Ack struct {
	Status      string                  `json:"status"` // accepted or refused
	Reason      *protocol.RefusalReason `json:"reason"`
	FirstResult *protocol.ActionOutcome `json:"first_result"`
}

// ActionRunner checks, logs, and runs Actions. It re-checks every guard
// itself, so a bug in the network part can only ask for what the owner
// enabled (v1 spec §4.1).
type ActionRunner struct {
	Log ActionLog
	// Journal gets every request as well; systemd keeps it in the journal.
	Journal             *slog.Logger
	Uptime              func() (time.Duration, error)
	PackageTaskLock     string
	PackageManagerLocks []string
	// PackageManagerPIDLocks are held while they name a running process.
	PackageManagerPIDLocks []string
	Reboot                 func(ctx context.Context) error
	// PackageManager is apt or dnf; "" on distros without full support,
	// where Update run is refused.
	PackageManager string
	// UpdateRuns starts the Update run unit. Without it, Update run is
	// refused.
	UpdateRuns UpdateRunStarter
	// UpdateRunRecord is the Update run record.
	UpdateRunRecord string

	// mu makes the Action ID check and the log write one step.
	mu sync.Mutex
}

// Request checks one Action request and writes it to the Action log. When it
// is accepted, run does the Action and logs its result. An error means the
// request itself is invalid; it is not logged in the Action log.
func (r *ActionRunner) Request(cfg config.Config, request ActionRequest) (ack Ack, run func(context.Context) protocol.ActionOutcome, err error) {
	if err := request.check(); err != nil {
		r.Journal.Warn("refused an invalid Action request", "error", err)
		return Ack{}, nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	reason, first, release := r.guard(cfg, request)
	entry := logEntry{
		Entry:     "request",
		ActionID:  request.ActionID,
		Action:    request.Action,
		PairingID: request.PairingID,
		Pairing:   request.PairingName,
		User:      ptr(noHAUser),
		Status:    "accepted",
		Reason:    reason,
	}
	if request.User != nil {
		// The user name is Home Assistant's claim. Any text is logged, as
		// JSON, and cut to a length.
		user := []rune(*request.User)
		entry.User = ptr(string(user[:min(len(user), maxUser)]))
	}
	if reason != "" {
		entry.Status = "refused"
	}
	if err := r.Log.write(entry); err != nil {
		r.Journal.Error("cannot write the Action log", "error", err)
		if release != nil {
			release()
		}
		entry.Status, entry.Reason, first, release = "refused", protocol.ReasonCannotLog, nil, nil
	} else {
		r.endUpdateRun()
	}
	r.Journal.Info("Action request", "action_id", entry.ActionID, "action", entry.Action, "pairing", entry.Pairing,
		"pairing_id", entry.PairingID, "user", *entry.User, "status", entry.Status, "reason", entry.Reason)
	if entry.Status == "refused" {
		return Ack{Status: "refused", Reason: &entry.Reason, FirstResult: first}, nil, nil
	}
	if request.Action == protocol.ActionUpdateRun {
		// The unit runs it now; its result comes through the run record.
		return Ack{Status: "accepted"}, nil, nil
	}
	return Ack{Status: "accepted"}, func(ctx context.Context) protocol.ActionOutcome {
		defer release()
		return r.reboot(ctx, request)
	}, nil
}

// guard returns why the request is refused, or "" when it may run. For an
// accepted Reboot, release frees the package-task lock it holds. An accepted
// Update run has already started: its unit holds the lock.
func (r *ActionRunner) guard(cfg config.Config, request ActionRequest) (reason protocol.RefusalReason, first *protocol.ActionOutcome, release func()) {
	found, first, err := r.Log.find(request.ActionID)
	if err != nil {
		r.Journal.Error("cannot read the Action log", "error", err)
		return protocol.ReasonCannotLog, nil, nil
	}
	if found {
		return protocol.ReasonDuplicate, first, nil
	}
	if !slices.Contains(cfg.EnabledActions, request.Action) {
		return protocol.ReasonDisabled, nil, nil
	}
	switch request.Action {
	case protocol.ActionReboot:
		reason, release = r.guardReboot()
		return reason, nil, release
	case protocol.ActionUpdateRun:
		return r.startUpdateRun(request), nil, nil
	}
	// Agent update comes in a later release of this helper.
	return protocol.ReasonDisabled, nil, nil
}

// startUpdateRun reserves the one run slot and starts the Update run unit
// (v1 spec §8). It returns "" once the unit holds the package-task lock.
func (r *ActionRunner) startUpdateRun(request ActionRequest) protocol.RefusalReason {
	if r.PackageManager == "" || r.UpdateRuns == nil {
		return protocol.ReasonDisabled
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateRunStartTimeout)
	defer cancel()
	// The helper handles one request at a time, so no other request can
	// take the slot between this check and the start.
	active, err := r.UpdateRuns.Active(ctx)
	if err != nil {
		r.Journal.Error("Update run refused: cannot read the Update run unit", "error", err)
		return protocol.ReasonBusy
	}
	if active {
		return protocol.ReasonUpdateRunRunning
	}
	// The unit takes the lock itself; this check spares starting it.
	release, err := lockPackageTask(r.PackageTaskLock)
	if err != nil {
		r.Journal.Info("Update run refused: a package task holds the package-task lock", "error", err)
		return protocol.ReasonBusy
	}
	release()
	if err := r.UpdateRuns.Start(ctx, request.ActionID); err != nil {
		r.Journal.Warn("Update run refused: the Update run unit did not take the package-task lock", "error", err)
		return protocol.ReasonBusy
	}
	return ""
}

// endUpdateRun writes the result of an ended Update run to the Action log
// when the unit could not (v1 spec §11: at the helper's next successful
// write), and marks a run that stopped without a result unknown.
func (r *ActionRunner) endUpdateRun() {
	if r.UpdateRunRecord == "" || r.UpdateRuns == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if active, err := r.UpdateRuns.Active(ctx); err != nil || active {
		return
	}
	if err := EndUpdateRun(r.UpdateRunRecord, r.Log); err != nil {
		r.Journal.Error("cannot complete the Update run record", "error", err)
	}
}

// ResetUpdateRun makes the Update run record "no run yet", for a Host that
// found it is a copy (v1 spec §4.4). A run that is going on keeps its record.
func (r *ActionRunner) ResetUpdateRun() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.UpdateRunRecord == "" || r.UpdateRuns == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if active, err := r.UpdateRuns.Active(ctx); err != nil || active {
		return err
	}
	if err := os.Remove(r.UpdateRunRecord); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (r *ActionRunner) guardReboot() (reason protocol.RefusalReason, release func()) {
	uptime, err := r.Uptime()
	if err != nil {
		r.Journal.Error("cannot read the uptime, so Reboot is refused", "error", err)
		return protocol.ReasonTooSoonAfterBoot, nil
	}
	if uptime < minUptime {
		return protocol.ReasonTooSoonAfterBoot, nil
	}
	release, err = lockPackageTask(r.PackageTaskLock)
	if err != nil {
		r.Journal.Info("Reboot refused: a package task holds the package-task lock", "error", err)
		return protocol.ReasonBusy, nil
	}
	if held, err := anyLockHeld(r.PackageManagerLocks); held || err != nil || anyPIDLockHeld(r.PackageManagerPIDLocks) {
		r.Journal.Info("Reboot refused: the package manager is busy", "lock", held, "error", err)
		release()
		return protocol.ReasonBusy, nil
	}
	return "", release
}

func (r *ActionRunner) reboot(ctx context.Context, request ActionRequest) protocol.ActionOutcome {
	outcome := protocol.ActionOutcome{Result: "ok"}
	if err := r.Reboot(ctx); err != nil {
		text := err.Error()
		outcome = protocol.ActionOutcome{Result: "failed", Error: &text}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := logEntry{Entry: "result", ActionID: request.ActionID, Action: request.Action, Result: outcome.Result, Error: outcome.Error}
	if err := r.Log.write(entry); err != nil {
		r.Journal.Error("cannot write an Action result to the Action log", "action_id", request.ActionID, "error", err)
	}
	r.Journal.Info("Action result", "action_id", request.ActionID, "action", request.Action, "result", outcome.Result, "error", outcome.Error)
	return outcome
}

func (request ActionRequest) check() error {
	switch {
	case !uuidPattern.MatchString(strings.ToLower(request.ActionID)):
		return errors.New("the Action ID is not a UUID")
	case !slices.Contains([]protocol.Action{protocol.ActionReboot, protocol.ActionUpdateRun, protocol.ActionAgentUpdate}, request.Action):
		return fmt.Errorf("unknown Action %q", request.Action)
	case !printable(request.PairingID, 64) || !printable(request.PairingName, 64):
		return errors.New("the Pairing is missing or not printable text")
	}
	return nil
}

func ptr[T any](value T) *T { return &value }

func printable(text string, maxRunes int) bool {
	runes := []rune(text)
	return len(runes) > 0 && len(runes) <= maxRunes && !slices.ContainsFunc(runes, func(r rune) bool { return !unicode.IsPrint(r) })
}

// lockPackageTask takes the package-task lock without waiting.
func lockPackageTask(path string) (release func(), err error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	// Closing the file frees the lock.
	return func() { file.Close() }, nil
}

// anyLockHeld says whether another process holds an fcntl lock on one of
// paths. It only asks; it never takes the lock.
func anyLockHeld(paths []string) (bool, error) {
	for _, path := range paths {
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		lock := syscall.Flock_t{Type: syscall.F_WRLCK}
		err = syscall.FcntlFlock(file.Fd(), syscall.F_GETLK, &lock)
		file.Close()
		if err != nil {
			return false, err
		}
		if lock.Type != syscall.F_UNLCK {
			return true, nil
		}
	}
	return false, nil
}

// anyPIDLockHeld says whether one of paths holds the process ID of a running
// process. A file left behind by a process that ended is not held.
func anyPIDLockHeld(paths []string) bool {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 {
			continue
		}
		// Signal 0 only checks that the process exists.
		if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
			return true
		}
	}
	return false
}

// ReadUptime reads how long the Host (or the container) has been up.
func ReadUptime(root string) (time.Duration, error) {
	data, err := os.ReadFile(filepath.Join(root, "proc", "uptime"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, errors.New("/proc/uptime is empty")
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
